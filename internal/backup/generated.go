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
	script := appLink(app, dest) + safeDir(dir, false, false) +
		"d=" + remote.Quote(dir) + "\n" +
		"[ -d \"$d\" ] || exit 0\n" +
		"yoho_private \"$d\"\n" +
		"for f in \"$d\"/* \"$d\"/.[!.]* \"$d\"/..?*; do\n" +
		"  [ -e \"$f\" ] || [ -L \"$f\" ] || continue\n" +
		"  case \"${f##*/}\" in [0-9]*|*[!A-Za-z0-9_]*|\"\") continue;; esac\n" +
		"  yoho_secret \"$f\"\n" +
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
	if err := h.Run(ctx, remote.Cmd{Script: appLink(app, dest) + safeDir(src, true, false) + safeDir(path.Join(staging, generatedPrefix), true, true)}); err != nil {
		return fmt.Errorf("generated secret paths: %w", err)
	}
	for _, name := range names {
		if !generatedNameRe.MatchString(name) {
			return fmt.Errorf("invalid generated secret name %q", name)
		}
		if err := h.Run(ctx, remote.Cmd{Script: appLink(app, dest) + safeFile(path.Join(src, name), true)}); err != nil {
			return fmt.Errorf("unsafe generated secret %s: %w", name, err)
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

// dumpOnlyGenerated maps each generated secret in names to the Service that
// keeps its current value: a Service declaring it in x-yoho.generate
// (owners) that the Backup holds as a dump only. A dump (pg_dump, say) has
// no database roles, so the restored database keeps the password it has
// now; writing back the Backup's value would lock its consumers out. Keys
// with an owner whose volumes are in the Backup, or with no known owner in
// the Backup, are absent and restored as before.
func dumpOnlyGenerated(m Manifest, names []string, owners map[string][]string) map[string]string {
	out := map[string]string{}
	for _, name := range names {
		dumpOnly, withVolumes := "", false
		for _, svc := range owners[name] {
			ms, ok := m.Services[svc]
			if !ok {
				continue
			}
			if len(ms.Volumes) > 0 {
				withVolumes = true
			} else if ms.Dump != "" && dumpOnly == "" {
				dumpOnly = svc
			}
		}
		if dumpOnly != "" && !withVolumes {
			out[name] = dumpOnly
		}
	}
	return out
}

// restoreGenerated writes Backup copies of generated secrets back to the
// Server. wrote is every name written. changed is the subset whose previous
// value differed (not names that were absent). keep names secrets whose
// current value stays (see dumpOnlyGenerated): kept lists those whose
// Backup value differed and was not written; an absent one is still
// written. A note names the secret and not the value. The next deploy
// keeps the file (ensureFileOnce). Running Services keep the previous
// release's secrets/<generation> copy until that deploy.
func restoreGenerated(ctx context.Context, h remote.Host, app, dest, data string, names []string, keep map[string]string, logf func(string, ...any)) (wrote, changed, kept []string, err error) {
	if len(names) == 0 {
		return nil, nil, nil, nil
	}
	dir := generatedDir(app, dest)
	script := appLink(app, dest) + safeDir(dir, true, true)
	if err := h.Run(ctx, remote.Cmd{Script: script}); err != nil {
		return nil, nil, nil, fmt.Errorf("create generated dir: %w", err)
	}
	for _, name := range names {
		if !generatedNameRe.MatchString(name) {
			return nil, nil, nil, fmt.Errorf("manifest: invalid generated secret name %q", name)
		}
		if err := h.Run(ctx, remote.Cmd{Script: safeFile(path.Join(data, generatedPrefix, name), true)}); err != nil {
			return nil, nil, nil, fmt.Errorf("unsafe Backup generated secret %s: %w", name, err)
		}
		b, err := h.ReadFile(ctx, path.Join(data, generatedPrefix, name), false)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("generated secret %s missing from Backup: %w", name, err)
		}
		dst := path.Join(dir, name)
		if err := h.Run(ctx, remote.Cmd{Script: appLink(app, dest) + pathSafety + "yoho_private " + remote.Quote(dir) + "\nif [ -e " + remote.Quote(dst) + " ] || [ -L " + remote.Quote(dst) + " ]; then yoho_secret " + remote.Quote(dst) + "; fi\n"}); err != nil {
			return nil, nil, nil, fmt.Errorf("unsafe generated secret destination %s: %w", name, err)
		}
		existing, err := h.ReadFile(ctx, dst, false)
		switch {
		case err == nil:
			if bytes.Equal(existing, b) {
				break
			}
			if svc, ok := keep[name]; ok {
				logf("keeping the current generated secret %s: Service %s is restored from a dump, which has no database roles, so its database still uses the current value", name, svc)
				kept = append(kept, name)
				continue
			}
			logf("generated secret %s differs on the Server; restoring the Backup value", name)
			changed = append(changed, name)
		case errors.Is(err, os.ErrNotExist):
		default:
			return nil, nil, nil, fmt.Errorf("read generated secret %s: %w", name, err)
		}
		if err := h.WriteFile(ctx, dst, b, 0o600, false); err != nil {
			return nil, nil, nil, fmt.Errorf("write generated secret %s: %w", name, err)
		}
		wrote = append(wrote, name)
	}
	return wrote, changed, kept, nil
}
