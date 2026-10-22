package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

const (
	rcloneSecretToken = "SECRET-REFRESH-TOKEN-4711"
	rcloneConfPath    = "/var/lib/yoho/backups/shop/production/.rclone/RCLONE_CONF/rclone.conf"
)

func rcloneSecret(n string) string {
	return "[od]\ntype = onedrive\ntoken = {\"refresh_token\":\"" + rcloneSecretToken + n + "\"}\n"
}

func rcloneTarget() config.BackupTarget {
	return config.BackupTarget{Type: "archive", Format: "7z", Repository: "od:backups/shop", RcloneConfigSecret: "RCLONE_CONF"}
}

// rcloneHost answers the seed probe from its own files, like the Server.
func rcloneHost() *fakeHost {
	h := &fakeHost{}
	h.respond = func(_ context.Context, s string) (string, error) {
		if strings.Contains(s, "head -n 1 ") {
			if _, ok := h.files[rcloneConfPath]; ok {
				seed := string(h.files[rcloneConfPath+".seed"])
				first, _, _ := strings.Cut(seed, "\n")
				return first, nil
			}
		}
		return "", nil
	}
	return h
}

func (f *fakeHost) writes(p string) int {
	n := 0
	for _, c := range f.cmds {
		if c.Script == "#writefile "+p {
			n++
		}
	}
	return n
}

func TestRcloneConfigSeedsOnlyOnFirstUseAndSecretChange(t *testing.T) {
	ctx := context.Background()
	h := rcloneHost()
	list := func(secret string) {
		t.Helper()
		if _, err := List(ctx, ListOptions{App: "shop", Destination: "production", Host: h, Target: rcloneTarget(), RcloneConfig: secret}); err != nil {
			t.Fatal(err)
		}
	}

	list(rcloneSecret("a"))
	if got := string(h.files[rcloneConfPath]); got != rcloneSecret("a") || h.modes[rcloneConfPath] != 0o600 {
		t.Fatalf("first use did not seed: %q %v", got, h.modes[rcloneConfPath])
	}
	sum := sha256.Sum256([]byte(rcloneSecret("a")))
	if seed := string(h.files[rcloneConfPath+".seed"]); !strings.HasPrefix(seed, hex.EncodeToString(sum[:])+" ") || h.modes[rcloneConfPath+".seed"] != 0o600 {
		t.Fatalf("seed sidecar %q", seed)
	}

	// rclone refreshed the token in the file; the same secret keeps it.
	h.files[rcloneConfPath] = []byte("[od]\ntoken = refreshed\n")
	list(rcloneSecret("a"))
	if got := string(h.files[rcloneConfPath]); got != "[od]\ntoken = refreshed\n" || h.writes(rcloneConfPath) != 1 {
		t.Fatalf("unchanged secret re-seeded: %q (%d writes)", got, h.writes(rcloneConfPath))
	}

	// The operator re-authorized: a new secret replaces the file.
	list(rcloneSecret("b"))
	if got := string(h.files[rcloneConfPath]); got != rcloneSecret("b") || h.writes(rcloneConfPath) != 2 {
		t.Fatalf("changed secret not re-seeded: %q", got)
	}

	// A missing config file is seeded again even with a matching sidecar.
	delete(h.files, rcloneConfPath)
	list(rcloneSecret("b"))
	if h.writes(rcloneConfPath) != 3 {
		t.Fatal("missing config not re-seeded")
	}
}

