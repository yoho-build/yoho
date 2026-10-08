package dist

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
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

	mux := http.NewServeMux()
	mux.HandleFunc("/"+version+"/"+asset, func(w http.ResponseWriter, r *http.Request) {
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
	if err == nil || err.Error() != "release v9.9.9 has no asset yoho-linux-amd64" {
		t.Fatalf("404: %v", err)
	}

	_, err = Download(context.Background(), "v0.0.1", "linux/amd64", cache)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("mismatch: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(cache, "v0.0.1", asset)); !os.IsNotExist(statErr) {
		t.Fatalf("mismatch left a file: %v", statErr)
	}
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
