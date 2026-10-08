// Package dist downloads release binaries of yoho from GitHub.
package dist

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
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

// APIBase is the GitHub REST API origin used when a token is available.
// Tests point it at an httptest server.
var APIBase = "https://api.github.com"

// currentToken returns a GitHub token, or "" to use the public release URLs.
// Tests replace it. discoverToken never logs the token.
var currentToken = discoverToken

// errNotFound is a download that returned HTTP 404. Callers map it to a
// release error. It is not wrapped with the URL.
var errNotFound = errors.New("not found")

// httpClient bounds a download when the caller context has no deadline.
// CheckRedirect is nil, so Go's default policy applies: Authorization is
// stripped when a redirect changes host. GitHub asset URLs redirect to a
// CDN; the token must not be forwarded there.
var httpClient = &http.Client{Timeout: 3 * time.Minute}

// maxAsset is a guard against a huge or hostile response.
const maxAsset = 256 << 20

const (
	acceptAPI   = "application/vnd.github+json"
	acceptAsset = "application/octet-stream"
)

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
//
// With a token (GITHUB_TOKEN, else GH_TOKEN, else `gh auth token`), the
// bytes come from the GitHub REST API. Otherwise the public release URLs
// are used.
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
	token := strings.TrimSpace(currentToken(ctx))

	var sum, binURL string
	if token != "" {
		sum, binURL, err = fetchAPI(ctx, token, version, asset)
	} else {
		sum, err = fetchChecksum(ctx, version, asset)
	}
	if err != nil {
		return "", err
	}
	if ok, _ := fileSumMatches(dest, sum); ok {
		if err := os.Chmod(dest, 0o755); err != nil {
			return "", err
		}
		return dest, nil
	}
	var body []byte
	if token != "" {
		body, err = getAsset(ctx, token, binURL, fmt.Errorf("release %s has no asset %s", version, asset))
	} else {
		body, err = getPublic(ctx, version, assetURL(version, asset))
	}
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

func releaseURL(version string) string {
	return strings.TrimRight(APIBase, "/") + "/repos/" + Repo + "/releases/tags/" + url.PathEscape(version)
}

func privateRepoHint(version string) error {
	return fmt.Errorf("release %s not found; if the repo is private set GITHUB_TOKEN or log in with gh", version)
}

func fetchChecksum(ctx context.Context, version, asset string) (string, error) {
	body, err := getPublic(ctx, version, assetURL(version, "checksums.txt"))
	if err != nil {
		return "", err
	}
	sum, ok := checksumEntry(string(body), asset)
	if !ok {
		return "", fmt.Errorf("release %s has no asset %s", version, asset)
	}
	return sum, nil
}

type ghAsset struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type ghRelease struct {
	Assets []ghAsset `json:"assets"`
}

// fetchAPI loads checksums.txt through the REST API and returns the sum
// plus the binary asset's API URL. The release document is fetched once.
func fetchAPI(ctx context.Context, token, version, asset string) (sum, binURL string, err error) {
	body, err := get(ctx, releaseURL(version), token, acceptAPI)
	if errors.Is(err, errNotFound) {
		return "", "", fmt.Errorf("release %s not found", version)
	}
	if err != nil {
		return "", "", err
	}
	var rel ghRelease
	if err := json.Unmarshal(body, &rel); err != nil {
		return "", "", fmt.Errorf("release %s: invalid response", version)
	}
	var sumURL string
	for _, a := range rel.Assets {
		switch a.Name {
		case "checksums.txt":
			sumURL = a.URL
		case asset:
			binURL = a.URL
		}
	}
	if sumURL == "" {
		return "", "", fmt.Errorf("release %s has no asset %s", version, "checksums.txt")
	}
	if binURL == "" {
		return "", "", fmt.Errorf("release %s has no asset %s", version, asset)
	}
	sumBody, err := getAsset(ctx, token, sumURL, fmt.Errorf("release %s has no asset %s", version, "checksums.txt"))
	if err != nil {
		return "", "", err
	}
	sum, ok := checksumEntry(string(sumBody), asset)
	if !ok {
		return "", "", fmt.Errorf("release %s has no asset %s", version, asset)
	}
	return sum, binURL, nil
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

func getPublic(ctx context.Context, version, rawURL string) ([]byte, error) {
	body, err := get(ctx, rawURL, "", "")
	if errors.Is(err, errNotFound) {
		return nil, privateRepoHint(version)
	}
	return body, err
}

func getAsset(ctx context.Context, token, rawURL string, notFound error) ([]byte, error) {
	body, err := get(ctx, rawURL, token, acceptAsset)
	if errors.Is(err, errNotFound) {
		return nil, notFound
	}
	return body, err
}

func get(ctx context.Context, rawURL, token, accept string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "yoho")
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: HTTP %s", rawURL, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAsset+1))
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", rawURL, err)
	}
	if len(body) > maxAsset {
		return nil, fmt.Errorf("download %s: response exceeds %d bytes", rawURL, maxAsset)
	}
	return body, nil
}

// discoverToken checks GITHUB_TOKEN, then GH_TOKEN, then `gh auth token`.
// The gh lookup is skipped when gh is not on PATH, times out after 3s, and
// ignores failures. The token is returned to the caller and never logged.
func discoverToken(ctx context.Context) string {
	if t := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); t != "" {
		return t
	}
	if t := strings.TrimSpace(os.Getenv("GH_TOKEN")); t != "" {
		return t
	}
	return ghAuthToken(ctx)
}

func ghAuthToken(ctx context.Context) string {
	exe, err := exec.LookPath("gh")
	if err != nil {
		return ""
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "auth", "token")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(out), "\n")
	return strings.TrimSpace(line)
}
