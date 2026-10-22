package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// Persistent rclone config (rclone_config_secret). OAuth remotes such as
// OneDrive refresh their token on every run and write it back to the config
// file, so the config lives in a private Server file instead of env. The CLI
// seeds it from the secret; Scheduled Jobs only use it and never carry the
// secret. A sidecar records the secret's SHA-256 so the file is re-seeded
// only when the operator changes the secret (re-authorized), never over
// tokens rclone refreshed since.
const (
	rcloneConfName   = "rclone.conf"
	rcloneSeedSuffix = ".seed"
	rcloneMountDir   = "/yoho-rclone"
)

// RcloneConfigDir is the private Server directory holding the persistent
// rclone config seeded from secret key for an App Destination. Targets that
// name the same secret share it, so a rotated refresh token is never split.
func RcloneConfigDir(app, dest, key string) string {
	return path.Join(release.BackupDir(app, dest), ".rclone", key)
}

// rcloneRun is how scripts point rclone (or restic's rclone backend) at the
// persistent config.
type rcloneRun struct {
	check     string // validates the private dir and 0600 file
	native    string // env prefix for a Server-installed binary
	docker    string // docker run flags for the rclone container
	dockerEnv string // docker run flags when --user is already set
	mount     string // read-write -v spec of the config directory
}

func (t *target) rcloneConfig() *rcloneRun {
	key := t.cfg.RcloneConfigSecret
	if key == "" {
		return nil
	}
	dir := RcloneConfigDir(t.app, t.dest, key)
	conf := path.Join(dir, rcloneConfName)
	env := " -e RCLONE_CONFIG=" + rcloneMountDir + "/" + rcloneConfName
	return &rcloneRun{
		check:  safeDir(dir, true, false) + "yoho_secret " + remote.Quote(conf) + "\n",
		native: "RCLONE_CONFIG=" + remote.Quote(conf) + " ",
		// The deploy user owns the file; rclone rewrites it (temp file +
		// rename in the mounted directory) as that user, keeping it 0600.
		docker:    ` --user "$(id -u):$(id -g)" -e HOME=/tmp` + env,
		dockerEnv: env,
		mount:     dir + ":" + rcloneMountDir,
	}
}

// prepareRclone seeds the persistent rclone config when value (the resolved
// rclone_config_secret) is set, otherwise requires an earlier seed.
func (t *target) prepareRclone(ctx context.Context, h remote.Host, value string, now time.Time) error {
	key := t.cfg.RcloneConfigSecret
	if key == "" {
		return nil
	}
	if value != "" {
		_, err := seedRclone(ctx, h, t.app, t.dest, key, value, now)
		return err
	}
	if err := h.Run(ctx, remote.Cmd{Script: t.rcloneConfig().check}); err != nil {
		return fmt.Errorf("rclone config %s is not seeded on the Server; run `yoho schedule install` or a `yoho backup` command with the secret: %w", key, err)
	}
	return nil
}

// SeedRcloneConfig writes the rclone_config_secret value of target to its
// persistent Server file unless the same value was seeded before. It reports
// whether it wrote the file. The value travels only over WriteFile's stdin.
func SeedRcloneConfig(ctx context.Context, h remote.Host, app, dest string, target config.BackupTarget, value string) (bool, error) {
	if target.RcloneConfigSecret == "" {
		return false, nil
	}
	if !nameRe.MatchString(app) || !nameRe.MatchString(dest) {
		return false, errors.New("rclone config: invalid App or Destination name")
	}
	if !target.UsesRclone() {
		return false, errors.New("rclone config: rclone_config_secret requires an rclone repository")
	}
	return seedRclone(ctx, h, app, dest, target.RcloneConfigSecret, value, time.Now())
}

func seedRclone(ctx context.Context, h remote.Host, app, dest, key, value string, now time.Time) (bool, error) {
	if !envNameRe.MatchString(key) {
		return false, fmt.Errorf("rclone config: invalid secret key %q", key)
	}
	if strings.TrimSpace(value) == "" {
		return false, fmt.Errorf("rclone config: secret %s is empty", key)
	}
	dir := RcloneConfigDir(app, dest, key)
	conf := path.Join(dir, rcloneConfName)
	seed := conf + rcloneSeedSuffix
	sum := sha256.Sum256([]byte(value))
	want := hex.EncodeToString(sum[:])
	got, err := readRcloneSeed(ctx, h, dir, true)
	if err != nil {
		return false, fmt.Errorf("rclone config: %w", err)
	}
	if f := strings.Fields(got); len(f) > 0 && f[0] == want {
		return false, nil
	}
	data := value
	if !strings.HasSuffix(data, "\n") {
		data += "\n"
	}
	if err := h.WriteFile(ctx, conf, []byte(data), 0o600, false); err != nil {
		return false, fmt.Errorf("rclone config: write %s: %w", conf, err)
	}
	// Written after the config: a lost reply re-seeds next time, never skips.
	if err := h.WriteFile(ctx, seed, []byte(want+" "+now.UTC().Format(time.RFC3339)+"\n"), 0o600, false); err != nil {
		return false, fmt.Errorf("rclone config: write %s: %w", seed, err)
	}
	return true, nil
}

// readRcloneSeed returns the sidecar ("<sha256> <time>") when both the config
// and the sidecar exist and are private; empty otherwise. create makes the
// private directory first.
func readRcloneSeed(ctx context.Context, h remote.Host, dir string, create bool) (string, error) {
	conf := path.Join(dir, rcloneConfName)
	seed := conf + rcloneSeedSuffix
	qc, qs := remote.Quote(conf), remote.Quote(seed)
	script := safeDir(dir, true, create) +
		"if [ -e " + qc + " ] || [ -L " + qc + " ]; then\n yoho_secret " + qc + "\n" +
		" if [ -e " + qs + " ] || [ -L " + qs + " ]; then yoho_secret " + qs + "; head -n 1 " + qs + "; fi\nfi\n"
	if !create {
		script = pathSafety + "[ -d " + remote.Quote(dir) + " ] || exit 0\n" + script
	}
	return h.Output(ctx, remote.Cmd{Script: script})
}

// rcloneSeeded describes the persistent config for `backup check`.
func (t *target) rcloneSeeded(ctx context.Context, h remote.Host) string {
	if t.cfg.RcloneConfigSecret == "" {
		return ""
	}
	got, err := readRcloneSeed(ctx, h, RcloneConfigDir(t.app, t.dest, t.cfg.RcloneConfigSecret), false)
	f := strings.Fields(got)
	if err != nil || len(f) < 2 {
		return "seeded (date unknown)"
	}
	return "seeded " + f[1]
}

var rcloneAuthRe = regexp.MustCompile(`(?i)invalid_grant|token (has )?expired|couldn't fetch token|failed to (get|refresh) token|unauthenticated|AADSTS[0-9]+|\b401\b`)

// rcloneAuthHint adds re-authorization advice to an rclone auth failure.
func (t *target) rcloneAuthHint(err error) error {
	if err == nil || t.cfg.RcloneConfigSecret == "" || !rcloneAuthRe.MatchString(err.Error()) {
		return err
	}
	return fmt.Errorf("%w\nhint: rclone could not authenticate; run `rclone config reconnect <remote>:` locally, then update secret %s with the new section", err, t.cfg.RcloneConfigSecret)
}
