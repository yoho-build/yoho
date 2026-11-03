package hooks

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/plan"
)

func writeHook(t *testing.T, dir, name, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), mode); err != nil {
		t.Fatal(err)
	}
}

func TestMissingHookIsNoop(t *testing.T) {
	h := New(t.TempDir(), nil, nil)
	if err := h(context.Background(), "pre-deploy", nil); err != nil {
		t.Fatal(err)
	}
}

func TestUnknownHook(t *testing.T) {
	h := New(t.TempDir(), nil, nil)
	if err := h(context.Background(), "pre-everything", nil); err == nil {
		t.Fatal("want error for unknown hook")
	}
}

func TestHookEnvAndOutput(t *testing.T) {
	dir := t.TempDir()
	writeHook(t, dir, "post-deploy", `echo "$YOHO_APP $YOHO_DESTINATION $YOHO_SERVICE_VERSION $YOHO_RUNTIME rec=${YOHO_RECORDED_AT:+yes}"`, 0o755)
	var out bytes.Buffer
	h := New(dir, map[string]string{"YOHO_APP": "shop", "YOHO_DESTINATION": "production", "YOHO_VERSION": "abc", "YOHO_RUNTIME": "1"}, &out)
	if err := h(context.Background(), "post-deploy", map[string]string{"YOHO_RUNTIME": "42"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "shop production shop@abc 42 rec=yes") {
		t.Errorf("output: %q", out.String())
	}
}

func TestHookFailureAborts(t *testing.T) {
	dir := t.TempDir()
	writeHook(t, dir, "pre-build", "exit 3", 0o755)
	err := New(dir, nil, nil)(context.Background(), "pre-build", nil)
	if err == nil || !strings.Contains(err.Error(), "pre-build") || !strings.Contains(err.Error(), "exit 3") {
		t.Fatalf("err = %v", err)
	}
}

func TestNonExecutableSkipped(t *testing.T) {
	dir := t.TempDir()
	writeHook(t, dir, "pre-deploy", "exit 1", 0o644)
	var out bytes.Buffer
	if err := New(dir, nil, &out)(context.Background(), "pre-deploy", nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "not executable") {
		t.Errorf("missing warning: %q", out.String())
	}
}

func TestHosts(t *testing.T) {
	got := Hosts([]plan.NamedHost{
		{Name: "a", Server: config.Server{SSH: "root@127.0.0.1:2222"}},
		{Name: "b", Server: config.Server{SSH: "ssh://deploy@web.example.com"}},
		{Name: "c", Server: config.Server{SSH: "[::1]:22"}},
		{Name: "local"},
	})
	if got != "127.0.0.1:2222,web.example.com,[::1]:22,local" {
		t.Errorf("Hosts = %q", got)
	}
}

// TestHookOrderAndEnv runs scripts that append "$0 $YOHO_COMMAND $YOHO_LOCK
// $YOHO_HOSTS" to a log, like the QA hooks.
func TestHookOrderAndEnv(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(t.TempDir(), "hooks.log")
	for _, n := range []string{"pre-deploy", "post-deploy"} {
		writeHook(t, dir, n, `echo "$(basename "$0") $YOHO_COMMAND $YOHO_LOCK $YOHO_HOSTS" >> `+log, 0o755)
	}
	h := New(dir, map[string]string{"YOHO_COMMAND": "rollback", "YOHO_HOSTS": "h1"}, nil)
	ctx := context.Background()
	if err := h(ctx, "pre-deploy", nil); err != nil {
		t.Fatal(err)
	}
	if err := h(ctx, "post-deploy", map[string]string{"YOHO_LOCK": LockHeld, "YOHO_HOSTS": "h1:22,h2"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if want := "pre-deploy rollback false h1\npost-deploy rollback true h1:22,h2\n"; string(b) != want {
		t.Errorf("log\n%s\nwant\n%s", b, want)
	}
}

func TestSamplesRun(t *testing.T) {
	dir := t.TempDir()
	for _, n := range Names {
		if when[n] == "" {
			t.Errorf("hook %s has no description", n)
		}
		s := Sample(n)
		for _, env := range []string{"YOHO_HOSTS", "YOHO_LOCK", "YOHO_COMMAND", "YOHO_RUNTIME"} {
			if !strings.Contains(s, env) {
				t.Errorf("%s.sample does not mention %s", n, env)
			}
		}
		// Enabled as is, a sample must run (set -u with no YOHO_* env).
		if err := os.WriteFile(filepath.Join(dir, n), []byte(s), 0o755); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := New(dir, nil, &out)(context.Background(), n, nil); err != nil {
			t.Errorf("%s sample: %v\n%s", n, err, out.String())
		}
	}
}
