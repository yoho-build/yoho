package deploy

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/dotenv"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/remote"
)

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func TestEnsureGeneratedNeverOverwrites(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	h := &remote.Local{}
	kinds := map[string]string{"PG": "password32", "KEY": "hex32"}
	first, err := ensureGenerated(ctx, h, dir, kinds)
	if err != nil {
		t.Fatal(err)
	}
	if len(first["PG"]) != 32 || len(first["KEY"]) != 64 {
		t.Fatalf("generated %v", first)
	}
	second, err := ensureGenerated(ctx, h, dir, kinds)
	if err != nil {
		t.Fatal(err)
	}
	if first["PG"] != second["PG"] || first["KEY"] != second["KEY"] {
		t.Error("generated secrets must be created once")
	}
	if m := mode(t, filepath.Join(dir, "generated", "PG")); m != 0o600 {
		t.Errorf("generated mode %o", m)
	}

	k1, err := ensureHMACKey(ctx, h, dir)
	if err != nil {
		t.Fatal(err)
	}
	k2, _ := ensureHMACKey(ctx, h, dir)
	if len(k1) != 32 || !bytes.Equal(k1, k2) {
		t.Error("hmac.key must be 32 bytes and stable")
	}
	if m := mode(t, filepath.Join(dir, "hmac.key")); m != 0o600 {
		t.Errorf("hmac.key mode %o", m)
	}
}

func TestGeneratedKindConflict(t *testing.T) {
	d := testDeploy(t, nil, nil)
	d.Ext["web"] = config.ServiceExt{Generate: map[string]string{"POSTGRES_PASSWORD": "hex64"}}
	if _, err := generatedDecls(d); err == nil {
		t.Error("conflicting kinds must fail")
	}
}

func TestServiceSecretsUnresolved(t *testing.T) {
	d := testDeploy(t, nil, nil)
	_, err := serviceSecrets(d, map[string]string{}, t.Logf)
	if err == nil || !strings.Contains(err.Error(), "POSTGRES_PASSWORD") {
		t.Errorf("err = %v", err)
	}
}

func TestWriteSecretsLayout(t *testing.T) {
	ctx := context.Background()
	gen := filepath.Join(t.TempDir(), "secrets", "G1")
	if err := os.MkdirAll(filepath.Dir(gen), 0o700); err != nil {
		t.Fatal(err)
	}
	d := testDeploy(t, nil, nil)
	svc, err := serviceSecrets(d, map[string]string{"POSTGRES_PASSWORD": "pg"}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if svc["web"]["DATABASE_PASSWORD"] != "pg" || svc["db"]["POSTGRES_PASSWORD"] != "pg" {
		t.Fatalf("generated secret not shared: %v", svc)
	}
	if err := writeSecrets(ctx, &remote.Local{}, gen, svc, map[string]bool{"cloudflared": true}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{gen, filepath.Join(gen, "web"), filepath.Join(gen, "db")} {
		if m := mode(t, p); m != 0o700 {
			t.Errorf("%s mode %o, want 0700", p, m)
		}
	}
	f := filepath.Join(gen, "web", "SECRET_KEY_BASE")
	if m := mode(t, f); m != 0o444 {
		t.Errorf("secret file mode %o, want 0444", m)
	}
	if b, _ := os.ReadFile(f); string(b) != webSecret {
		t.Errorf("secret content %q", b)
	}
	envPath := filepath.Join(gen, "cloudflared.env")
	if m := mode(t, envPath); m != 0o600 {
		t.Errorf("env file mode %o, want 0600", m)
	}
	if _, err := os.Stat(filepath.Join(gen, "cloudflared")); !errors.Is(err, os.ErrNotExist) {
		t.Error("env-delivered service must not get a file directory")
	}
	env, err := dotenv.GetEnvFromFile(nil, []string{envPath})
	if err != nil {
		t.Fatal(err)
	}
	if env["TUNNEL_TOKEN"] != tunnelSecret {
		t.Errorf("env round trip: %q", env["TUNNEL_TOKEN"])
	}
}

func TestEnvFileRoundTrip(t *testing.T) {
	vals := map[string]string{"A": `plain`, "B": `with "quotes" and \backslash\`, "C": "multi\nline", "D": `$HOME ${X} $$`, "E": `'single'`, "F": `ends\`}
	p := filepath.Join(t.TempDir(), "x.env")
	if err := os.WriteFile(p, envFile(vals), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := dotenv.GetEnvFromFile(map[string]string{"HOME": "/nope", "X": "nope"}, []string{p})
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range vals {
		if got[k] != v {
			t.Errorf("%s: got %q want %q", k, got[k], v)
		}
	}
}
