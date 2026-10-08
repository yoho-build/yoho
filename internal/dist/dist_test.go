package dist

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAssetName(t *testing.T) {
	got, err := AssetName("linux/amd64")
	if err != nil || got != "yoho-linux-amd64" {
		t.Fatalf("AssetName: %q %v", got, err)
	}
	if _, err := AssetName("windows/amd64"); err == nil {
		t.Fatal("want unsupported OS")
	}
}

func TestDownloadDevBuild(t *testing.T) {
	prev := ReleaseBase
	ReleaseBase = "http://127.0.0.1:1"
	t.Cleanup(func() { ReleaseBase = prev })
	for _, v := range []string{"dev", ""} {
		_, err := Download(context.Background(), v, "linux/amd64", t.TempDir())
		if !errors.Is(err, ErrDevBuild) {
			t.Fatalf("version %q: %v", v, err)
		}
	}
}

func TestDownloadSuccessMismatch404AndCache(t *testing.T) {
	const version = "v0.1.0"
	const asset = "yoho-linux-amd64"
	payload := []byte("yoho-binary-bytes")
	sum := sha256.Sum256(payload)
	good := hex.EncodeToString(sum[:])
	var assetHits atomic.Int32
	useToken(t, "")

	mux := http.NewServeMux()
	mux.HandleFunc("/"+version+"/"+asset, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("public download sent Authorization")
		}
		assetHits.Add(1)
		_, _ = w.Write(payload)
	})
	mux.HandleFunc("/"+version+"/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(good + "  " + asset + "\n"))
	})
	mux.HandleFunc("/v9.9.9/"+asset, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.HandleFunc("/v9.9.9/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(good + "  " + asset + "\n"))
	})
	mux.HandleFunc("/v0.0.1/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("0000000000000000000000000000000000000000000000000000000000000000  " + asset + "\n"))
	})
	mux.HandleFunc("/v0.0.1/"+asset, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	})
	mux.HandleFunc("/v0.0.2/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(good + "  yoho-linux-arm64\n"))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	prev := ReleaseBase
	ReleaseBase = srv.URL
	t.Cleanup(func() { ReleaseBase = prev })

	cache := t.TempDir()
	path, err := Download(context.Background(), version, "linux/amd64", cache)
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(cache, version, asset)
	if path != wantPath {
		t.Fatalf("path %s", path)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != string(payload) {
		t.Fatalf("cached %q %v", b, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v %v", info, err)
	}

	if _, err := Download(context.Background(), version, "linux/amd64", cache); err != nil {
		t.Fatal(err)
	}
	if n := assetHits.Load(); n != 1 {
		t.Fatalf("asset downloads = %d, want 1 (cache reuse)", n)
	}

	_, err = Download(context.Background(), "v9.9.9", "linux/amd64", cache)
	const hint = "release v9.9.9 not found; if the repo is private set GITHUB_TOKEN or log in with gh"
	if err == nil || err.Error() != hint {
		t.Fatalf("404: %v", err)
	}

	_, err = Download(context.Background(), "v0.0.2", "linux/amd64", cache)
	if err == nil || err.Error() != "release v0.0.2 has no asset yoho-linux-amd64" {
		t.Fatalf("missing asset: %v", err)
	}

	_, err = Download(context.Background(), "v0.0.1", "linux/amd64", cache)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("mismatch: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(cache, "v0.0.1", asset)); !os.IsNotExist(statErr) {
		t.Fatalf("mismatch left a file: %v", statErr)
	}
}

func TestTokenAPIRedirectStripsAuthorization(t *testing.T) {
	const version = "v1.2.3"
	const asset = "yoho-linux-amd64"
	const token = "test-token"
	payload := []byte("yoho-binary-bytes")
	sum := sha256.Sum256(payload)
	good := hex.EncodeToString(sum[:])
	checksumFile := good + "  " + asset + "\n"
	useToken(t, token)

	var apiURL string
	// [::1] is a different host from the API's 127.0.0.1, and it is loopback.
	cdn := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("authorization forwarded to other host: %q", got)
		}
		switch r.URL.Path {
		case "/bin":
			_, _ = w.Write(payload)
		default:
			http.NotFound(w, r)
		}
	}))
	if err := cdn.Listener.Close(); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatal(err)
	}
	cdn.Listener = ln
	cdn.Start()
	t.Cleanup(cdn.Close)
	cdnURL := cdn.URL

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+Repo+"/releases/tags/"+version, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("release auth %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Accept") != "application/vnd.github+json" {
			t.Errorf("release accept %q", r.Header.Get("Accept"))
		}
		fmt.Fprintf(w, `{"assets":[{"name":"checksums.txt","url":"%s/assets/checksums"},{"name":"%s","url":"%s/assets/bin"}]}`, apiURL, asset, apiURL)
	})
	mux.HandleFunc("/assets/checksums", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("checksum auth %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Accept") != "application/octet-stream" {
			t.Errorf("checksum accept %q", r.Header.Get("Accept"))
		}
		http.Redirect(w, r, apiURL+"/final-checksums", http.StatusFound)
	})
	mux.HandleFunc("/final-checksums", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("same-host redirect dropped Authorization: %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(checksumFile))
	})
	mux.HandleFunc("/assets/bin", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("asset auth %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Accept") != "application/octet-stream" {
			t.Errorf("asset accept %q", r.Header.Get("Accept"))
		}
		http.Redirect(w, r, cdnURL+"/bin", http.StatusFound)
	})
	mux.HandleFunc("/repos/"+Repo+"/releases/tags/v0.0.2", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"assets":[{"name":"checksums.txt","url":"%s/assets/checksums"}]}`, apiURL)
	})
	mux.HandleFunc("/repos/"+Repo+"/releases/tags/v0.0.3", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"assets":[{"name":"checksums.txt","url":"%s/assets/checksums"},{"name":"%s","url":"%s/assets/bad"}]}`, apiURL, asset, apiURL)
	})
	mux.HandleFunc("/assets/bad", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not-the-binary"))
	})
	mux.HandleFunc("/repos/"+Repo+"/releases/tags/v9.9.9", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.HandleFunc("/repos/"+Repo+"/releases/tags/v8.8.8", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(token))
	})

	api := httptest.NewServer(mux)
	t.Cleanup(api.Close)
	apiURL = api.URL
	prev := APIBase
	APIBase = api.URL
	t.Cleanup(func() { APIBase = prev })

	cache := t.TempDir()
	path, err := Download(context.Background(), version, "linux/amd64", cache)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != string(payload) {
		t.Fatalf("cached %q %v", b, err)
	}
	if strings.Contains(path, token) {
		t.Fatalf("token in path")
	}

	_, err = Download(context.Background(), "v0.0.2", "linux/amd64", cache)
	if err == nil || err.Error() != "release v0.0.2 has no asset "+asset {
		t.Fatalf("missing asset: %v", err)
	}
	if err != nil && strings.Contains(err.Error(), token) {
		t.Fatalf("token leaked: %v", err)
	}

	_, err = Download(context.Background(), "v0.0.3", "linux/amd64", cache)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("mismatch: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(cache, "v0.0.3", asset)); !os.IsNotExist(statErr) {
		t.Fatalf("mismatch left a file: %v", statErr)
	}

	_, err = Download(context.Background(), "v9.9.9", "linux/amd64", cache)
	if err == nil || err.Error() != "release v9.9.9 not found" {
		t.Fatalf("api 404: %v", err)
	}

	_, err = Download(context.Background(), "v8.8.8", "linux/amd64", cache)
	if err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("token leaked in error: %v", err)
	}
}

