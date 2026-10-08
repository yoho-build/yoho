package build

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/yoho-build/yoho/internal/ui"
)

// containerReleaseAPI is the GitHub releases endpoint for Apple container.
const containerReleaseAPI = "https://api.github.com/repos/apple/container/releases/latest"

const containerUpdateTTL = 24 * time.Hour

// containerVersionRE extracts a dotted version from `container --version`.
var containerVersionRE = regexp.MustCompile(`\d+\.\d+\.\d+`)

// containerLatestCache is os.UserCacheDir()/yoho/container-latest.json.
type containerLatestCache struct {
	CheckedAt time.Time `json:"checked_at"`
	Latest    string    `json:"latest"`
}

// containerUpdateConfig is the Apple container update check. Zero values use
// the production defaults (user cache dir, GitHub, 2s, process environment).
type containerUpdateConfig struct {
	Installed string
	Exec      Exec
	Getenv    func(string) string
	Now       func() time.Time
	APIURL    string
	CacheFile string
	Timeout   time.Duration
	Client    *http.Client
}

// VersionLess reports whether a is older than b. Each dotted component is
// numeric, so 1.10.0 is newer than 1.5.0. A leading v is ignored. Missing
// components compare as zero.
func VersionLess(a, b string) bool {
	pa := strings.Split(strings.TrimPrefix(a, "v"), ".")
	pb := strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x < y
		}
	}
	return false
}

func (c containerUpdateConfig) getenv(k string) string {
	if c.Getenv != nil {
		return c.Getenv(k)
	}
	return os.Getenv(k)
}

func (c containerUpdateConfig) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// containerUpdateHint returns one hint line when the installed Apple container
// is older than the latest release, or "" when the install is current, the
// check is disabled, or the network check cannot finish. It never returns an
// error: a build must not fail because the update check did.
func containerUpdateHint(ctx context.Context, cfg containerUpdateConfig) string {
	if cfg.getenv("YOHO_NO_UPDATE_CHECK") == "1" {
		return ""
	}
	installed := cfg.Installed
	if installed == "" {
		v, ok := installedContainerVersion(ctx, cfg.Exec)
		if !ok {
			return ""
		}
		installed = v
	}
	cacheFile, err := cfg.cacheFile()
	if err != nil {
		return ""
	}
	now := cfg.now()
	if latest, ok := readFreshContainerCache(cacheFile, now); ok {
		return outdatedContainerHint(installed, latest)
	}
	latest, err := fetchLatestContainer(ctx, cfg)
	if err != nil {
		return ""
	}
	// Remember the answer so the next build within 24h does not hit the network.
	_ = writeContainerCache(cacheFile, containerLatestCache{CheckedAt: now.UTC(), Latest: latest})
	return outdatedContainerHint(installed, latest)
}

func (c containerUpdateConfig) cacheFile() (string, error) {
	if c.CacheFile != "" {
		return c.CacheFile, nil
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "yoho", "container-latest.json"), nil
}

func (c containerUpdateConfig) apiURL() string {
	if c.APIURL != "" {
		return c.APIURL
	}
	return containerReleaseAPI
}

func (c containerUpdateConfig) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 2 * time.Second
}

func outdatedContainerHint(installed, latest string) string {
	if latest == "" || !VersionLess(installed, latest) {
		return ""
	}
	return fmt.Sprintf("Apple container %s is outdated (latest %s); run `yoho builder setup` to upgrade (needs sudo)", installed, latest)
}

func installedContainerVersion(ctx context.Context, ex Exec) (string, bool) {
	if ex == nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var out bytes.Buffer
	err := ex(ctx, Command{Argv: []string{"container", "--version"}, Stdout: &out, Stderr: io.Discard})
	if err != nil {
		return "", false
	}
	v := containerVersionRE.FindString(out.String())
	return v, v != ""
}

func readFreshContainerCache(path string, now time.Time) (string, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var c containerLatestCache
	if json.Unmarshal(b, &c) != nil || c.Latest == "" || c.CheckedAt.IsZero() {
		return "", false
	}
	if now.Sub(c.CheckedAt) >= containerUpdateTTL {
		return "", false
	}
	return c.Latest, true
}

func writeContainerCache(path string, c containerLatestCache) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(dir, ".container-latest-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

func fetchLatestContainer(ctx context.Context, cfg containerUpdateConfig) (string, error) {
	client := cfg.Client
	if client == nil {
		client = http.DefaultClient
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.timeout())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.apiURL(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "yoho")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github api: %s", resp.Status)
	}
	var r struct {
		Tag string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r); err != nil {
		return "", err
	}
	latest := strings.TrimPrefix(strings.TrimSpace(r.Tag), "v")
	if latest == "" {
		return "", errors.New("empty release tag")
	}
	return latest, nil
}

// noteContainerUpdate prints one hint when this build uses Apple container and
// the install is older than the latest release. Network errors and a 2s
// timeout are ignored. YOHO_NO_UPDATE_CHECK=1 disables the check. --json emits
// a warning event on stdout; human mode prints the same hint through ui.
// Output goes to stdout, not the build step writer: that writer stays quiet
// unless the command is verbose, and a hint must be visible either way.
func noteContainerUpdate(ctx context.Context, ex Exec) {
	msg := containerUpdateHint(ctx, containerUpdateConfig{Exec: ex})
	if msg == "" {
		return
	}
	writeUpdateHint(os.Stdout, cliJSON(), msg)
}

func writeUpdateHint(w io.Writer, jsonMode bool, msg string) {
	if w == nil || msg == "" {
		return
	}
	mode := ui.Human
	if jsonMode {
		mode = ui.JSON
	}
	ui.New(w, mode, false).Warn("%s", msg)
}

func cliJSON() bool {
	for _, a := range os.Args[1:] {
		if a == "--" {
			break
		}
		name, val, hasVal := strings.Cut(a, "=")
		if name != "--json" {
			continue
		}
		if !hasVal {
			return true
		}
		b, err := strconv.ParseBool(val)
		return err == nil && b
	}
	return false
}
