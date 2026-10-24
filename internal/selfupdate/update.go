// Package selfupdate replaces the local CLI with a verified stable release.
package selfupdate

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/yoho-build/yoho/internal/dist"
)

type Result struct {
	Previous string `json:"previous"`
	Version  string `json:"version"`
	Path     string `json:"path"`
	Updated  bool   `json:"updated"`
}

type updater struct {
	executable func() (string, error)
	latest     func(context.Context) (string, error)
	download   func(context.Context, string, string, string) (string, error)
	platform   string
}

// Upgrade updates the executable that is running, following symlinks. Downloads
// must pass both SHA-256 and signed release provenance verification in dist.
func Upgrade(ctx context.Context, current string) (Result, error) {
	return (updater{os.Executable, dist.Latest, dist.Download, runtime.GOOS + "/" + runtime.GOARCH}).run(ctx, current)
}

func (u updater) run(ctx context.Context, current string) (Result, error) {
	result := Result{Previous: current}
	if _, err := dist.AssetName(u.platform); err != nil {
		return result, err
	}
	latest, err := u.latest(ctx)
	if err != nil {
		return result, fmt.Errorf("check latest Yoho release: %w", err)
	}
	if !stable.MatchString(latest) {
		return result, fmt.Errorf("invalid stable release version %q", latest)
	}
	result.Version = latest
	if current == latest || newerStable(current, latest) {
		result.Version = current
		return result, nil
	}
	executable, err := u.executable()
	if err != nil {
		return result, fmt.Errorf("locate running executable: %w", err)
	}
	target, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return result, fmt.Errorf("resolve running executable: %w", err)
	}
	result.Path = target
	for _, part := range []string{"/Cellar/", "/Caskroom/", "/nix/store/"} {
		if strings.Contains(filepath.ToSlash(target), part) {
			return result, fmt.Errorf("%s is managed by a package manager; update Yoho with that package manager", target)
		}
	}
	original, err := os.Stat(target)
	if err != nil {
		return result, err
	}
	if !original.Mode().IsRegular() {
		return result, fmt.Errorf("%s is not a regular executable", target)
	}
	staged, err := os.CreateTemp(filepath.Dir(target), ".yoho-upgrade-*")
	if err != nil {
		return result, fmt.Errorf("cannot update %s: directory must be writable by your user; reinstall Yoho into a writable directory: %w", target, err)
	}
	defer os.Remove(staged.Name())
	defer staged.Close()
	cache, err := os.MkdirTemp("", "yoho-upgrade-*")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(cache)
	source, err := u.download(ctx, latest, u.platform, cache)
	if err != nil {
		return result, fmt.Errorf("download verified Yoho %s: %w", latest, err)
	}
	f, err := os.Open(source)
	if err != nil {
		return result, err
	}
	defer f.Close()
	if _, err := io.Copy(staged, f); err != nil {
		return result, err
	}
	if err := staged.Chmod(original.Mode().Perm()); err != nil {
		return result, err
	}
	if err := staged.Sync(); err != nil {
		return result, err
	}
	if err := staged.Close(); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	// Refuse to overwrite a different installation changed during the download.
	now, err := os.Lstat(target)
	if err != nil {
		return result, err
	}
	if !os.SameFile(original, now) {
		return result, fmt.Errorf("%s changed during upgrade; retry", target)
	}
	if err := os.Rename(staged.Name(), target); err != nil {
		return result, fmt.Errorf("replace %s: %w", target, err)
	}
	result.Updated = true
	return result, nil
}

var stable = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// Compare decimal strings without integer overflow. Development builds may be
// replaced; a newer stable installation must never be downgraded by "latest".
func newerStable(current, latest string) bool {
	if !stable.MatchString(current) {
		return false
	}
	a, b := strings.Split(current[1:], "."), strings.Split(latest[1:], ".")
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return len(a[i]) > len(b[i])
		}
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}
