package build

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestVersionLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"1.5.0", "1.10.0", true},
		{"1.10.0", "1.5.0", false},
		{"1.10.0", "1.10.0", false},
		{"v1.2.0", "1.2.1", true},
		{"1.2", "1.2.0", false},
		{"1.2.0", "1.2", false},
		{"1.9.0", "1.10.0", true},
	}
	for _, c := range cases {
		if got := VersionLess(c.a, c.b); got != c.want {
			t.Errorf("VersionLess(%s, %s) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestContainerUpdateHint(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	release := func(tag string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Accept") == "" {
				t.Errorf("missing Accept header")
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": tag})
		}
	}

	t.Run("outdated fetches and caches", func(t *testing.T) {
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			release("v1.10.0").ServeHTTP(w, r)
		}))
		defer srv.Close()
		cache := filepath.Join(t.TempDir(), "yoho", "container-latest.json")
		cfg := containerUpdateConfig{
			Installed: "1.5.0",
			APIURL:    srv.URL,
			CacheFile: cache,
			Now:       func() time.Time { return now },
			Getenv:    func(string) string { return "" },
			Timeout:   time.Second,
		}
		msg := containerUpdateHint(context.Background(), cfg)
		if !strings.Contains(msg, "run `yoho builder setup` to upgrade (needs sudo)") || !strings.Contains(msg, "1.5.0") || !strings.Contains(msg, "1.10.0") {
			t.Fatalf("hint %q", msg)
		}
		b, err := os.ReadFile(cache)
		if err != nil {
			t.Fatal(err)
		}
		var got containerLatestCache
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		if got.Latest != "1.10.0" || !got.CheckedAt.Equal(now) {
			t.Fatalf("cache %+v", got)
		}
		if containerUpdateHint(context.Background(), cfg) == "" || hits.Load() != 1 {
			t.Fatalf("second check hit network %d times, hint missing", hits.Load())
		}
	})

	t.Run("current version is silent", func(t *testing.T) {
		srv := httptest.NewServer(release("v1.5.0"))
		defer srv.Close()
		msg := containerUpdateHint(context.Background(), containerUpdateConfig{
			Installed: "1.10.0",
			APIURL:    srv.URL,
			CacheFile: filepath.Join(t.TempDir(), "container-latest.json"),
			Now:       func() time.Time { return now },
			Getenv:    func(string) string { return "" },
		})
		if msg != "" {
			t.Fatalf("hint %q", msg)
		}
	})

	t.Run("fresh cache skips the network", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Error("network used")
		}))
		defer srv.Close()
		dir := t.TempDir()
		cache := filepath.Join(dir, "container-latest.json")
		if err := writeContainerCache(cache, containerLatestCache{CheckedAt: now.Add(-time.Hour), Latest: "1.10.0"}); err != nil {
			t.Fatal(err)
		}
		msg := containerUpdateHint(context.Background(), containerUpdateConfig{
			Installed: "1.0.0",
			APIURL:    srv.URL,
			CacheFile: cache,
			Now:       func() time.Time { return now },
			Getenv:    func(string) string { return "" },
		})
		if !strings.Contains(msg, "latest 1.10.0") {
			t.Fatalf("hint %q", msg)
		}
	})

	t.Run("stale cache refetches", func(t *testing.T) {
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			release("v1.8.0").ServeHTTP(w, r)
		}))
		defer srv.Close()
		cache := filepath.Join(t.TempDir(), "container-latest.json")
		if err := writeContainerCache(cache, containerLatestCache{CheckedAt: now.Add(-25 * time.Hour), Latest: "1.2.0"}); err != nil {
			t.Fatal(err)
		}
		msg := containerUpdateHint(context.Background(), containerUpdateConfig{
			Installed: "1.2.0",
			APIURL:    srv.URL,
			CacheFile: cache,
			Now:       func() time.Time { return now },
			Getenv:    func(string) string { return "" },
		})
		if hits.Load() != 1 || !strings.Contains(msg, "latest 1.8.0") {
			t.Fatalf("hits %d hint %q", hits.Load(), msg)
		}
	})

	t.Run("network failure is silent", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()
		cache := filepath.Join(t.TempDir(), "container-latest.json")
		msg := containerUpdateHint(context.Background(), containerUpdateConfig{
			Installed: "1.0.0",
			APIURL:    srv.URL,
			CacheFile: cache,
			Now:       func() time.Time { return now },
			Getenv:    func(string) string { return "" },
		})
		if msg != "" {
			t.Fatalf("hint %q", msg)
		}
		if _, err := os.Stat(cache); !os.IsNotExist(err) {
			t.Fatalf("cache err %v", err)
		}
	})

	t.Run("timeout is silent", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		}))
		defer srv.Close()
		start := time.Now()
		msg := containerUpdateHint(context.Background(), containerUpdateConfig{
			Installed: "1.0.0",
			APIURL:    srv.URL,
			CacheFile: filepath.Join(t.TempDir(), "container-latest.json"),
			Now:       func() time.Time { return now },
			Getenv:    func(string) string { return "" },
			Timeout:   50 * time.Millisecond,
		})
		if msg != "" || time.Since(start) > time.Second {
			t.Fatalf("hint %q after %s", msg, time.Since(start))
		}
	})

	t.Run("env disables the check", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Error("network used")
		}))
		defer srv.Close()
		msg := containerUpdateHint(context.Background(), containerUpdateConfig{
			Installed: "1.0.0",
			APIURL:    srv.URL,
			CacheFile: filepath.Join(t.TempDir(), "container-latest.json"),
			Getenv: func(k string) string {
				if k == "YOHO_NO_UPDATE_CHECK" {
					return "1"
				}
				return ""
			},
		})
		if msg != "" {
			t.Fatalf("hint %q", msg)
		}
	})

	t.Run("reads container --version", func(t *testing.T) {
		srv := httptest.NewServer(release("v1.5.0"))
		defer srv.Close()
		ex := func(ctx context.Context, c Command) error {
			if strings.Join(c.Argv, " ") != "container --version" {
				t.Fatalf("argv %v", c.Argv)
			}
			_, _ = c.Stdout.Write([]byte("container CLI version 1.0.0 (build: abc)\n"))
			return nil
		}
		msg := containerUpdateHint(context.Background(), containerUpdateConfig{
			Exec:      ex,
			APIURL:    srv.URL,
			CacheFile: filepath.Join(t.TempDir(), "container-latest.json"),
			Now:       func() time.Time { return now },
			Getenv:    func(string) string { return "" },
		})
		if !strings.Contains(msg, "Apple container 1.0.0 is outdated") {
			t.Fatalf("hint %q", msg)
		}
	})
}

func TestWriteUpdateHint(t *testing.T) {
	var b strings.Builder
	writeUpdateHint(&b, false, "Apple container 1.0.0 is outdated (latest 1.5.0); run `yoho builder setup` to upgrade (needs sudo)")
	if !strings.Contains(b.String(), "! Apple container 1.0.0 is outdated") {
		t.Fatalf("human %q", b.String())
	}
	b.Reset()
	writeUpdateHint(&b, true, "run `yoho builder setup` to upgrade (needs sudo)")
	var ev map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(b.String())), &ev); err != nil {
		t.Fatal(err)
	}
	if ev["event"] != "warning" || !strings.Contains(ev["message"].(string), "yoho builder setup") {
		t.Fatalf("event %#v", ev)
	}
}