func TestDiscoverToken(t *testing.T) {
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "ran")
	gh := filepath.Join(dir, "gh")
	script := "#!/bin/sh\nprintf 'x\\n' >> \"$SENTINEL\"\nprintf '%s\\n' \"$GH_OUT\"\nexit \"${GH_EXIT:-0}\"\n"
	if err := os.WriteFile(gh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("SENTINEL", sentinel)
	t.Setenv("GH_OUT", "token-from-gh")
	t.Setenv("GH_EXIT", "0")

	t.Setenv("GITHUB_TOKEN", "from-github")
	t.Setenv("GH_TOKEN", "from-gh-env")
	if got := discoverToken(context.Background()); got != "from-github" {
		t.Fatalf("GITHUB_TOKEN precedence: %q", got)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatal("gh ran even though GITHUB_TOKEN was set")
	}

	t.Setenv("GITHUB_TOKEN", "")
	if got := discoverToken(context.Background()); got != "from-gh-env" {
		t.Fatalf("GH_TOKEN precedence: %q", got)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatal("gh ran even though GH_TOKEN was set")
	}

	t.Setenv("GH_TOKEN", "")
	if got := discoverToken(context.Background()); got != "token-from-gh" {
		t.Fatalf("gh auth token: %q", got)
	}

	t.Setenv("GH_EXIT", "1")
	t.Setenv("GH_OUT", "should-not-use")
	if got := discoverToken(context.Background()); got != "" {
		t.Fatalf("gh failure: %q", got)
	}

	slow := "#!/bin/sh\nexec /bin/sleep 30\n"
	if err := os.WriteFile(gh, []byte(slow), 0o755); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if got := discoverToken(context.Background()); got != "" {
		t.Fatalf("slow gh: %q", got)
	}
	if elapsed := time.Since(start); elapsed > 8*time.Second {
		t.Fatalf("gh auth token timed out too slowly: %s", elapsed)
	}
}

func useToken(t *testing.T, token string) {
	t.Helper()
	prev := currentToken
	currentToken = func(context.Context) string { return token }
	t.Cleanup(func() { currentToken = prev })
}

func TestInstallScriptSyntax(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	script := filepath.Join(filepath.Dir(file), "..", "..", "scripts", "install.sh")
	out, err := exec.Command("sh", "-n", script).CombinedOutput()
	if err != nil {
		t.Fatalf("sh -n: %v\n%s", err, out)
	}
}

// is_writable_dir must accept a directory whose parents do not exist yet when
// the nearest existing ancestor is writable (a fresh $HOME has no ~/.local),
// and refuse one under a read-only ancestor.
func TestInstallScriptWritableDirWalksToExistingAncestor(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	script := filepath.Join(filepath.Dir(file), "..", "..", "scripts", "install.sh")
	src, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	start := strings.Index(text, "is_writable_dir() {")
	end := strings.Index(text[start:], "\n}\n")
	if start < 0 || end < 0 {
		t.Fatal("is_writable_dir not found")
	}
	fn := text[start : start+end+3]
	home := t.TempDir()
	ro := filepath.Join(home, "ro")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(ro, 0o700)
	for dir, want := range map[string]bool{
		filepath.Join(home, ".local", "bin"): true,
		home:                                 true,
		filepath.Join(ro, "a", "b"):          os.Getuid() == 0,
		"/nonexistent-root-dir/x/y":          os.Getuid() == 0,
	} {
		err := exec.Command("sh", "-c", fn+"\nis_writable_dir \"$1\"", "sh", dir).Run()
		if (err == nil) != want {
			t.Errorf("is_writable_dir %s = %v, want %v", dir, err == nil, want)
		}
	}
}
