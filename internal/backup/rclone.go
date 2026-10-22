package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/secrets"
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
	t.diagnostics = secrets.RcloneConfigValues(value)
	if value != "" {
		if _, err := seedRclone(ctx, h, t.app, t.dest, key, value, now); err != nil {
			return secrets.RedactError(err, t.diagnostics...)
		}
	}
	values, err := RcloneDiagnosticValues(ctx, h, t.app, t.dest, t.cfg)
	if err != nil {
		return err
	}
	t.diagnostics = append(t.diagnostics, values...)
	for _, v := range t.env {
		t.diagnostics = append(t.diagnostics, v)
	}
	return nil
}

// SeedRcloneConfig writes the rclone_config_secret value of target to its
// persistent Server file unless the same value was seeded before. It reports
// whether it wrote the file. Call under the Destination lock. The value
// travels only over WriteFile's stdin.
func SeedRcloneConfig(ctx context.Context, h remote.Host, app, dest string, target config.BackupTarget, value string) (written bool, err error) {
	defer func() { err = secrets.RedactError(err, secrets.RcloneConfigValues(value)...) }()
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
	tmpName, _, err := newStagingName("seed")
	if err != nil {
		return false, err
	}
	tmpConf := path.Join(dir, tmpName)
	tmpSeed := tmpConf + rcloneSeedSuffix
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		_ = h.Run(cctx, remote.Cmd{Script: "set -eu\nrm -f " + remote.QuoteArgs(tmpConf, tmpSeed)})
	}()
	data := value
	if !strings.HasSuffix(data, "\n") {
		data += "\n"
	}
	if err := h.WriteFile(ctx, tmpConf, []byte(data), 0o600, false); err != nil {
		return false, fmt.Errorf("rclone config: write %s: %w", conf, err)
	}
	// Written after the config: a lost reply re-seeds next time, never skips.
	if err := h.WriteFile(ctx, tmpSeed, []byte(want+" "+now.UTC().Format(time.RFC3339)+"\n"), 0o600, false); err != nil {
		return false, fmt.Errorf("rclone config: write %s: %w", seed, err)
	}
	if err := h.Run(ctx, remote.Cmd{Script: safeDir(dir, true, false) +
		"yoho_secret " + remote.Quote(tmpConf) + "\nyoho_secret " + remote.Quote(tmpSeed) + "\n" +
		"rm -f " + remote.Quote(seed) + "\n" +
		"mv -f " + remote.QuoteArgs(tmpConf, conf) + "\nmv -f " + remote.QuoteArgs(tmpSeed, seed) + "\n"}); err != nil {
		return false, fmt.Errorf("publish rclone config: %w", err)
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

// diagnosticHost redacts remote failures and any explicitly streamed output.
type diagnosticHost struct {
	remote.Host
	values []string
}

func (h diagnosticHost) command(c remote.Cmd) (remote.Cmd, func()) {
	var rs []*secrets.Redactor
	for _, w := range []*io.Writer{&c.Stdout, &c.Stderr} {
		if *w != nil {
			r := secrets.NewRedactor(*w, h.values...)
			*w = r
			rs = append(rs, r)
		}
	}
	return c, func() {
		for _, r := range rs {
			_ = r.Flush()
		}
	}
}
func (h diagnosticHost) Run(ctx context.Context, c remote.Cmd) error {
	c, flush := h.command(c)
	defer flush()
	return secrets.RedactError(h.Host.Run(ctx, c), h.values...)
}
func (h diagnosticHost) Output(ctx context.Context, c remote.Cmd) (string, error) {
	c, flush := h.command(c)
	defer flush()
	out, err := h.Host.Output(ctx, c)
	return out, secrets.RedactError(err, h.values...)
}

// SeedRcloneConfigAsUser repairs only Yoho's private Backup directories and
// config files, then seeds as the system Scheduled Job's user. Call under the
// Destination lock, including when the secret hash has not changed.
func SeedRcloneConfigAsUser(ctx context.Context, h remote.Host, app, dest string, cfg config.BackupTarget, value, user string) (bool, error) {
	if !nameRe.MatchString(app) || !nameRe.MatchString(dest) || !envNameRe.MatchString(cfg.RcloneConfigSecret) || !cfg.UsesRclone() || !nameRe.MatchString(user) {
		return false, errors.New("rclone config: invalid seed identity or Target")
	}
	dir := RcloneConfigDir(app, dest, cfg.RcloneConfigSecret)
	dirs := []string{path.Join(release.Root, "backups"), path.Join(release.Root, "backups", app), release.BackupDir(app, dest), path.Dir(dir), dir}
	script := pathSafety + pathCreation + "yoho_uid=$(id -u " + remote.Quote(user) + ")\n"
	for i, d := range dirs {
		q := remote.Quote(d)
		script += "if [ ! -e " + q + " ] && [ ! -L " + q + " ]; then yoho_make " + q + "; chown " + remote.Quote(user+":") + " " + q + "; fi\n"
		if i < 2 {
			// Root may have created private spool ancestors during an earlier seed.
			script += "yoho_path " + q + "\nif [ -n \"$(find " + q + " -prune -user 0 ! -perm -0040 ! -perm -0010 ! -perm -0004 ! -perm -0001 -print)\" ]; then chown " + remote.Quote(user+":") + " " + q + "; fi\n"
		}
		if i >= 2 {
			// Accept only already-private root/user-owned resources before chown.
			script += "yoho_path " + q + "\n[ -n \"$(find " + q + " -prune ! -perm -0040 ! -perm -0020 ! -perm -0010 ! -perm -0004 ! -perm -0002 ! -perm -0001 -print)\" ] || yoho_bad " + q + "\nchown " + remote.Quote(user+":") + " " + q + "\n"
		}
	}
	for _, file := range []string{path.Join(dir, rcloneConfName), path.Join(dir, rcloneConfName) + rcloneSeedSuffix} {
		q := remote.Quote(file)
		script += "if [ -e " + q + " ] || [ -L " + q + " ]; then yoho_file " + q + "; [ -n \"$(find " + q + " -prune ! -perm -0040 ! -perm -0020 ! -perm -0010 ! -perm -0004 ! -perm -0002 ! -perm -0001 -print)\" ] || yoho_bad " + q + "; chown " + remote.Quote(user+":") + " " + q + "; fi\n"
	}
	if err := h.Run(ctx, remote.Cmd{Script: script, Sudo: true}); err != nil {
		return false, fmt.Errorf("rclone config ownership: %w", err)
	}
	return SeedRcloneConfig(ctx, rcloneUserHost{Host: h, user: user}, app, dest, cfg, value)
}

type rcloneUserHost struct {
	remote.Host
	user string
}

func (h rcloneUserHost) cmd(c remote.Cmd) remote.Cmd {
	c.Script = "set -eu\nexec sudo -n -u " + remote.Quote(h.user) + " sh -c " + remote.Quote(c.Script)
	c.Sudo = true
	return c
}
func (h rcloneUserHost) Run(ctx context.Context, c remote.Cmd) error {
	return h.Host.Run(ctx, h.cmd(c))
}
func (h rcloneUserHost) Output(ctx context.Context, c remote.Cmd) (string, error) {
	return h.Host.Output(ctx, h.cmd(c))
}
func (h rcloneUserHost) WriteFile(ctx context.Context, p string, b []byte, mode os.FileMode, _ bool) error {
	// The private directory was validated above; use an exclusive temp path.
	return h.Run(ctx, remote.Cmd{Script: "set -eu\numask 077\nset -C\ncat > " + remote.Quote(p) + "\nchmod " + fmt.Sprintf("%o", mode.Perm()) + " " + remote.Quote(p), Stdin: strings.NewReader(string(b))})
}

// RcloneDiagnosticValues reads the current private config, including tokens
// refreshed since installation. It never returns values in an error.
func RcloneDiagnosticValues(ctx context.Context, h remote.Host, app, dest string, cfg config.BackupTarget) ([]string, error) {
	if cfg.RcloneConfigSecret == "" {
		return nil, nil
	}
	if !nameRe.MatchString(app) || !nameRe.MatchString(dest) || !envNameRe.MatchString(cfg.RcloneConfigSecret) {
		return nil, errors.New("rclone config: invalid name")
	}
	dir := RcloneConfigDir(app, dest, cfg.RcloneConfigSecret)
	conf := path.Join(dir, rcloneConfName)
	if err := h.Run(ctx, remote.Cmd{Script: safeDir(dir, true, false) + "yoho_secret " + remote.Quote(conf) + "\n"}); err != nil {
		return nil, fmt.Errorf("rclone config is not seeded: %w", err)
	}
	b, err := h.ReadFile(ctx, conf, false)
	if err != nil {
		return nil, fmt.Errorf("read rclone config for redaction: %w", err)
	}
	return secrets.RcloneConfigValues(string(b)), nil
}
