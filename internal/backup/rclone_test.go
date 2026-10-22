package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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
	h := &fakeHost{files: map[string][]byte{}, modes: map[string]os.FileMode{}}
	h.respond = func(_ context.Context, s string) (string, error) {
		if strings.Contains(s, "echo acquired") {
			return "acquired", nil
		}
		if strings.Contains(s, "mv -f ") {
			for _, line := range strings.Split(s, "\n") {
				if strings.HasPrefix(line, "mv -f ") {
					parts := strings.Split(line, "'")
					if len(parts) == 5 {
						h.files[parts[3]] = h.files[parts[1]]
						h.modes[parts[3]] = h.modes[parts[1]]
						delete(h.files, parts[1])
						delete(h.modes, parts[1])
					}
				}
			}
		}
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
		if strings.Contains(c.Script, "mv -f ") && strings.Contains(c.Script, " "+remote.Quote(p)+"\n") {
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
	h.files = map[string][]byte{rcloneConfPath: []byte(rcloneSecret("seeded"))}
	if _, err := List(context.Background(), ListOptions{App: "shop", Destination: "production", Host: h, Target: rcloneTarget()}); err != nil {
		t.Fatal(err)
	}
	if h.writes(rcloneConfPath) != 0 {
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

func TestRcloneDiagnosticsAndListLock(t *testing.T) {
	for _, typ := range []string{"archive", "restic"} {
		t.Run(typ, func(t *testing.T) {
			h := rcloneHost()
			respond := h.respond
			locked, unlocked := false, false
			line := `token = {"refresh_token":"sensitive-refresh-token"}`
			cfg := rcloneTarget()
			password := ""
			if typ == "restic" {
				cfg.Type = "restic"
				cfg.Repository = "rclone:od:backups"
				password = "backup-password"
			}
			h.respond = func(ctx context.Context, script string) (string, error) {
				if !locked {
					t.Fatal("remote operation before Destination lock")
				}
				if strings.Contains(script, "rc lsf ") || strings.Contains(script, "r snapshots ") {
					return "", fmt.Errorf("could not parse line: %s; token JSON: %s", line, strings.TrimPrefix(line, "token = "))
				}
				return respond(ctx, script)
			}
			_, err := List(context.Background(), ListOptions{App: "shop", Destination: "production", Target: cfg, Password: password, Host: h,
				RcloneConfig: "[od]\n" + line + "\n", Lock: func(context.Context) (func(context.Context) error, error) {
					locked = true
					return func(ctx context.Context) error {
						if ctx.Err() != nil {
							t.Fatal(ctx.Err())
						}
						unlocked = true
						return nil
					}, nil
				}})
			if err == nil || strings.Contains(err.Error(), "sensitive-refresh-token") || !strings.Contains(err.Error(), "***") || !unlocked {
				t.Fatalf("unsafe error or lock lifecycle: %v, unlocked %v", err, unlocked)
			}
			script := h.scripts()
			if !strings.Contains(script, "mv -f ") || strings.Index(script, " '"+rcloneConfPath+"'\n") > strings.Index(script, " '"+rcloneConfPath+".seed'\n") {
				t.Fatal("config must publish before sidecar")
			}
		})
	}
}

func TestRcloneListRefusesHeldLock(t *testing.T) {
	h := rcloneHost()
	_, err := List(context.Background(), ListOptions{App: "shop", Destination: "production", Target: rcloneTarget(), Host: h, RcloneConfig: rcloneSecret("x"),
		Lock: func(context.Context) (func(context.Context) error, error) { return nil, errors.New("already locked") }})
	if err == nil || !strings.Contains(err.Error(), "already locked") || len(h.cmds) != 0 {
		t.Fatalf("seed ran without lock: %v", err)
	}
}

func TestRcloneSeedScheduledUserRepairsMatchingSeed(t *testing.T) {
	h := rcloneHost()
	// A root-created config with an unchanged seed must still change owner.
	secret := rcloneSecret("unchanged")
	sum := sha256.Sum256([]byte(secret))
	h.files = map[string][]byte{rcloneConfPath: []byte("refreshed config"), rcloneConfPath + ".seed": []byte(hex.EncodeToString(sum[:]) + " date")}
	written, err := SeedRcloneConfigAsUser(context.Background(), h, "shop", "production", rcloneTarget(), secret, "yoho")
	if err != nil || written {
		t.Fatalf("matching seed: wrote %v, %v", written, err)
	}
	scripts := h.scripts()
	for _, want := range []string{"chown 'yoho:' '" + rcloneConfPath + "'", "chown 'yoho:' '" + rcloneConfPath + ".seed'", "chown 'yoho:' '/var/lib/yoho/backups/shop/production'", "sudo -n -u 'yoho' sh -c"} {
		if !strings.Contains(scripts, want) {
			t.Errorf("missing %s", want)
		}
	}
	if string(h.files[rcloneConfPath]) != "refreshed config" {
		t.Fatal("matching seed overwrote refreshed config")
	}
}

func TestRcloneSeedScheduledUserFirstSeed(t *testing.T) {
	h := rcloneHost()
	written, err := SeedRcloneConfigAsUser(context.Background(), h, "shop", "production", rcloneTarget(), rcloneSecret("new"), "yoho")
	if err != nil || !written {
		t.Fatalf("first seed: wrote %v, %v", written, err)
	}
	scripts := h.scripts()
	for _, want := range []string{"chown 'yoho:' '/var/lib/yoho/backups'", "chown 'yoho:' '/var/lib/yoho/backups/shop'", "sudo -n -u 'yoho' sh -c", "mv -f "} {
		if !strings.Contains(scripts, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(scripts, rcloneSecretToken) {
		t.Fatal("config secret in script")
	}
}

func TestRcloneListUsesDestinationLockByDefault(t *testing.T) {
	h := rcloneHost()
	_, err := List(context.Background(), ListOptions{App: "shop", Destination: "production", Host: h, Target: rcloneTarget(), RcloneConfig: rcloneSecret("lock")})
	if err != nil {
		t.Fatal(err)
	}
	scripts := h.scripts()
	acquired := strings.Index(scripts, "echo acquired")
	seeded := strings.Index(scripts, "#writefile ")
	listed := strings.Index(scripts, "rc lsf ")
	unlocked := strings.LastIndex(scripts, "rm -rf '/var/lib/yoho/apps/shop/production/lock'")
	if acquired < 0 || seeded < acquired || listed < seeded || unlocked < listed {
		t.Fatal("Destination lock did not span seed and List")
	}
}
