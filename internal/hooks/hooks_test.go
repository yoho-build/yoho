package hooks

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
