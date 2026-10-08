package secrets

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/yoho-dev/yoho/internal/config"
)

// ParseRefs decodes x-yoho.secrets from a generic decoded value: map form
// {CONTAINER_NAME: SECRET_KEY} or Kamal-style list form
// ["KEY", "CONTAINER_NAME:SECRET_KEY"]. Map entries are sorted by name.
func ParseRefs(raw any) (config.SecretRefs, error) {
	var refs config.SecretRefs
	switch v := raw.(type) {
	case nil:
		return nil, nil
	case map[string]string:
		for name, key := range v {
			refs = append(refs, config.SecretRef{Name: name, Key: key})
		}
		sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	case map[string]any:
		for name, key := range v {
			k, ok := key.(string)
			if !ok {
				return nil, fmt.Errorf("secrets: value for %q must be a string key", name)
			}
			refs = append(refs, config.SecretRef{Name: name, Key: k})
		}
		sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	case []string:
		for _, s := range v {
			refs = append(refs, listRef(s))
		}
	case []any:
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("secrets: list entries must be strings, got %T", item)
			}
			refs = append(refs, listRef(s))
		}
	default:
		return nil, fmt.Errorf("secrets: want a map or list, got %T", raw)
	}
	seen := map[string]bool{}
	for _, r := range refs {
		if !validName(r.Name) || !validName(r.Key) {
			return nil, fmt.Errorf("secrets: invalid entry %s:%s (names must match [A-Za-z_][A-Za-z0-9_]*)", r.Name, r.Key)
		}
		if seen[r.Name] {
			return nil, fmt.Errorf("secrets: %s is declared twice", r.Name)
		}
		seen[r.Name] = true
	}
	return refs, nil
}

func listRef(s string) config.SecretRef {
	if name, key, ok := strings.Cut(s, ":"); ok {
		return config.SecretRef{Name: name, Key: key}
	}
	return config.SecretRef{Name: s, Key: s}
}

// ForService returns container name -> value for the secrets a Service
// declares. Only declared secrets are returned (ADR 0006).
func (s *Store) ForService(refs config.SecretRefs) (map[string]string, error) {
	out := make(map[string]string, len(refs))
	var missing []string
	for _, r := range refs {
		v, ok := s.values[r.Key]
		if !ok {
			missing = append(missing, r.Key)
			continue
		}
		out[r.Name] = v
	}
	if len(missing) > 0 {
		return nil, missingErr(missing)
	}
	return out, nil
}

func missingErr(keys []string) error {
	sort.Strings(keys)
	keys = compactStrings(keys)
	return fmt.Errorf("missing secrets: %s (define them in .yoho/secrets or secrets.values)", strings.Join(keys, ", "))
}

func compactStrings(s []string) []string {
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// BuildxArgs returns `--secret id=KEY,env=KEY` arguments for buildx and the
// KEY=value entries to add to the docker process environment, so values
// never appear in argv.
func (s *Store) BuildxArgs(keys []string) (args []string, env []string, err error) {
	var missing []string
	for _, k := range keys {
		if !validName(k) {
			return nil, nil, fmt.Errorf("build secret %q is not a valid environment variable name", k)
		}
		v, ok := s.values[k]
		if !ok {
			missing = append(missing, k)
			continue
		}
		args = append(args, "--secret", "id="+k+",env="+k)
		env = append(env, k+"="+v)
	}
	if len(missing) > 0 {
		return nil, nil, missingErr(missing)
	}
	return args, env, nil
}

// FindInterpolatedSecrets returns the secret keys referenced as $KEY or
// ${KEY...} in raw compose text. Interpolated secrets leak through
// `docker compose config`, so `yoho config check` rejects them. "$$" is
// compose's escape for a literal "$" and is skipped.
func FindInterpolatedSecrets(composeYAML []byte, keys []string) []string {
	want := map[string]bool{}
	for _, k := range keys {
		want[k] = true
	}
	found := map[string]bool{}
	src := composeYAML
	for i := 0; i < len(src); i++ {
		if src[i] != '$' || i+1 >= len(src) {
			continue
		}
		j := i + 1
		switch {
		case src[j] == '$':
			i = j
			continue
		case src[j] == '{':
			j++
		}
		start := j
		for j < len(src) && isNameChar(src[j]) {
			j++
		}
		if j > start && isNameStart(src[start]) && want[string(src[start:j])] {
			found[string(src[start:j])] = true
		}
		i = j - 1
	}
	out := make([]string, 0, len(found))
	for k := range found {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Entry describes a secret for `yoho secrets print` without its value.
type Entry struct {
	Key string
	Ref string
	// Length in bytes of the value.
	Length int
	// Fingerprint (see Fingerprint); empty when no key was given.
	Fingerprint string
}

// Describe lists the Store's secrets, sorted by key. fingerprintKey is the
// per-Destination HMAC key; nil omits fingerprints.
func Describe(s *Store, fingerprintKey []byte) []Entry {
	var out []Entry
	for _, k := range s.Keys() {
		e := Entry{Key: k, Ref: s.refs[k], Length: len(s.values[k])}
		if fingerprintKey != nil {
			e.Fingerprint = Fingerprint(fingerprintKey, s.values[k])
		}
		out = append(out, e)
	}
	return out
}

// ErrNotTTY is returned by Reveal when output is not an interactive terminal.
var ErrNotTTY = errors.New("refusing to reveal secret values: output is not a terminal")

// Reveal writes KEY=value lines (dotenv-quoted when needed) for keys, or for
// all keys when empty. It refuses unless isTTY, so values don't end up in
// pipes, files, or CI logs.
func Reveal(w io.Writer, s *Store, keys []string, isTTY bool) error {
	if !isTTY {
		return ErrNotTTY
	}
	if len(keys) == 0 {
		keys = s.Keys()
	}
	var missing []string
	for _, k := range keys {
		if _, ok := s.values[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return missingErr(missing)
	}
	for _, k := range keys {
		if _, err := fmt.Fprintf(w, "%s=%s\n", k, quoteValue(s.values[k])); err != nil {
			return err
		}
	}
	return nil
}

// quoteValue renders v so that parseDotenv reads typical values back unchanged.
func quoteValue(v string) string {
	if v != "" && !strings.ContainsAny(v, " \t\r\n#'\"$\\") {
		return v
	}
	if !strings.ContainsAny(v, "'\r\n") {
		return "'" + v + "'"
	}
	q := strconv.Quote(v) // escapes \n, ", \
	return strings.ReplaceAll(q, "$", `\$`)
}
