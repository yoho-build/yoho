package secrets

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
)

// 1Password authorizes per process when op has no terminal session (coding
// agents, scripts, CI on a desktop), so one `op read` per secret can mean one
// approval prompt per secret. Load therefore reads every op reference of an
// account in a single `op inject` call and only falls back to `op read` per
// reference when that call fails for a reason other than sign-in or approval,
// so the failing key can still be named.

// opRef is one op reference in one account ("" is op's default account).
type opRef struct {
	account, ref string
}

// opWant is a reference to prefetch and the secret that first asked for it,
// for wait events and errors.
type opWant struct {
	opRef
	key  string
	fail func(error) error
}

// opCache holds values read by opPrefetch. A nil cache is empty.
type opCache map[opRef]string

func (c opCache) get(r opRef) (string, bool) {
	v, ok := c[r]
	return v, ok
}

// batchable reports whether ref can sit in an inject template unchanged:
// braces, quotes and newlines would change the template, and "$" is expanded
// differently by op inject than by a shell or op read.
func batchable(ref string) bool {
	return strings.HasPrefix(ref, "op://") && !strings.ContainsAny(ref, "{}\"$\\\r\n")
}

// opPrefetch reads wants with one op inject per account that has at least
// two distinct references. A sign-in or approval failure is returned (via the
// first want's fail) so the caller stops instead of prompting again per
// reference; any other failure leaves those references to op read.
func (r *providerResolver) opPrefetch(ctx context.Context, wants []opWant) (opCache, error) {
	if _, ok := r.run.(inputRunner); !ok {
		return nil, nil
	}
	var accounts []string
	groups := map[string][]opWant{}
	seen := map[opRef]bool{}
	for _, w := range wants {
		if seen[w.opRef] || !batchable(w.ref) {
			continue
		}
		seen[w.opRef] = true
		if _, ok := groups[w.account]; !ok {
			accounts = append(accounts, w.account)
		}
		groups[w.account] = append(groups[w.account], w)
	}
	cache := opCache{}
	for _, account := range accounts {
		g := groups[account]
		if len(g) < 2 {
			continue
		}
		vals, err := r.opInject(ctx, account, g)
		if err != nil {
			var pe *ProviderError
			if errors.As(err, &pe) && (pe.Class == ApprovalTimeout || pe.Class == NotSignedIn) {
				return nil, g[0].fail(err)
			}
			if ctx.Err() != nil {
				return nil, g[0].fail(err)
			}
			if r.warn != nil {
				r.warn("secrets provider op: batch read failed; reading each reference separately")
			}
			continue
		}
		for i, w := range g {
			cache[w.opRef] = vals[i]
		}
	}
	return cache, nil
}

// opInject resolves refs through one op inject. Each value sits between
// marker lines carrying a random nonce, so the output splits back into values
// verbatim (trailing newlines included, as with op read --no-newline).
func (r *providerResolver) opInject(ctx context.Context, account string, g []opWant) ([]string, error) {
	var nb [16]byte
	if _, err := rand.Read(nb[:]); err != nil {
		return nil, err
	}
	nonce := "yoho-" + hex.EncodeToString(nb[:])
	marker := func(i int) string { return nonce + ":" + strconv.Itoa(i) }
	end := nonce + ":end"
	var tpl strings.Builder
	for i, w := range g {
		tpl.WriteString(marker(i) + "\n{{ " + w.ref + " }}\n")
	}
	tpl.WriteString(end)

	argv := []string{"op", "inject"}
	if account != "" {
		argv = append(argv, "--account", account)
	}
	out, err := r.callInput(ctx, "op", g[0].key, argv, []byte(tpl.String()))
	if err != nil {
		return nil, err
	}
	rest, ok := bytes.CutPrefix(out, []byte(marker(0)+"\n"))
	if !ok {
		return nil, errors.New("op inject returned unexpected output")
	}
	vals := make([]string, len(g))
	for i := range g {
		next := "\n" + end
		if i+1 < len(g) {
			next = "\n" + marker(i+1) + "\n"
		}
		v, after, ok := bytes.Cut(rest, []byte(next))
		if !ok {
			return nil, errors.New("op inject returned unexpected output")
		}
		vals[i], rest = string(v), after
	}
	return vals, nil
}

// parseOpRead recognises a $(...) command that is exactly an op read of one
// reference, so its value can come from the batch: `op read [--no-newline|-n]
// [--account X] REF`, words plain or quoted, nothing else. The result is the
// same either way because $(...) trims trailing newlines.
func parseOpRead(cmd string) (opRef, bool) {
	words, ok := shellWords(cmd)
	if !ok || len(words) < 3 || words[0] != "op" || words[1] != "read" {
		return opRef{}, false
	}
	var r opRef
	args := words[2:]
	for len(args) > 1 {
		switch a := args[0]; {
		case a == "--no-newline" || a == "-n":
			args = args[1:]
		case a == "--account" && len(args) > 2:
			r.account, args = args[1], args[2:]
		case strings.HasPrefix(a, "--account="):
			r.account, args = strings.TrimPrefix(a, "--account="), args[1:]
		default:
			return opRef{}, false
		}
	}
	if len(args) != 1 || strings.HasPrefix(r.account, "-") {
		return opRef{}, false
	}
	r.ref = args[0]
	return r, batchable(r.ref)
}

// shellWords splits plain, 'single' and "double" quoted words. Anything the
// shell would treat specially (expansion, escapes, operators) is refused.
func shellWords(s string) ([]string, bool) {
	var words []string
	var cur strings.Builder
	in := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ' || c == '\t':
			if in {
				words, in = append(words, cur.String()), false
				cur.Reset()
			}
		case c == '\'' || c == '"':
			j := strings.IndexByte(s[i+1:], c)
			if j < 0 {
				return nil, false
			}
			q := s[i+1 : i+1+j]
			if c == '"' && strings.ContainsAny(q, "$`\\") {
				return nil, false
			}
			cur.WriteString(q)
			in, i = true, i+1+j
		case strings.IndexByte("$`\\;&|<>()*?[]{}~#!\n\r", c) >= 0:
			return nil, false
		default:
			cur.WriteByte(c)
			in = true
		}
	}
	if in {
		words = append(words, cur.String())
	}
	return words, true
}
