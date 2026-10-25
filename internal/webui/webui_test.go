package webui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/yoho-build/yoho/internal/dash"
)

type fakeData struct {
	logsFor   []string
	diskFresh []bool
}

func (f *fakeData) Disk(_ context.Context, fresh bool) (*dash.DiskUsage, error) {
	f.diskFresh = append(f.diskFresh, fresh)
	return &dash.DiskUsage{Servers: []dash.ServerDisk{{Name: "web-1", Access: "sudo", Volumes: []dash.Volume{{Name: "yoho-shop-production_pgdata", Sized: true, Bytes: 1 << 30}}}}}, nil
}

func (f *fakeData) Snapshot(context.Context, bool) (*dash.Snapshot, error) {
	return &dash.Snapshot{App: "shop", Destination: "production", Servers: []dash.Server{{Name: "web-1"}}}, nil
}

func (f *fakeData) Logs(_ context.Context, server, name string, lines int) (string, error) {
	f.logsFor = append(f.logsFor, server+"/"+name)
	if name != "yoho-proxy" {
		return "", errors.New("unknown container")
	}
	return "hello", nil
}

func start(t *testing.T) (*Server, string, *fakeData) {
	t.Helper()
	data := &fakeData{}
	assets := fstest.MapFS{
		"index.html":       {Data: []byte("<!doctype html><title>Yoho</title>")},
		"assets/app.js":    {Data: []byte("console.log(1)")},
		"assets/style.css": {Data: []byte("body{}")},
	}
	s := New(data, Meta{Version: "test", Interval: 5}, assets)
	ln, err := Listen(0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Serve did not stop")
		}
	})
	return s, s.LoginURL(ln.Addr()), data
}

func noRedirect() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func get(t *testing.T, u, token string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", u, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := noRedirect().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

// login redeems the link and returns the session token from the redirect.
func login(t *testing.T, link string) string {
	t.Helper()
	res, _ := get(t, link, "")
	tok, ok := strings.CutPrefix(res.Header.Get("Location"), "/#session=")
	if res.StatusCode != http.StatusSeeOther || !ok || len(tok) != 64 {
		t.Fatalf("login: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	if res.Header.Get("Set-Cookie") != "" {
		t.Error("no cookies: they are shared by every port of 127.0.0.1")
	}
	return tok
}

func TestLoopbackListener(t *testing.T) {
	_, link, _ := start(t)
	u, _ := url.Parse(link)
	if !strings.HasPrefix(u.Host, "127.0.0.1:") {
		t.Fatalf("must listen on loopback: %s", u.Host)
	}
}

func TestLoginIsSingleUseAndGuardsAPI(t *testing.T) {
	_, link, _ := start(t)
	base := strings.SplitN(link, "/login", 2)[0]
	if res, body := get(t, base+"/api/snapshot", ""); res.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "login link") {
		t.Fatalf("API without session: %d %s", res.StatusCode, body)
	}
	if res, _ := get(t, base+"/login?code=wrong", ""); res.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong code: %d", res.StatusCode)
	}
	tok := login(t, link)
	if res, _ := get(t, base+"/api/snapshot", "x"+tok[1:]); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", res.StatusCode)
	}
	res, body := get(t, base+"/api/snapshot", tok)
	var snap dash.Snapshot
	if res.StatusCode != 200 || json.Unmarshal([]byte(body), &snap) != nil || snap.App != "shop" {
		t.Fatalf("snapshot: %d %s", res.StatusCode, body)
	}
	if cc := res.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("API responses must not be cached: %q", cc)
	}
	// The code is consumed.
	if res, _ := get(t, link, ""); res.StatusCode != http.StatusForbidden {
		t.Fatalf("second use of the login link: %d", res.StatusCode)
	}
}

func TestRejectsForeignHostAndWrites(t *testing.T) {
	_, link, _ := start(t)
	u, _ := url.Parse(link)
	_, port, _ := strings.Cut(u.Host, ":")
	for _, host := range []string{"evil.example:" + port, "127.0.0.1.nip.io:" + port, "127.0.0.1:1", "localhost", "127.0.0.1"} {
		req, _ := http.NewRequest("GET", "http://"+u.Host+"/login?"+u.RawQuery, nil)
		req.Host = host
		res, err := noRedirect().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("Host %q: %d, want 403", host, res.StatusCode)
		}
	}
	for _, host := range []string{"localhost:" + port, "[::1]:" + port} {
		req, _ := http.NewRequest("GET", "http://"+u.Host+"/", nil)
		req.Host = host
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("Host %q: %d, want 200", host, res.StatusCode)
		}
	}
	res, err := http.Post("http://"+u.Host+"/api/snapshot", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d, want 405", res.StatusCode)
	}
}

