package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/templates"
	"github.com/yoho-build/yoho/internal/ui"
)

func initIn(t *testing.T, dir string, names ...string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	err := runInit(ui.New(&buf, ui.JSON, false), dir, names)
	return buf.String(), err
}

// Each template, alone and all together, passes `config check --offline`
// with no errors and no warnings.
func TestInitTemplatesPassConfigCheck(t *testing.T) {
	sets := [][]string{}
	for _, n := range templates.Names() {
		sets = append(sets, []string{n})
	}
	sets = append(sets, templates.Names())
	for _, set := range sets {
		t.Run(strings.Join(set, "+"), func(t *testing.T) {
			dir := t.TempDir()
			out, err := initIn(t, dir, set...)
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			for _, n := range set {
				if !strings.Contains(out, `"event":"template_added"`) || !strings.Contains(out, `"template":"`+n+`"`) {
					t.Fatalf("no template_added event for %s:\n%s", n, out)
				}
			}
			check, err := runCheck(t, filepath.Join(dir, "yoho.yml"), "--offline")
			if err != nil {
				t.Fatalf("config check: %v\n%s", err, check)
			}
			ev := checkEvent(t, check)
			if ev["ok"] != true || ev["warnings"].(float64) != 0 || int(ev["services"].(float64)) != len(set) {
				t.Fatalf("check event %#v\n%s", ev, check)
			}
		})
	}
}

func TestInitTemplateMergesAndRefusesConflicts(t *testing.T) {
	dir := t.TempDir()
	compose := "services:\n  web:\n    image: nginx:1.27-alpine\n    restart: unless-stopped\n"
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(compose), 0o640); err != nil {
		t.Fatal(err)
	}
	if out, err := initIn(t, dir, "redis"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "compose.yaml"))
	if !strings.HasPrefix(string(b), compose) || !strings.Contains(string(b), "\n  redis:\n") {
		t.Fatalf("merge:\n%s", b)
	}
	if st, _ := os.Stat(filepath.Join(dir, "compose.yaml")); st.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v", st.Mode())
	}
	// A second run keeps yoho.yml and adds another template.
	yoho, _ := os.ReadFile(filepath.Join(dir, "yoho.yml"))
	out, err := initIn(t, dir, "postgres")
	if err != nil || !strings.Contains(out, "yoho.yml exists, left unchanged") {
		t.Fatalf("%v\n%s", err, out)
	}
	if again, _ := os.ReadFile(filepath.Join(dir, "yoho.yml")); string(again) != string(yoho) {
		t.Fatal("yoho.yml changed")
	}
	before, _ := os.ReadFile(filepath.Join(dir, "compose.yaml"))
	out, err = initIn(t, dir, "rustfs", "redis")
	if err == nil || !strings.Contains(out, "already has service redis") || !strings.Contains(out, `"hint"`) {
		t.Fatalf("conflict: %v\n%s", err, out)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "compose.yaml")); string(after) != string(before) {
		t.Fatal("refused merge changed compose.yaml")
	}
	check, err := runCheck(t, filepath.Join(dir, "yoho.yml"), "--offline")
	if err != nil || checkEvent(t, check)["ok"] != true {
		t.Fatalf("config check: %v\n%s", err, check)
	}
	// Without --template an existing Yoho file is still an error.
	if _, err := initIn(t, dir); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("plain init: %v", err)
	}
}

// A name declared in a Destination overlay or another compose entry would
// merge into the template's Service: refused before anything is written.
func TestInitTemplateRefusesNamesInOtherComposeFiles(t *testing.T) {
	write := func(dir, rel, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, file, body, yoho, want string
	}{
		{"destination overlay", "compose.production.yaml", "services:\n  postgres:\n    environment: {X: y}\n", "", `Service \"postgres\" is already declared in compose.production.yaml`},
		{"second compose entry", "db.yaml", "services:\n  postgres:\n    image: postgres:16\n", "compose: [compose.yaml, db.yaml]\n", `Service \"postgres\" is already declared in db.yaml`},
		{"volume in another entry", "db.yaml", "services:\n  other:\n    image: busybox\nvolumes:\n  pgdata: {}\n", "compose: [compose.yaml, db.yaml]\n", `volume \"pgdata\" is already declared in db.yaml`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if out, err := initIn(t, dir, "redis"); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if tc.yoho != "" {
				y, _ := os.ReadFile(filepath.Join(dir, "yoho.yml"))
				write(dir, "yoho.yml", tc.yoho+string(y))
			}
			write(dir, tc.file, tc.body)
			before, _ := os.ReadFile(filepath.Join(dir, "compose.yaml"))
			out, err := initIn(t, dir, "postgres")
			if err == nil || !strings.Contains(out, tc.want) || !strings.Contains(out, `"hint"`) {
				t.Fatalf("collision not refused: %v\n%s", err, out)
			}
			if after, _ := os.ReadFile(filepath.Join(dir, "compose.yaml")); string(after) != string(before) {
				t.Fatal("refused template changed compose.yaml")
			}
		})
	}
}

func TestInitListTemplatesJSON(t *testing.T) {
	c := initCmd(&globals{json: true})
	var buf bytes.Buffer
	c.SetOut(&buf)
	c.SetErr(&buf)
	c.SetArgs([]string{"--list-templates"})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		if e["event"] == "template" {
			names = append(names, e["name"].(string))
		}
	}
	if strings.Join(names, ",") != "postgres,redis,clickhouse,rustfs" {
		t.Fatalf("templates %v\n%s", names, buf.String())
	}
	c = initCmd(&globals{json: true})
	buf.Reset()
	c.SetOut(&buf)
	c.SetErr(&buf)
	c.SilenceErrors, c.SilenceUsage = true, true
	c.SetArgs([]string{"--template", "mysql"})
	if err := c.Execute(); err == nil || !strings.Contains(buf.String(), "unknown template") {
		t.Fatalf("unknown template: %v\n%s", err, buf.String())
	}
}
