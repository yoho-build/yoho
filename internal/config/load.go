package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/tailscale/hujson"
	"go.yaml.in/yaml/v3"
)

// FileNames are the Yoho file names Find looks for, in order.
var FileNames = []string{"yoho.yml", "yoho.yaml", "yoho.toml", "yoho.json", "yoho.jsonc"}

// Find returns the Yoho file in dir. Several candidates is an error rather
// than a silent pick, because the wrong file would deploy the wrong thing.
func Find(dir string) (string, error) {
	var found []string
	for _, n := range FileNames {
		p := filepath.Join(dir, n)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			found = append(found, p)
		}
	}
	switch len(found) {
	case 0:
		return "", fmt.Errorf("no Yoho file in %s (looked for %s)", dir, strings.Join(FileNames, ", "))
	case 1:
		return found[0], nil
	}
	return "", fmt.Errorf("several Yoho files in %s: %s; keep one", dir, strings.Join(found, ", "))
}

// Load reads and parses a Yoho file. The format is detected from content.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data, path)
}

// Parse decodes a Yoho file in any supported format and applies defaults.
// nameHint (usually the path) only decorates error messages.
func Parse(data []byte, nameHint string) (*Config, error) {
	generic, err := decodeGeneric(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", nameHint, err)
	}
	var c Config
	if err := decodeStrict(generic, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", nameHint, err)
	}
	c.applyDefaults()
	return &c, nil
}

// DecodeServiceExt decodes a raw x-yoho value (as found in compose
// extensions) strictly into ServiceExt.
func DecodeServiceExt(raw any) (ServiceExt, error) {
	var e ServiceExt
	if raw == nil {
		return e, nil
	}
	err := decodeStrict(raw, &e)
	return e, err
}

func (c *Config) applyDefaults() {
	if c.Builder.Location == "" {
		c.Builder.Location = "local"
	}
	if c.Transport.Mode == "" {
		c.Transport.Mode = "auto"
	}
	if c.RetainReleases == 0 {
		c.RetainReleases = 5
	}
	if c.Setup.User == "" {
		c.Setup.User = "yoho"
	}
	if c.Hooks.Path == "" {
		c.Hooks.Path = ".yoho/hooks"
	}
	for n, d := range c.Destinations {
		if d.Runtime == "" {
			d.Runtime = "compose"
			c.Destinations[n] = d
		}
	}
}

var (
	reTOMLSection = regexp.MustCompile(`(?m)^\s*\[{1,2}[A-Za-z0-9_."'\- ]+\]{1,2}\s*(#.*)?$`)
	reTOMLKV      = regexp.MustCompile(`(?m)^\s*[A-Za-z0-9_."'\-]+\s*=\s*\S`)
	reYAMLKV      = regexp.MustCompile(`(?m)^\s*(- )?[A-Za-z0-9_."'\-$]+\s*:(\s|$)`)
)

// decodeGeneric detects the format by content, not file extension.
func decodeGeneric(data []byte) (any, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, errors.New("empty Yoho file")
	}
	if looksLikeJSON(trimmed) {
		std, err := hujson.Standardize(data)
		if err != nil {
			return nil, fmt.Errorf("invalid JSON: %w", err)
		}
		var v any
		if err := json.Unmarshal(std, &v); err != nil {
			return nil, fmt.Errorf("invalid JSON: %w", err)
		}
		return v, nil
	}
	if (reTOMLSection.Match(data) || reTOMLKV.Match(data)) && !reYAMLKV.Match(data) {
		var v map[string]any
		terr := toml.Unmarshal(data, &v)
		if terr == nil {
			return v, nil
		}
		// Heuristic can misfire on exotic YAML; fall back before giving up.
		var y any
		if yaml.Unmarshal(data, &y) == nil {
			if _, ok := y.(map[string]any); ok {
				return y, nil
			}
		}
		return nil, fmt.Errorf("invalid TOML: %w", terr)
	}
	var v any
	if err := yaml.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("invalid YAML: %w", err)
	}
	if _, ok := v.(map[string]any); !ok {
		return nil, errors.New("top level must be a mapping")
	}
	return v, nil
}

// looksLikeJSON: first non-space, non-comment character is '{'.
func looksLikeJSON(b []byte) bool {
	for len(b) > 0 {
		switch {
		case b[0] == '{':
			return true
		case bytes.HasPrefix(b, []byte("//")):
			i := bytes.IndexByte(b, '\n')
			if i < 0 {
				return false
			}
			b = bytes.TrimSpace(b[i:])
		case bytes.HasPrefix(b, []byte("/*")):
			i := bytes.Index(b, []byte("*/"))
			if i < 0 {
				return false
			}
			b = bytes.TrimSpace(b[i+2:])
		default:
			return false
		}
	}
	return false
}

// decodeStrict maps a generic value onto out through JSON. Unknown keys are
// reported with their full path (encoding/json alone drops the path).
func decodeStrict(generic any, out any) error {
	if err := checkKeys(generic, reflect.TypeOf(out).Elem(), ""); err != nil {
		return err
	}
	b, err := json.Marshal(normalize(generic))
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		var ute *json.UnmarshalTypeError
		if errors.As(err, &ute) && ute.Field != "" {
			return fmt.Errorf("%s: expected %s, got %s", ute.Field, ute.Type, ute.Value)
		}
		return err
	}
	return nil
}

// normalize converts map[any]any (from YAML with odd keys) to map[string]any.
func normalize(v any) any {
	switch t := v.(type) {
	case map[any]any:
		m := make(map[string]any, len(t))
		for k, x := range t {
			m[fmt.Sprint(k)] = normalize(x)
		}
		return m
	case map[string]any:
		for k, x := range t {
			t[k] = normalize(x)
		}
	case []any:
		for i, x := range t {
			t[i] = normalize(x)
		}
	}
	return v
}

var unmarshalerType = reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()

func jsonFields(t reflect.Type) map[string]reflect.Type {
	f := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		name := strings.Split(sf.Tag.Get("json"), ",")[0]
		if name == "-" {
			continue
		}
		if name == "" {
			name = sf.Name
		}
		f[name] = sf.Type
	}
	return f
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func checkKeys(v any, t reflect.Type, path string) error {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if reflect.PointerTo(t).Implements(unmarshalerType) {
		return nil
	}
	switch t.Kind() {
	case reflect.Struct:
		m, ok := v.(map[string]any)
		if !ok {
			if m2, ok2 := v.(map[any]any); ok2 {
				m, ok = normalize(m2).(map[string]any), true
			}
		}
		if !ok {
			return nil // type error reported by json decode
		}
		fields := jsonFields(t)
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			ft, known := fields[k]
			if !known {
				return fmt.Errorf("unknown key %q", join(path, k))
			}
			if err := checkKeys(m[k], ft, join(path, k)); err != nil {
				return err
			}
		}
	case reflect.Map:
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if err := checkKeys(m[k], t.Elem(), join(path, k)); err != nil {
				return err
			}
		}
	case reflect.Slice:
		l, ok := v.([]any)
		if !ok {
			return nil
		}
		for i, x := range l {
			if err := checkKeys(x, t.Elem(), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	}
	return nil
}
