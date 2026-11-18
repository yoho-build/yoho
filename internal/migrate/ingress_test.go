package migrate_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/migrate"
)

// pathRules has two rules for app.example.com: one scoped to /ws and the
// default one, plus another hostname and the catch-all.
const pathRules = `{"ingress":[` +
	`{"hostname":"app.example.com","path":"/ws","service":"http://sync:8080"},` +
	`{"hostname":"app.example.com","service":"http://web:3000","originRequest":{"noTLSVerify":true,"connectTimeout":"10s"}},` +
	`{"hostname":"other.example.com","service":"http://other:80","x-unknown":[1,2]},` +
	`{"service":"http_status:404"}],"warp-routing":{"enabled":false},"originRequest":{"http2Origin":true}}`

func decode(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestRewriteRuleChangesOnlyTheSelectedRule(t *testing.T) {
	out, old, err := migrate.RewriteRule(json.RawMessage(pathRules), "APP.example.com", "", "http://yoho-proxy:80")
	if err != nil {
		t.Fatal(err)
	}
	if old != (migrate.IngressRule{Hostname: "app.example.com", Service: "http://web:3000"}) {
		t.Errorf("old %+v", old)
	}
	want := decode(t, json.RawMessage(pathRules))
	want["ingress"].([]any)[1].(map[string]any)["service"] = "http://yoho-proxy:80"
	if got := decode(t, out); !reflect.DeepEqual(got, want) {
		t.Errorf("got  %s\nwant %v", out, want)
	}

	out, old, err = migrate.RewriteRule(json.RawMessage(pathRules), "app.example.com", "/ws", "http://yoho-proxy:80")
	if err != nil {
		t.Fatal(err)
	}
	rules, _ := migrate.Rules(out)
	if old.Service != "http://sync:8080" || rules[0].Service != "http://yoho-proxy:80" || rules[1].Service != "http://web:3000" || rules[3].Service != "http_status:404" || len(rules) != 4 {
		t.Errorf("old %+v rules %+v", old, rules)
	}
}

func TestRewriteRuleRoundTrip(t *testing.T) {
	out, old, err := migrate.RewriteRule(json.RawMessage(pathRules), "app.example.com", "", "http://yoho-proxy:80")
	if err != nil {
		t.Fatal(err)
	}
	back, _, err := migrate.RewriteRule(out, "app.example.com", "", old.Service)
	if err != nil {
		t.Fatal(err)
	}
	if migrate.Hash(back) != migrate.Hash(json.RawMessage(pathRules)) {
		t.Errorf("round trip changed the configuration:\n%s", back)
	}
}

func TestRewriteRuleErrors(t *testing.T) {
	onlyPaths := `{"ingress":[{"hostname":"api.example.com","path":"/v1","service":"http://a:1"},{"hostname":"api.example.com","path":"/v2","service":"http://b:1"},{"service":"http_status:404"}]}`
	dup := `{"ingress":[{"hostname":"app.example.com","service":"http://a:1"},{"hostname":"App.example.com","service":"http://b:1"},{"service":"http_status:404"}]}`
	for _, tc := range []struct {
		name, cfg, host, path string
		want                  []string
	}{
		{"only path rules", onlyPaths, "api.example.com", "", []string{"/v1, /v2", "--path"}},
		{"wrong path", pathRules, "app.example.com", "/api", []string{"/ws, (no path)", "--path"}},
		{"no rule", pathRules, "new.example.com", "", []string{"no ingress rule for new.example.com", "public hostname"}},
		{"duplicates", dup, "app.example.com", "", []string{"2 ingress rules", "first"}},
		{"not an object", `[1]`, "app.example.com", "", []string{"not a JSON object"}},
	} {
		_, _, err := migrate.RewriteRule(json.RawMessage(tc.cfg), tc.host, tc.path, "http://yoho-proxy:80")
		if err == nil {
			t.Errorf("%s: no error", tc.name)
			continue
		}
		for _, w := range tc.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: %q does not mention %q", tc.name, err, w)
			}
		}
	}
}

func TestFindRuleReturnsRawRule(t *testing.T) {
	r, raw, err := migrate.FindRule(json.RawMessage(pathRules), "app.example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Service != "http://web:3000" {
		t.Errorf("rule %+v", r)
	}
	got := decode(t, raw)
	if or, _ := got["originRequest"].(map[string]any); or["connectTimeout"] != "10s" || got["service"] != "http://web:3000" {
		t.Errorf("raw %s", raw)
	}
}
