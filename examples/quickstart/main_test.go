package main

import (
	"os"
	"testing"
)

func TestFingerprintStableAndOpaque(t *testing.T) {
	length, prefix := fingerprint("hello-from-yoho")
	if length != 15 {
		t.Fatalf("length = %d", length)
	}
	if prefix != "6e6bcacd" {
		t.Fatalf("sha256_8 = %q", prefix)
	}
	if prefix == "hello-from-yoho" {
		t.Fatal("fingerprint leaked the value")
	}
}

func TestSecretPrefersFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/greeting"
	if err := os.WriteFile(path, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("APP_GREETING_FILE", path)
	t.Setenv("APP_GREETING", "from-env")
	got, err := secret("APP_GREETING")
	if err != nil {
		t.Fatal(err)
	}
	if got != "from-file" {
		t.Fatalf("got %q", got)
	}
}

func TestSecretEnvFallback(t *testing.T) {
	t.Setenv("APP_GREETING_FILE", "")
	t.Setenv("APP_GREETING", "from-env")
	got, err := secret("APP_GREETING")
	if err != nil {
		t.Fatal(err)
	}
	if got != "from-env" {
		t.Fatalf("got %q", got)
	}
}
