package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/release"
)

func TestCheckGeneratedFlags(t *testing.T) {
	got, err := checkGeneratedFlags([]string{"POSTGRES_PASSWORD", "MYSQL_PASSWORD", "POSTGRES_PASSWORD"}, nil, "compose")
	if err != nil || strings.Join(got, ",") != "MYSQL_PASSWORD,POSTGRES_PASSWORD" {
		t.Fatalf("%v %v", got, err)
	}
	if got, err := checkGeneratedFlags(nil, []string{"web"}, "compose"); err != nil || got != nil {
		t.Fatalf("no flag: %v %v", got, err)
	}
	for _, c := range []struct {
		keys, services []string
		runtime, want  string
	}{
		{[]string{"POSTGRES_PASSWORD"}, []string{"web"}, "compose", "cannot be combined with Service names"},
		{[]string{"POSTGRES_PASSWORD"}, nil, "swarm", "compose runtime only"},
		{[]string{"BAD-KEY"}, nil, "compose", "is not a secret key"},
		{[]string{""}, nil, "compose", "is not a secret key"},
	} {
		_, err := checkGeneratedFlags(c.keys, c.services, c.runtime)
		var he *hintError
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%v: %v", c, err)
		}
		if !errors.As(err, &he) || he.hint == "" {
			t.Fatalf("%v: no hint", c)
		}
	}
}

func TestSecretsRotateGeneratedWithServiceFailsBeforeSSH(t *testing.T) {
	oldRoot := release.Root
	t.Cleanup(func() { release.Root = oldRoot })
	src := "services:\n  web:\n    image: nginx\n  db:\n    image: postgres\n"
	c := secretsCmd(&globals{configPath: appDir(t, "compose", src)})
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"rotate", "--generated", "POSTGRES_PASSWORD", "web"})
	c.SilenceErrors, c.SilenceUsage = true, true
	err := c.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(out.String(), "cannot be combined") || strings.Contains(out.String(), "Connect") {
		t.Fatalf("error=%v output=%s", err, out.String())
	}
}

func TestMigrateDraftRotationChecklist(t *testing.T) {
	from := filepath.Join("..", "..", "examples", "quickstart", "railway", "inventory.json")
	out, err := runDraft(t, "draft", "--from", from)
	if err != nil {
		t.Fatal(err, out)
	}
	for _, want := range []string{
		"Rotation checklist",
		"[ ] APP_GREETING (used by web, worker): rotate at the provider before first deploy",
		"[ ] POSTGRES_PASSWORD (used by web, worker, db): generated on the Server",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "hello-from") {
		t.Fatal("checklist printed a value")
	}

	g := &globals{json: true}
	c := migrateCmd(g)
	var buf bytes.Buffer
	c.SetOut(&buf)
	c.SetErr(&buf)
	c.SetArgs([]string{"draft", "--from", from})
	c.SilenceErrors, c.SilenceUsage = true, true
	if err := c.Execute(); err != nil {
		t.Fatal(err, buf.String())
	}
	var ev map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("not NDJSON: %q", line)
		}
		if e["event"] == "migrate_rotation_checklist" {
			ev = e
		}
	}
	items, _ := ev["secrets"].([]any)
	if len(items) != 2 {
		t.Fatalf("checklist event %v\n%s", ev, buf.String())
	}
	first := items[0].(map[string]any)
	if first["key"] != "APP_GREETING" || first["action"] != "rotate at the provider before first deploy" {
		t.Fatalf("item %v", first)
	}
	if svcs, _ := first["services"].([]any); len(svcs) != 2 {
		t.Fatalf("services %v", first["services"])
	}
}