func TestPort80HostWithoutPort(t *testing.T) {
	s := New(&fakeData{}, Meta{}, fstest.MapFS{})
	s.port = "80"
	for host, want := range map[string]bool{"127.0.0.1": true, "localhost": true, "[::1]": true, "127.0.0.1:80": true, "evil.example": false, "127.0.0.1:81": false} {
		if got := s.loopbackHost(host); got != want {
			t.Errorf("%q: %v, want %v", host, got, want)
		}
	}
	s.port = "8080"
	if s.loopbackHost("127.0.0.1") {
		t.Error("a missing port means 80")
	}
}

func TestStaticAndSecurityHeaders(t *testing.T) {
	_, link, _ := start(t)
	base := strings.SplitN(link, "/login", 2)[0]
	res, body := get(t, base+"/assets/app.js", "")
	if res.StatusCode != 200 || body != "console.log(1)" || !strings.Contains(res.Header.Get("Cache-Control"), "immutable") {
		t.Errorf("asset: %d %q %v", res.StatusCode, body, res.Header)
	}
	res, body = get(t, base+"/some/client/route", "")
	if res.StatusCode != 200 || !strings.Contains(body, "<title>Yoho") {
		t.Errorf("SPA fallback: %d %s", res.StatusCode, body)
	}
	res, body = get(t, base+"/../../etc/passwd", "")
	if strings.Contains(body, "root:") || res.StatusCode != 200 && res.StatusCode != 400 {
		t.Errorf("traversal: %d %s", res.StatusCode, body)
	}
	for _, h := range []string{"Content-Security-Policy", "X-Frame-Options", "X-Content-Type-Options", "Referrer-Policy"} {
		if res.Header.Get(h) == "" {
			t.Errorf("missing %s", h)
		}
	}
}

func TestLogsAPI(t *testing.T) {
	_, link, data := start(t)
	base := strings.SplitN(link, "/login", 2)[0]
	tok := login(t, link)
	res, body := get(t, base+"/api/logs?server=web-1&container=yoho-proxy&lines=10", tok)
	if res.StatusCode != 200 || !strings.Contains(body, "hello") {
		t.Fatalf("logs: %d %s", res.StatusCode, body)
	}
	res, _ = get(t, base+"/api/logs?server=web-1&container=postgres", tok)
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("unowned container: %d", res.StatusCode)
	}
	if strings.Join(data.logsFor, ",") != "web-1/yoho-proxy,web-1/postgres" {
		t.Errorf("logs calls: %v", data.logsFor)
	}
}

func TestEmbeddedAssets(t *testing.T) {
	b, err := Assets().(interface {
		ReadFile(string) ([]byte, error)
	}).ReadFile("index.html")
	if err != nil {
		t.Fatalf("index.html must be embedded (run `npm --prefix web run build`): %v", err)
	}
	if !strings.Contains(string(b), "/assets/") {
		t.Errorf("index.html does not reference built assets:\n%s", b)
	}
}

func TestDiskAPI(t *testing.T) {
	_, link, data := start(t)
	base := strings.SplitN(link, "/login", 2)[0]
	if res, _ := get(t, base+"/api/disk", ""); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("disk without session: %d", res.StatusCode)
	}
	tok := login(t, link)
	res, body := get(t, base+"/api/disk", tok)
	var du dash.DiskUsage
	if res.StatusCode != 200 || json.Unmarshal([]byte(body), &du) != nil || du.Servers[0].Volumes[0].Bytes != 1<<30 {
		t.Fatalf("disk: %d %s", res.StatusCode, body)
	}
	get(t, base+"/api/disk?fresh=1", tok)
	if len(data.diskFresh) != 2 || data.diskFresh[0] || !data.diskFresh[1] {
		t.Errorf("fresh must pass through: %v", data.diskFresh)
	}
}
