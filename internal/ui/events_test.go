package ui

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"testing"
	"time"
)

// validateSchema implements the small subset used by EventsSchema, including
// conditional requirements. Keeping it data-driven catches schema drift.
func validateSchema(t *testing.T, s map[string]any, v any) {
	t.Helper()
	if typ, ok := s["type"].(string); ok {
		valid := false
		switch typ {
		case "object":
			_, valid = v.(map[string]any)
		case "string":
			_, valid = v.(string)
		case "array":
			_, valid = v.([]any)
		case "integer":
			n, ok := v.(float64)
			valid = ok && n == float64(int64(n))
		}
		if !valid {
			t.Fatalf("want %s got %#v", typ, v)
		}
	}
	if c, ok := s["const"]; ok && c != v {
		t.Fatalf("const %v got %v", c, v)
	}
	if enums, ok := s["enum"].([]any); ok {
		found := false
		for _, e := range enums {
			if e == v {
				found = true
			}
		}
		if !found {
			t.Fatalf("invalid enum %v", v)
		}
	}
	if minimum, ok := s["minimum"].(float64); ok && v.(float64) < minimum {
		t.Fatalf("below minimum: %v", v)
	}
	if minimum, ok := s["minLength"].(float64); ok && len(v.(string)) < int(minimum) {
		t.Fatalf("short string: %v", v)
	}
	if pattern, ok := s["pattern"].(string); ok && !regexp.MustCompile(pattern).MatchString(v.(string)) {
		t.Fatalf("invalid pattern: %v", v)
	}
	if s["format"] == "date-time" {
		if _, err := time.Parse(time.RFC3339Nano, v.(string)); err != nil {
			t.Fatal(err)
		}
	}
	if xs, ok := v.([]any); ok {
		if item, ok := s["items"].(map[string]any); ok {
			for _, x := range xs {
				validateSchema(t, item, x)
			}
		}
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return
	}
	if req, ok := s["required"].([]any); ok {
		for _, k := range req {
			if _, ok := obj[k.(string)]; !ok {
				t.Fatalf("missing %s", k)
			}
		}
	}
	if props, ok := s["properties"].(map[string]any); ok {
		for k, rule := range props {
			if x, ok := obj[k]; ok {
				validateSchema(t, rule.(map[string]any), x)
			}
		}
	}
	if all, ok := s["allOf"].([]any); ok {
		for _, entry := range all {
			rule := entry.(map[string]any)
			cond := rule["if"].(map[string]any)
			matches := true
			for k, check := range cond["properties"].(map[string]any) {
				if obj[k] != check.(map[string]any)["const"] {
					matches = false
				}
			}
			if matches {
				validateSchema(t, rule["then"].(map[string]any), v)
			}
		}
	}
}
func TestEventSchemaGoldenAndEmitted(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal(EventsSchema(), &schema); err != nil {
		t.Fatal(err)
	}
	golden, err := os.ReadFile("testdata/outcomes.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	u := New(&buf, JSON, false)
	u.Operation("abc123")
	u.Finished(errors.New("failed"), "")
	for _, stream := range [][]byte{golden, buf.Bytes()} {
		scan := bufio.NewScanner(bytes.NewReader(stream))
		for scan.Scan() {
			var event any
			if err = json.Unmarshal(scan.Bytes(), &event); err != nil {
				t.Fatal(err)
			}
			validateSchema(t, schema, event)
		}
		if err = scan.Err(); err != nil {
			t.Fatal(err)
		}
	}
}
func TestDeferredFinishAndRedaction(t *testing.T) {
	var b bytes.Buffer
	u := New(&b, JSON, false)
	u.SetRedact(func(s string) string {
		return string(bytes.ReplaceAll([]byte(s), []byte("secret-value"), []byte("[REDACTED]")))
	})
	u.HoldFinish()
	u.Finished(nil, "done")
	if b.Len() != 0 {
		t.Fatal("finished too early")
	}
	u.Complete(errors.New("secret-value"))
	if bytes.Contains(b.Bytes(), []byte("secret-value")) {
		t.Fatal("secret leaked")
	}
	u.Finished(nil, "")
	if bytes.Count(b.Bytes(), []byte(`"event":"finished"`)) != 1 {
		t.Fatal(b.String())
	}
}
