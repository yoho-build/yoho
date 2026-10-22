package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/release"
)

func runCheck(t *testing.T, path string, args ...string) (string, error) {
	t.Helper()
	c := configCmd(&globals{configPath: path, json: true})
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs(append([]string{"check"}, args...))
	c.SilenceErrors, c.SilenceUsage = true, true
	err := c.ExecuteContext(context.Background())
	return out.String(), err
}

func checkEvent(t *testing.T, out string) map[string]any {
	t.Helper()
	var found map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("invalid NDJSON %q: %v", line, err)
		}
		if e["event"] == "check" {
			found = e
		}
	}
	if found == nil {
		t.Fatalf("no check event in:\n%s", out)
	}
	return found
}

func exitCodeOf(err error) int {
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	if err != nil {
		return 1
	}
	return 0
}

func TestConfigCheckOfflineNeverRunsSecretLines(t *testing.T) {
	oldRoot := release.Root
	t.Cleanup(func() { release.Root = oldRoot })
	src := "services:\n  web: {image: nginx, restart: unless-stopped, x-yoho: {secrets: {ALIAS: KEY}}}\n"
	p := nonemptyApp(t, "compose", src, "KEY", "")
	dir := filepath.Dir(p)
	marker := filepath.Join(dir, "line-ran")
	if err := os.MkdirAll(filepath.Join(dir, ".yoho"), 0o700); err != nil {
		t.Fatal(err)
	}
	line := "KEY=$(touch " + marker + "; echo v)\n"
	if err := os.WriteFile(filepath.Join(dir, ".yoho", "secrets"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runCheck(t, p, "--offline")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatal("--offline ran a $(...) line")
	}
	if !strings.Contains(out, "secrets.nonempty not verified (--offline)") {
		t.Errorf("missing warning:\n%s", out)
	}
	e := checkEvent(t, out)
	if e["ok"] != true || e["offline"] != true || e["warnings"] != float64(1) || e["services"] != float64(1) || e["secret_keys"] != float64(1) {
		t.Errorf("check event: %v", e)
	}
	// YOHO_OFFLINE=1 is the same as the flag.
	t.Setenv("YOHO_OFFLINE", "1")
	if _, err := runCheck(t, p); err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatal("YOHO_OFFLINE=1 ran a $(...) line")
	}
	// Without it the opt-in resolves and runs the line.
	t.Setenv("YOHO_OFFLINE", "")
	if _, err := runCheck(t, p); err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatal("nonempty check without --offline should run the line")
	}
}

func TestConfigCheckWithoutNonemptyRunsNoSecretLine(t *testing.T) {
	oldRoot := release.Root
	t.Cleanup(func() { release.Root = oldRoot })
	p := appDir(t, "compose", "services:\n  web: {image: nginx, restart: unless-stopped, x-yoho: {secrets: {ALIAS: KEY}}}\n")
	dir := filepath.Dir(p)
	marker := filepath.Join(dir, "line-ran")
	if err := os.MkdirAll(filepath.Join(dir, ".yoho"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".yoho", "secrets"), []byte("KEY=$(touch "+marker+")\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runCheck(t, p)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatal("config check ran a $(...) line without secrets.nonempty")
	}
	if e := checkEvent(t, out); e["ok"] != true || e["offline"] != false || e["warnings"] != float64(0) {
		t.Errorf("check event: %v", e)
	}
}

func TestConfigCheckExitCodes(t *testing.T) {
	oldRoot := release.Root
	t.Cleanup(func() { release.Root = oldRoot })
	// Unknown secret name: findings, exit 1.
	p := appDir(t, "compose", "services:\n  web: {image: nginx, x-yoho: {secrets: {ALIAS: MISSING}}}\n")
	out, err := runCheck(t, p, "--offline")
	if exitCodeOf(err) != 1 || !strings.Contains(out, `"event":"finding"`) || !strings.Contains(out, "MISSING") {
		t.Fatalf("want exit 1 with finding, got %d:\n%s", exitCodeOf(err), out)
	}
	if e := checkEvent(t, out); e["ok"] != false || e["errors"] != float64(1) {
		t.Errorf("check event: %v", e)
	}
	// Malformed Yoho file: load error, exit 2.
	if err := os.WriteFile(p, []byte("app: [unterminated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = runCheck(t, p, "--offline")
	if exitCodeOf(err) != 2 {
		t.Fatalf("want exit 2, got %d: %v", exitCodeOf(err), err)
	}
	if e := checkEvent(t, out); e["ok"] != false {
		t.Errorf("check event: %v", e)
	}
	// Unreadable compose file: load error, exit 2.
	p = appDir(t, "compose", "services: [not, a, map\n")
	if _, err = runCheck(t, p, "--offline"); exitCodeOf(err) != 2 {
		t.Fatalf("bad compose: want exit 2, got %d: %v", exitCodeOf(err), err)
	}
}

func TestConfigCheckCountsSecretLoadWarnings(t *testing.T) {
	oldRoot := release.Root
	t.Cleanup(func() { release.Root = oldRoot })
	p := nonemptyApp(t, "compose", "services:\n  web: {image: nginx, restart: unless-stopped, x-yoho: {secrets: {ALIAS: KEY}}}\n", "KEY", "")
	dir := filepath.Dir(p)
	if err := os.MkdirAll(filepath.Join(dir, ".yoho"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".yoho", "secrets"), []byte("KEY=a\nKEY=b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runCheck(t, p)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "defined more than once") {
		t.Fatalf("expected the duplicate-key warning:\n%s", out)
	}
	if e := checkEvent(t, out); e["warnings"] != float64(1) {
		t.Errorf("check event: %v", e)
	}
}
