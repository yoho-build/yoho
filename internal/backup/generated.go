package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// generatedPrefix is the directory inside a Backup that holds generated
// secrets. Names only; values are the file contents.
const generatedPrefix = "yoho-generated"

// generatedNameRe matches x-yoho.generate names (file names on the Server).
var generatedNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// generatedDir is <root>/apps/<app>/<destination>/generated. Compose and
// Swarm both write x-yoho.generate secrets there.
func generatedDir(app, dest string) string {
	return path.Join(release.AppDir(app, dest), "generated")
}

// TargetEncrypts reports whether a Backup Target encrypts at rest.
// restic always does. An archive does only for zip or 7z with a password
// (tar.gz cannot be encrypted).
func TargetEncrypts(cfg config.BackupTarget, password string) bool {
	typ := cfg.Type
	if typ == "" {
		typ = "restic"
	}
	format := cfg.Format
	if format == "" {
		format = "tar.gz"
	}
	switch typ {
	case "restic":
		return true
	case "archive":
		return password != "" && (format == "zip" || format == "7z")
	default:
		return false
	}
}

func (t *target) encryptsAtRest() bool {
	if t.typ == "restic" {
		return true
	}
	_, ok := t.env[envArchivePass]
	return ok && (t.format == "zip" || t.format == "7z")
}

func (o *RunOptions) targetLabel() string {
	if o.TargetName != "" {
		return o.TargetName
	}
	if o.Target.Repository != "" {
		return o.Target.Repository
	}
	if o.Target.Type != "" {
		return o.Target.Type
	}
	return "restic"
}

// ListGenerated returns the names of generated secrets on the Server.
// It never reads or returns values. Missing directory is an empty list.
func ListGenerated(ctx context.Context, h remote.Host, app, dest string) ([]string, error) {
	if h == nil {
		return nil, errors.New("backup: Host is required")
	}
	dir := generatedDir(app, dest)
	script := "set -eu\n" +
		"d=" + remote.Quote(dir) + "\n" +
		"[ -d \"$d\" ] || exit 0\n" +
		"for f in \"$d\"/* \"$d\"/.[!.]* \"$d\"/..?*; do\n" +
		"  [ -f \"$f\" ] || continue\n" +
		"  printf '%s\\n' \"${f##*/}\"\n" +
		"done\n"
	out, err := h.Output(ctx, remote.Cmd{Script: script})
	if err != nil {
		return nil, err
	}
	var names []string
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		name := strings.TrimSpace(line)
		if !generatedNameRe.MatchString(name) || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// stageGenerated copies generated secrets into staging/yoho-generated/<NAME>
// (files 0600, directory 0700). Callers must not log the bytes.
func stageGenerated(ctx context.Context, h remote.Host, app, dest, staging string, names []string) error {
	src := generatedDir(app, dest)
	for _, name := range names {
		if !generatedNameRe.MatchString(name) {
			return fmt.Errorf("invalid generated secret name %q", name)
		}
		b, err := h.ReadFile(ctx, path.Join(src, name), false)
		if err != nil {
			return fmt.Errorf("read generated secret %s: %w", name, err)
		}
		if err := h.WriteFile(ctx, path.Join(staging, generatedPrefix, name), b, 0o600, false); err != nil {
			return fmt.Errorf("stage generated secret %s: %w", name, err)
		}
	}
	return nil
}

// restoreGenerated writes Backup copies of generated secrets back to the
// Server. wrote is every name written. changed is the subset whose previous
// value differed (not names that were absent). A warning names the secret
// and not the value. The next deploy keeps the file (ensureFileOnce).
// Running Services keep the previous release's secrets/<generation> copy
// until that deploy.
func restoreGenerated(ctx context.Context, h remote.Host, app, dest, data string, names []string, logf func(string, ...any)) (wrote, changed []string, err error) {
	if len(names) == 0 {
		return nil, nil, nil
	}
	dir := generatedDir(app, dest)
	script := "set -eu; umask 077; mkdir -p " + remote.Quote(dir) + " && chmod 0700 " + remote.Quote(dir)
	if err := h.Run(ctx, remote.Cmd{Script: script}); err != nil {
		return nil, nil, fmt.Errorf("create generated dir: %w", err)
	}
	for _, name := range names {
		if !generatedNameRe.MatchString(name) {
			return nil, nil, fmt.Errorf("manifest: invalid generated secret name %q", name)
		}
		b, err := h.ReadFile(ctx, path.Join(data, generatedPrefix, name), false)
		if err != nil {
			return nil, nil, fmt.Errorf("generated secret %s missing from Backup: %w", name, err)
		}
		dst := path.Join(dir, name)
		existing, err := h.ReadFile(ctx, dst, false)
		switch {
		case err == nil:
			if !bytes.Equal(existing, b) {
				logf("generated secret %s differs on the Server; restoring the Backup value", name)
				changed = append(changed, name)
			}
		case errors.Is(err, os.ErrNotExist):
		default:
			return nil, nil, fmt.Errorf("read generated secret %s: %w", name, err)
		}
		if err := h.WriteFile(ctx, dst, b, 0o600, false); err != nil {
			return nil, nil, fmt.Errorf("write generated secret %s: %w", name, err)
		}
		wrote = append(wrote, name)
	}
	return wrote, changed, nil
}