func TestRcloneConfigScriptsCarryNoSecret(t *testing.T) {
	ctx := context.Background()
	h := rcloneHost()
	secret := rcloneSecret("x")
	tg, err := newTarget("shop", "production", "yoho-shop-production", rcloneTarget(), "pw", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tg.prepareRclone(ctx, h, secret, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := tg.storeArchive(ctx, h, "/var/lib/yoho/backups/shop/production/run-x", "20261008T030000Z"); err != nil {
		t.Fatal(err)
	}
	if _, err := List(ctx, ListOptions{App: "shop", Destination: "production", Host: h, Target: rcloneTarget(), Password: "pw", RcloneConfig: secret}); err != nil {
		t.Fatal(err)
	}
	for _, c := range h.cmds {
		if strings.Contains(c.Script, rcloneSecretToken) {
			t.Fatalf("secret in script:\n%s", c.Script)
		}
		for k, v := range c.Env {
			if strings.Contains(v, rcloneSecretToken) {
				t.Fatalf("secret in env %s", k)
			}
		}
	}
	all := h.scripts()
	for _, want := range []string{
		"RCLONE_CONFIG='" + rcloneConfPath + "' rclone",
		"-e RCLONE_CONFIG=/yoho-rclone/rclone.conf",
		"-v '/var/lib/yoho/backups/shop/production/.rclone/RCLONE_CONF:/yoho-rclone'",
		`docker run --rm --user "$(id -u):$(id -g)" -e HOME=/tmp`,
		"yoho_secret '" + rcloneConfPath + "'",
		"rc copyto ", "rc lsf ",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("scripts lack %q", want)
		}
	}
}

func TestRcloneConfigWithoutSecretUsesSeededFile(t *testing.T) {
	h := rcloneHost()
	if _, err := List(context.Background(), ListOptions{App: "shop", Destination: "production", Host: h, Target: rcloneTarget()}); err != nil {
		t.Fatal(err)
	}
	if len(h.files) != 0 {
		t.Fatalf("seeded without a secret: %v", h.files)
	}
	if !strings.Contains(h.scripts(), "yoho_secret '"+rcloneConfPath+"'") {
		t.Fatalf("persistent config not required:\n%s", h.scripts())
	}
}

func TestRcloneConfigRestic(t *testing.T) {
	tg, err := newTarget("shop", "production", "yoho-shop-production",
		config.BackupTarget{Repository: "rclone:gd:restic/shop", PasswordSecret: "PW", RcloneConfigSecret: "RCLONE_CONF"}, "pw", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{tg.resticPrelude("/b"), tg.readOnlyResticPrelude()} {
		for _, want := range []string{"RCLONE_CONFIG='" + rcloneConfPath + "' restic", "-e RCLONE_CONFIG=/yoho-rclone/rclone.conf", ":/yoho-rclone'", `--user "$(id -u):$(id -g)"`} {
			if !strings.Contains(s, want) {
				t.Errorf("restic prelude lacks %q:\n%s", want, s)
			}
		}
	}
}

func TestRcloneConfigSecretValidation(t *testing.T) {
	for i, cfg := range []config.BackupTarget{
		{Type: "archive", Repository: "/srv/backups", RcloneConfigSecret: "RCLONE_CONF"},
		{Repository: "s3:s3.amazonaws.com/b", RcloneConfigSecret: "RCLONE_CONF"},
		{Type: "archive", Repository: "od:x", RcloneConfigSecret: "bad key"},
	} {
		if _, err := newTarget("a", "b", "yoho-a-b", cfg, "pw", nil); err == nil {
			t.Errorf("case %d: want error", i)
		}
	}
}

func TestRcloneAuthHint(t *testing.T) {
	tg, _ := newTarget("shop", "production", "yoho-shop-production", rcloneTarget(), "", nil)
	err := tg.rcloneAuthHint(errString("couldn't fetch token: invalid_grant: AADSTS70000"))
	if !strings.Contains(err.Error(), "rclone config reconnect") || !strings.Contains(err.Error(), "RCLONE_CONF") {
		t.Fatalf("no hint: %v", err)
	}
	if err := tg.rcloneAuthHint(errString("directory not found")); strings.Contains(err.Error(), "reconnect") {
		t.Fatalf("hint on non-auth error: %v", err)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// A Server-installed rclone rewrites the seeded file (as rclone does after a
// token refresh); the next run with the same secret keeps it.
func TestRcloneConfigLocalKeepsRefreshedTokens(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	old := release.Root
	release.Root = root
	t.Cleanup(func() { release.Root = old })
	bin := t.TempDir()
	fake := "#!/bin/sh\nprintf '[od]\\ntoken = refreshed\\n' > \"$RCLONE_CONFIG.tmp\"\nchmod 600 \"$RCLONE_CONFIG.tmp\"\nmv \"$RCLONE_CONFIG.tmp\" \"$RCLONE_CONFIG\"\n"
	if err := os.WriteFile(filepath.Join(bin, "rclone"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	ctx := context.Background()
	h := &remote.Local{HostName: "local"}
	tg, err := newTarget("shop", "production", "yoho-shop-production", rcloneTarget(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(RcloneConfigDir("shop", "production", "RCLONE_CONF"), "rclone.conf")
	read := func() string { b, _ := os.ReadFile(conf); return string(b) }
	if err := tg.prepareRclone(ctx, h, "", time.Now()); err == nil {
		t.Fatal("unseeded config must fail without the secret")
	}
	if err := tg.prepareRclone(ctx, h, "[od]\ntoken = a\n", time.Now()); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(conf); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("seeded file: %v %v", fi, err)
	}
	if _, err := tg.list(ctx, h, ""); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"[od]\ntoken = a\n", ""} {
		if err := tg.prepareRclone(ctx, h, secret, time.Now()); err != nil {
			t.Fatal(err)
		}
		if got := read(); got != "[od]\ntoken = refreshed\n" {
			t.Fatalf("refreshed token lost: %q", got)
		}
	}
	if got := tg.rcloneSeeded(ctx, h); !strings.HasPrefix(got, "seeded 20") {
		t.Fatalf("check report %q", got)
	}
	if err := tg.prepareRclone(ctx, h, "[od]\ntoken = b\n", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != "[od]\ntoken = b\n" {
		t.Fatalf("new secret not seeded: %q", got)
	}
}
