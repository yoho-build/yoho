package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrateDraftWritesFilesAndChecks(t *testing.T) {
	dir := t.TempDir()
	from := filepath.Join("..", "..", "examples", "quickstart", "railway", "inventory.json")
	out, err := runDraft(t, "draft", "--from", from, "--out", dir)
	if err != nil {
		t.Fatal(err, out)
	}
	if !strings.Contains(out, "APP_GREETING=$(op read op://Production/shop/APP_GREETING)") {
		t.Fatalf("missing secret reference:\n%s", out)
	}
	if strings.Contains(out, "POSTGRES_PASSWORD=$(op") {
		t.Fatalf("generated password was copied into a secret line:\n%s", out)
	}
	compose, err := os.ReadFile(filepath.Join(dir, "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	yoho, err := os.ReadFile(filepath.Join(dir, "yoho.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(compose), "hello-from") || strings.Contains(string(yoho), "postgres://") {
		t.Fatal("draft wrote a secret value")
	}
	if _, err := runDraft(t, "draft", "--from", from, "--out", dir); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("overwrite: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".yoho"), 0o700); err != nil {
		t.Fatal(err)
	}
	secrets := "APP_GREETING=$(op read op://Production/shop/APP_GREETING)\nCF_API_TOKEN=$(op read op://Production/shop/CF_API_TOKEN)\nCF_TUNNEL_TOKEN=$(op read op://Production/shop/CF_TUNNEL_TOKEN)\n"
	if err := os.WriteFile(filepath.Join(dir, ".yoho", "secrets"), []byte(secrets), 0o600); err != nil {
		t.Fatal(err)
	}
	check, err := runCheck(t, filepath.Join(dir, "yoho.yml"), "--offline")
	if err != nil {
		t.Fatalf("config check: %v\n%s", err, check)
	}
	ev := checkEvent(t, check)
	if ev["ok"] != true {
		t.Fatalf("check event %#v\n%s", ev, check)
	}
}

func TestMigrateDraftJSONAndMissingFrom(t *testing.T) {
	g := &globals{json: true}
	c := migrateCmd(g)
	var buf bytes.Buffer
	c.SetOut(&buf)
	c.SetErr(&buf)
	c.SetArgs([]string{"draft"})
	c.SilenceErrors, c.SilenceUsage = true, true
	if err := c.Execute(); err == nil || !strings.Contains(err.Error(), "--from") {
		t.Fatalf("missing --from: %v\n%s", err, buf.String())
	}
	plan, _, err := migrateCmd(g).Find([]string{"plan"})
	if err != nil || plan.Flags().Lookup("cutover-allow-empty") == nil {
		t.Fatalf("plan flag: %v", err)
	}
}

func runDraft(t *testing.T, args ...string) (string, error) {
	t.Helper()
	c := migrateCmd(&globals{})
	var buf bytes.Buffer
	c.SetOut(&buf)
	c.SetErr(&buf)
	c.SetArgs(args)
	c.SilenceErrors, c.SilenceUsage = true, true
	err := c.Execute()
	return buf.String(), err
}
