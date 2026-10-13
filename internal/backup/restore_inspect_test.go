package backup

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"text/template"
)

func renderRestoreInspect(t *testing.T, format string, root any) string {
	t.Helper()
	// Container/image inspect falls back to decoded JSON and missingkey=error.
	tpl, err := template.New("inspect").Option("missingkey=error").Funcs(template.FuncMap{
		"json": func(v any) string { b, _ := json.Marshal(v); return string(b) },
	}).Parse(format)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := tpl.Execute(&out, root); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestRestoreInspectOptionalJSONFields(t *testing.T) {
	for _, tc := range []struct {
		name          string
		health        any
		omit, enabled bool
	}{
		{"omitted", nil, true, false},
		{"null", nil, false, false},
		{"timing-only", map[string]any{"Interval": 1000}, false, false},
		{"empty-test", map[string]any{"Test": []string{}}, false, false},
		{"disabled", map[string]any{"Test": []string{"NONE"}}, false, false},
		{"enabled", map[string]any{"Test": []string{"CMD", "private-probe-secret"}}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := map[string]any{"Labels": map[string]string{
				"yoho.app": "shop", "yoho.destination": "production", "com.docker.compose.project": "yoho-shop-production", "com.docker.compose.service": "db",
			}}
			if !tc.omit {
				cfg["Healthcheck"] = tc.health
			}
			root := map[string]any{
				"Id": "cid123", "Config": cfg, "State": map[string]any{"Status": "running"},
				"HostConfig": map[string]any{"NetworkMode": "default"}, "NetworkSettings": map[string]any{},
			}
			out := renderRestoreInspect(t, restoreContainerFormat, root)
			var got restoreObject
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatal(err)
			}
			if got.HasHealthcheck != tc.enabled || got.ID != "cid123" || got.Service != "db" || len(got.Ports) != 0 || len(got.Networks) != 0 || strings.Contains(out, "private-probe-secret") {
				t.Fatalf("incorrect container metadata: %s", out)
			}
			if health := renderRestoreInspect(t, restoreImageHealthFormat, root); (health == "true") != tc.enabled || (health != "true" && health != "false") {
				t.Fatalf("incorrect image health metadata: %s", health)
			}
		})
	}
}
