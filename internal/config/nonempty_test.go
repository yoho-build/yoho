package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestNonemptyFormatsAndSchema(t *testing.T) {
	sources := map[string]string{
		"yaml":  strings.Replace(yamlSrc, "secrets:\n", "secrets:\n  nonempty: [DB_PASSWORD, RESTIC_PW]\n", 1),
		"toml":  strings.Replace(tomlSrc, "[secrets.providers.op]", "[secrets]\nnonempty = [\"DB_PASSWORD\", \"RESTIC_PW\"]\n\n[secrets.providers.op]", 1),
		"json":  strings.Replace(jsonSrc, `"secrets": {`, `"secrets": {"nonempty": ["DB_PASSWORD", "RESTIC_PW"],`, 1),
		"jsonc": strings.Replace(jsoncSrc, `"secrets": {`, `"secrets": {"nonempty": ["DB_PASSWORD", "RESTIC_PW",],`, 1),
	}
	for name, src := range sources {
		t.Run(name, func(t *testing.T) {
			c, err := Parse([]byte(src), name)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(c.Secrets.Nonempty, []string{"DB_PASSWORD", "RESTIC_PW"}) {
				t.Fatalf("declaration: %v", c.Secrets.Nonempty)
			}
			if errs := Validate(c); len(errs) != 0 {
				t.Fatal(errs)
			}
		})
	}
	b, err := Schema()
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	def := s["$defs"].(map[string]any)["SecretsConfig"].(map[string]any)
	p := def["properties"].(map[string]any)["nonempty"].(map[string]any)
	if p["type"] != "array" || p["uniqueItems"] != true || p["items"].(map[string]any)["pattern"] != secretKeyPattern.String() {
		t.Fatalf("schema: %v", p)
	}
	required, _ := def["required"].([]any)
	for _, key := range required {
		if key == "nonempty" {
			t.Fatal("nonempty must be optional")
		}
	}
}

func TestNonemptyDeclarationValidation(t *testing.T) {
	for _, key := range []string{"", " KEY", "KEY ", "1KEY", "KEY-ONE", "ALIAS:SOURCE", "KEY\nVALUE", "钥匙"} {
		t.Run(key, func(t *testing.T) {
			c, err := Parse([]byte(yamlSrc), "test")
			if err != nil {
				t.Fatal(err)
			}
			c.Secrets.Nonempty = []string{key}
			errs := Validate(c)
			if len(errs) != 1 || !strings.Contains(errs[0].Error(), "secrets.nonempty[0]") {
				t.Fatalf("errors: %v", errs)
			}
			if key != "" && strings.Contains(errs[0].Error(), key) {
				t.Fatalf("echoed invalid entry: %v", errs)
			}
		})
	}
	if errs := ValidateNonempty([]string{"KEY", "KEY"}); len(errs) != 1 || !strings.Contains(errs[0].Error(), "duplicate source key KEY") {
		t.Fatalf("duplicates: %v", errs)
	}
	if errs := ValidateNonempty([]string{"_", "key_2", "UPPER"}); len(errs) != 0 {
		t.Fatal(errs)
	}
	for _, src := range []string{`{"secrets":{"nonempty":"KEY"}}`, `{"secrets":{"nonempty":[12]}}`, `{"secrets":{"nonempty":{"KEY":true}}}`} {
		if _, err := Parse([]byte(src), "test"); err == nil {
			t.Fatalf("accepted wrong type: %s", src)
		}
	}
}
