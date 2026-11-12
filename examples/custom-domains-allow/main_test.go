package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type sourceFunc func(context.Context, string) (bool, error)

func (f sourceFunc) IsVerified(c context.Context, h string) (bool, error) { return f(c, h) }
func TestNormalize(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Customer.Example.COM.", "customer.example.com"}, {"xn--bcher-kva.example", "xn--bcher-kva.example"},
		{"", ""}, {"127.0.0.1", ""}, {"[::1]", ""}, {"::1", ""}, {"example.com:443", ""}, {"a..b", ""},
		{"-a.com", ""}, {"a-.com", ""}, {"a_b.com", ""}, {"a.com..", ""}, {"é.com", ""}, {" a.com", ""},
		{strings.Repeat("a", 64) + ".com", ""},
		{strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 62), ""},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := normalize(tc.in)
			if tc.want == "" {
				if err == nil {
					t.Fatal("accepted invalid hostname")
				}
			} else if err != nil || got != tc.want {
				t.Fatalf("%q %v", got, err)
			}
		})
	}
}
func TestHandler(t *testing.T) {
	for _, tc := range []struct {
		name, method, host string
		allow              bool
		err                error
		code               int
	}{
		{"allow", "GET", "EXAMPLE.COM.", true, nil, 200}, {"deny", "GET", "example.com", false, nil, 403},
		{"source error", "GET", "example.com", false, errors.New("offline"), 503},
		{"invalid", "GET", "example.com:443", true, nil, 400}, {"method", "POST", "example.com", true, nil, 405},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &handler{source: sourceFunc(func(ctx context.Context, host string) (bool, error) {
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 1500*time.Millisecond {
					t.Error("missing deadline")
				}
				if host != "example.com" {
					t.Errorf("not normalized: %q", host)
				}
				return tc.allow, tc.err
			}), now: time.Now}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(tc.method, "/custom-domains/allow?host="+tc.host, nil))
			if w.Code != tc.code {
				t.Fatalf("got %d want %d", w.Code, tc.code)
			}
		})
	}
}
func TestCache(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		t.Run(map[bool]string{true: "positive", false: "negative"}[allowed], func(t *testing.T) {
			now := time.Unix(0, 0)
			calls := 0
			fail := false
			h := &handler{now: func() time.Time { return now }, source: sourceFunc(func(context.Context, string) (bool, error) {
				calls++
				if fail {
					return false, errors.New("offline")
				}
				return allowed, nil
			})}
			ctx := context.Background()
			for range 2 {
				if _, err := h.check(ctx, "a.com"); err != nil {
					t.Fatal(err)
				}
			}
			if calls != 1 {
				t.Fatal(calls)
			}
			ttl := 30 * time.Second
			if allowed {
				ttl = 5 * time.Minute
			}
			now = now.Add(ttl)
			fail = true
			for range 2 {
				if _, err := h.check(ctx, "a.com"); err == nil {
					t.Fatal("cached source error")
				}
			}
			if calls != 3 {
				t.Fatal(calls)
			}
		})
	}
}
func TestFileReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	ctx := context.Background()
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("EXAMPLE.COM.\n")
	f := &fileVerified{path: path}
	if ok, err := f.IsVerified(ctx, "example.com"); !ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	write("other.example.com\n")
	if ok, err := f.IsVerified(ctx, "example.com"); ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	write("third.example.com\n")
	if err := f.reload(ctx, true); err != nil {
		t.Fatal(err)
	}
	if ok, err := f.IsVerified(ctx, "third.example.com"); !ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := f.IsVerified(ctx, "third.example.com"); err == nil {
		t.Fatal("missing file allowed")
	}
}
func TestCacheBound(t *testing.T) {
	h := &handler{now: time.Now, cache: make(map[string]entry), source: sourceFunc(func(context.Context, string) (bool, error) { return true, nil })}
	for i := range 10000 {
		h.cache[string(rune(i))] = entry{true, time.Now().Add(time.Hour)}
	}
	if _, err := h.check(context.Background(), "extra.example"); err != nil {
		t.Fatal(err)
	}
	if len(h.cache) > 10000 {
		t.Fatal("unbounded cache")
	}
}
func TestTimeout(t *testing.T) {
	h := &handler{now: time.Now, source: sourceFunc(func(ctx context.Context, _ string) (bool, error) { <-ctx.Done(); return false, ctx.Err() })}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/?host=example.com", nil))
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
}

func TestMuxRejectsUnknownAllowPaths(t *testing.T) {
	mux := newMux(sourceFunc(func(context.Context, string) (bool, error) {
		t.Error("unexpected allow check")
		return true, nil
	}))
	for _, path := range []string{"/check", "/allow", "/custom-domains/allow/"} {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest("GET", path+"?host=evil.example", nil))
			if w.Code != 404 {
				t.Fatalf("got %d, want 404", w.Code)
			}
		})
	}
}
