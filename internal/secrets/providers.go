package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yoho-build/yoho/internal/config"
)

// providerResolver resolves one reference through a password-manager CLI.
// Errors never include stdout, which would be the secret.
type providerResolver struct {
	run    Runner
	getenv func(string) (string, bool)
	// clock times calls for classification. Default time.Now.
	clock func() time.Time
	// retries is how many times an ApprovalTimeout is retried.
	retries   int
	onWait    func(WaitEvent)
	waitAfter time.Duration
	warn      func(string)
}

func (r *providerResolver) resolve(ctx context.Context, key string, p config.SecretProvider, ref string) (string, error) {
	switch p.Type {
	case "op":
		return r.op(ctx, key, p, ref)
	case "bw":
		return r.bw(ctx, key, ref)
	case "bws":
		return r.bws(ctx, key, p, ref)
	case "command":
		if len(p.Command) == 0 {
			return "", errors.New("type command needs a command argv")
		}
		argv := append(append([]string{}, p.Command...), ref)
		out, err := r.call(ctx, "command", key, argv)
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(out), "\r\n"), nil
	}
	return "", fmt.Errorf("unknown provider type %q (want op, bw, bws, command)", p.Type)
}

// call runs one provider command. Failures are classified here, where stderr
// is still in memory; the returned *ProviderError never carries it (Other
// keeps the redacted command error). An ApprovalTimeout is retried up to
// r.retries times.
func (r *providerResolver) call(ctx context.Context, provider, key string, argv []string) ([]byte, error) {
	clock := r.clock
	if clock == nil {
		clock = time.Now
	}
	for attempt := 0; ; attempt++ {
		stop := watchWait(r.onWait, r.waitAfter, WaitEvent{Provider: provider, Key: key})
		start := clock()
		var entry *memoCall
		var out, stderr []byte
		var err error
		if m, ok := r.run.(*memoRunner); ok {
			out, stderr, err, entry = m.run(ctx, argv, nil)
		} else {
			out, stderr, err = r.run.Run(ctx, argv, nil)
		}
		stop()
		if err == nil {
			return out, nil
		}
		pe := classifiedError(ctx, provider, argv[0], err, out, stderr, clock().Sub(start))
		if pe.Class == ApprovalTimeout && attempt < r.retries && ctx.Err() == nil {
			if m, ok := r.run.(*memoRunner); ok {
				m.forget(argv, entry)
			}
			if r.warn != nil {
				r.warn("secrets provider " + provider + " approval timed out; retrying")
			}
			continue
		}
		return nil, pe
	}
}

// op read prints the field verbatim with --no-newline, preserving values
// that legitimately end in a newline (PEM keys).
func (r *providerResolver) op(ctx context.Context, key string, p config.SecretProvider, ref string) (string, error) {
	argv := []string{"op", "read", "--no-newline"}
	if p.Account != "" {
		argv = append(argv, "--account", p.Account)
	}
	out, err := r.call(ctx, "op", key, append(argv, ref))
	return string(out), err
}

// bw refs: "<id-or-name>" reads the login password; "<id-or-name>/<field>"
// reads a custom field (or username, password, totp, notes) from the item
// JSON. Item names containing "/" must use the id. The vault must already be
// unlocked (BW_SESSION).
func (r *providerResolver) bw(ctx context.Context, key, ref string) (string, error) {
	i := strings.LastIndexByte(ref, '/')
	if i < 0 {
		out, err := r.call(ctx, "bw", key, []string{"bw", "get", "password", ref})
		return strings.TrimRight(string(out), "\r\n"), err
	}
	item, field := ref[:i], ref[i+1:]
	out, err := r.call(ctx, "bw", key, []string{"bw", "get", "item", item})
	if err != nil {
		return "", err
	}
	var it struct {
		Login struct {
			Username *string `json:"username"`
			Password *string `json:"password"`
			Totp     *string `json:"totp"`
		} `json:"login"`
		Notes  *string `json:"notes"`
		Fields []struct {
			Name  string  `json:"name"`
			Value *string `json:"value"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(out, &it); err != nil {
		return "", errors.New("bw get item returned invalid JSON")
	}
	for _, f := range it.Fields {
		if f.Name == field && f.Value != nil {
			return *f.Value, nil
		}
	}
	var v *string
	switch field {
	case "username":
		v = it.Login.Username
	case "password":
		v = it.Login.Password
	case "totp":
		v = it.Login.Totp
	case "notes":
		v = it.Notes
	}
	if v == nil {
		return "", fmt.Errorf("item %q has no field %q", item, field)
	}
	return *v, nil
}

// bws authenticates with BWS_ACCESS_TOKEN from the environment; Account maps
// to --profile.
func (r *providerResolver) bws(ctx context.Context, key string, p config.SecretProvider, ref string) (string, error) {
	if v, ok := r.getenv("BWS_ACCESS_TOKEN"); !ok || v == "" {
		return "", &ProviderError{Class: NotSignedIn, Provider: "bws", note: "BWS_ACCESS_TOKEN is not set"}
	}
	argv := []string{"bws", "secret", "get", ref, "-o", "json"}
	if p.Account != "" {
		argv = append(argv, "--profile", p.Account)
	}
	out, err := r.call(ctx, "bws", key, argv)
	if err != nil {
		return "", err
	}
	var s struct {
		Value *string `json:"value"`
	}
	if err := json.Unmarshal(out, &s); err != nil || s.Value == nil {
		return "", errors.New("bws returned JSON without a value")
	}
	return *s.Value, nil
}
