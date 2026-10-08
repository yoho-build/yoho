package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/invopop/jsonschema"
)

// UnmarshalJSON accepts the map form {NAME: KEY} and the Kamal-style list
// form ["KEY", "NAME:KEY"]. A bare "KEY" means NAME == KEY.
func (r *SecretRefs) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("null")) {
		*r = nil
		return nil
	}
	var out SecretRefs
	switch {
	case len(data) > 0 && data[0] == '{':
		var m map[string]string
		if err := json.Unmarshal(data, &m); err != nil {
			return fmt.Errorf("secrets map form must be {NAME: KEY} with string values: %w", err)
		}
		names := make([]string, 0, len(m))
		for n := range m {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			if n == "" || m[n] == "" {
				return fmt.Errorf("secrets: empty name or key in %q", n)
			}
			out = append(out, SecretRef{Name: n, Key: m[n]})
		}
	case len(data) > 0 && data[0] == '[':
		var l []string
		if err := json.Unmarshal(data, &l); err != nil {
			return fmt.Errorf("secrets list form must be strings (\"KEY\" or \"NAME:KEY\"): %w", err)
		}
		for _, s := range l {
			name, key, found := strings.Cut(s, ":")
			if !found {
				name, key = s, s
			}
			if name == "" || key == "" {
				return fmt.Errorf("secrets: invalid reference %q", s)
			}
			out = append(out, SecretRef{Name: name, Key: key})
		}
	default:
		return fmt.Errorf("secrets must be a map {NAME: KEY} or a list [\"KEY\", \"NAME:KEY\"]")
	}
	*r = out
	return nil
}

// MarshalJSON writes the map form, sorted by NAME for determinism.
func (r SecretRefs) MarshalJSON() ([]byte, error) {
	m := make(map[string]string, len(r))
	for _, s := range r {
		m[s.Name] = s.Key
	}
	return json.Marshal(m) // encoding/json sorts map keys
}

// Keys returns the distinct secret keys referenced, sorted.
func (r SecretRefs) Keys() []string {
	seen := map[string]bool{}
	var keys []string
	for _, s := range r {
		if !seen[s.Key] {
			seen[s.Key] = true
			keys = append(keys, s.Key)
		}
	}
	sort.Strings(keys)
	return keys
}

// JSONSchema describes both accepted forms.
func (SecretRefs) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Description: "Secrets this Service receives: {NAME: KEY} or [\"KEY\", \"NAME:KEY\"]",
		OneOf: []*jsonschema.Schema{
			{Type: "object", AdditionalProperties: &jsonschema.Schema{Type: "string"}},
			{Type: "array", Items: &jsonschema.Schema{Type: "string"}},
		},
	}
}
