// Package dist downloads release binaries of yoho from GitHub.
package dist

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Repo is the GitHub repository that publishes yoho releases.
const Repo = "yoho-build/yoho"

// ErrDevBuild is returned when version is empty or "dev": there is no
// release artifact to download.
var ErrDevBuild = errors.New("dev build: no release to download")

// ReleaseBase is the URL prefix for "<base>/<version>/<asset>".
// Tests point it at an httptest server.
var ReleaseBase = "https://github.com/" + Repo + "/releases/download"

// httpClient bounds a download when the caller context has no deadline.
var httpClient = &http.Client{Timeout: 3 * time.Minute}

// maxAsset is a guard against a huge or hostile response.
const maxAsset = 256 << 20

// AssetName maps a docker-style platform (linux/amd64) to a release asset.
func AssetName(platform string) (string, error) {
	osName, arch, ok := strings.Cut(platform, "/")
	if !ok || osName == "" || arch == "" || strings.Contains(arch, "/") {
		return "", fmt.Errorf("invalid platform %q", platform)
	}
	switch osName {
	case "linux", "darwin":
	default:
		return "", fmt.Errorf("unsupported OS %q", osName)
	}
	switch arch {
	case "amd64", "arm64":
	default:
		return "", fmt.Errorf("unsupported architecture %q", arch)
	}
	return "yoho-" + osName + "-" + arch, nil
}

// Download fetches the yoho binary for version and platform into cacheDir
// (default: UserCacheDir/yoho/bin) and returns the cached path. A cached
// file is reused when its sha256 matches checksums.txt. version "dev" or
// empty returns ErrDevBuild.
func Download(ctx context.Context, version, platform, cacheDir string) (string, error) {
	if version == "" || version == "dev" {
		return "", ErrDevBuild
	}
	if strings.Contains(version, "/") || strings.Contains(version, "\\") || strings.Contains(version, "..") {
		return "", fmt.Errorf("invalid version %q", version)
	}
	asset, err := AssetName(platform)
	if err != nil {
		return "", err
	}
	if cacheDir == "" {
		cacheDir, err = defaultCacheDir()
		if err != nil {
			return "", err
		}
	}
	dest := filepath.Join(cacheDir, version, asset)
	sum, err := fetchChecksum(ctx, version, asset)
	if err != nil {
		return "", err
	}
	if ok, _ := fileSumMatches(dest, sum); ok {
		if err := os.Chmod(dest, 0o755); err != nil {
			return "", err
		}
		return dest, nil
	}
	body, err := get(ctx, assetURL(version, asset))
	if err != nil {
		return "", err
	}
	got := sha256.Sum256(body)
	if hex.EncodeToString(got[:]) != sum {
		return "", fmt.Errorf("checksum mismatch for %s", asset)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), asset+".*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return "", err
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return "", err
	}
	cleanup = false
	return dest, nil
}

func defaultCacheDir() (string, error) {
	d, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "yoho", "bin"), nil
}

func assetURL(version, name string) string {
	return strings.TrimRight(ReleaseBase, "/") + "/" + version + "/" + name
}

func fetchChecksum(ctx context.Context, version, asset string) (string, error) {
	body, err := get(ctx, assetURL(version, "checksums.txt"))
	if err != nil {
		return "", err
	}
	sum, ok := checksumEntry(string(body), asset)
	if !ok {
		return "", fmt.Errorf("release %s has no asset %s", version, asset)
	}
	return sum, nil
}

// checksumEntry finds asset in a sha256sum file ("<hash>  name" or "<hash> *name").
func checksumEntry(text, asset string) (string, bool) {
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || len(f[0]) != 64 {
			continue
		}
		name := strings.TrimPrefix(f[len(f)-1], "*")
		if name == asset || strings.HasSuffix(name, "/"+asset) {
			return strings.ToLower(f[0]), true
		}
	}
	return "", false
}

func fileSumMatches(path, sum string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == sum, nil
}

func get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "yoho")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		version, name := urlVersionName(url)
		return nil, fmt.Errorf("release %s has no asset %s", version, name)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: HTTP %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAsset+1))
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	if len(body) > maxAsset {
		return nil, fmt.Errorf("download %s: response exceeds %d bytes", url, maxAsset)
	}
	return body, nil
}

func urlVersionName(raw string) (version, name string) {
	base := strings.TrimRight(ReleaseBase, "/") + "/"
	rest := strings.TrimPrefix(raw, base)
	version, name, _ = strings.Cut(rest, "/")
	if name == "" {
		name = rest
	}
	return version, name
}
