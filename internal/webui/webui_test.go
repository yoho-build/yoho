package webui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/yoho-build/yoho/internal/dash"
)

type fakeData struct{ logsFor []string }

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

func client(t *testing.T) *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar}
}

func get(t *testing.T, c *http.Client, u string) (*http.Response, string) {
	t.Helper()
	res, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

func TestLoopbackListener(t *testing.T) {
	_, login, _ := start(t)
	u, _ := url.Parse(login)
	if !strings.HasPrefix(u.Host, "127.0.0.1:") {
		t.Fatalf("must listen on loopback: %s", u.Host)
	}
}

func TestLoginIsSingleUseAndGuardsAPI(t *testing.T) {
	_, login, _ := start(t)
	base := strings.SplitN(login, "/login", 2)[0]
	anon := client(t)
	if res, body := get(t, anon, base+"/api/snapshot"); res.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "login link") {
		t.Fatalf("API without session: %d %s", res.StatusCode, body)
	}
	if res, _ := get(t, anon, base+"/login?code=wrong"); res.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong code: %d", res.StatusCode)
	}

	user := client(t)
	res, body := get(t, user, login)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "<title>Yoho") {
		t.Fatalf("login should redirect to the app: %d %s", res.StatusCode, body)
	}
	cookies := user.Jar.Cookies(res.Request.URL)
	if len(cookies) != 1 || cookies[0].Name != cookieName {
		t.Fatalf("session cookie: %v", cookies)
	}
	res, body = get(t, user, base+"/api/snapshot")
	var snap dash.Snapshot
	if res.StatusCode != 200 || json.Unmarshal([]byte(body), &snap) != nil || snap.App != "shop" {
		t.Fatalf("snapshot: %d %s", res.StatusCode, body)
	}
	if cc := res.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("API responses must not be cached: %q", cc)
	}

	// The code is consumed: another browser cannot reuse it.
	if res, _ := get(t, client(t), login); res.StatusCode != http.StatusForbidden {
		t.Fatalf("second use of the login link: %d", res.StatusCode)
	}
	// The signed-in browser revisiting the link is sent to the app.
	if res, _ := get(t, user, login); res.StatusCode != http.StatusOK {
		t.Fatalf("revisit with session: %d", res.StatusCode)
	}
}

func TestSetCookieAttributes(t *testing.T) {
	_, login, _ := start(t)
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, _ := get(t, c, login)
	sc := res.Header.Get("Set-Cookie")
	for _, want := range []string{"HttpOnly", "SameSite=Strict", "Path=/"} {
		if !strings.Contains(sc, want) {
			t.Errorf("Set-Cookie %q lacks %s", sc, want)
		}
	}
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/" {
		t.Errorf("redirect: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
}

func TestRejectsForeignHostAndWrites(t *testing.T) {
	_, login, _ := start(t)
	u, _ := url.Parse(login)
	_, port, _ := strings.Cut(u.Host, ":")
	for _, host := range []string{"evil.example:" + port, "127.0.0.1.nip.io:" + port, "127.0.0.1:1", "localhost"} {
		req, _ := http.NewRequest("GET", "http://"+u.Host+"/login?"+u.RawQuery, nil)
		req.Host = host
		res, err := http.DefaultClient.Do(req)
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

func TestStaticAndSecurityHeaders(t *testing.T) {
	_, login, _ := start(t)
	base := strings.SplitN(login, "/login", 2)[0]
	c := client(t)
	res, body := get(t, c, base+"/assets/app.js")
	if res.StatusCode != 200 || body != "console.log(1)" || !strings.Contains(res.Header.Get("Cache-Control"), "immutable") {
		t.Errorf("asset: %d %q %v", res.StatusCode, body, res.Header)
	}
	res, body = get(t, c, base+"/some/client/route")
	if res.StatusCode != 200 || !strings.Contains(body, "<title>Yoho") {
		t.Errorf("SPA fallback: %d %s", res.StatusCode, body)
	}
	res, body = get(t, c, base+"/../../etc/passwd")
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
	_, login, data := start(t)
	base := strings.SplitN(login, "/login", 2)[0]
	c := client(t)
	get(t, c, login)
	res, body := get(t, c, base+"/api/logs?server=web-1&container=yoho-proxy&lines=10")
	if res.StatusCode != 200 || !strings.Contains(body, "hello") {
		t.Fatalf("logs: %d %s", res.StatusCode, body)
	}
	res, _ = get(t, c, base+"/api/logs?server=web-1&container=postgres")
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
