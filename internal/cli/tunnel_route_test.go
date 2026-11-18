package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/migrate"
	"github.com/yoho-build/yoho/internal/migrate/migratetest"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/proxy"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/secrets"
	"github.com/yoho-build/yoho/internal/ui"
)

const routeIngress = `{"ingress":[` +
	`{"hostname":"app.example.com","path":"/ws","service":"http://sync:8080"},` +
	`{"hostname":"app.example.com","service":"http://web:3000","originRequest":{"noTLSVerify":true}},` +
	`{"hostname":"other.example.com","service":"http://other:80"},` +
	`{"service":"http_status:404"}],"warp-routing":{"enabled":false}}`

const routeList = `{"shop-production-web":{"hosts":["app.example.com"],"path_prefixes":["/"],"targets":["c1:3000"],"state":"running"},` +
	`"blog-production-web":{"hosts":["blog.example.com"],"targets":["c2:80"],"state":"running"}}`

const routeNetworks = "/yoho-proxy|basecamp/kamal-proxy:v0.10.2|bridge=172.17.0.2|yoho=172.18.0.2\n" +
	"/yoho-tunnel|cloudflare/cloudflared:2026.10.0|yoho=172.18.0.3\n" +
	"/yoho-shop-production-web-1|yoho/shop-production-web:v1|yoho=172.18.0.4|yoho-shop-production_default=172.20.0.2\n"

// routeHost is a local Server (files, lock and readlink are real) whose
// Docker answers with canned output and whose dials reach origin.
type routeHost struct {
	*remote.Local
	mu       sync.Mutex
	routes   string
	networks string
	origin   string
	dials    []string
}

func (h *routeHost) Name() string { return "s1" }

func (h *routeHost) Output(ctx context.Context, c remote.Cmd) (string, error) {
	switch {
	case strings.Contains(c.Script, "kamal-proxy list --json"):
		return h.routes, nil
	case strings.Contains(c.Script, "docker inspect --format") && strings.Contains(c.Script, "NetworkSettings"):
		return h.networks, nil
	case strings.Contains(c.Script, "docker"):
		return "", errors.New("unexpected docker script: " + c.Script)
	}
	return h.Local.Output(ctx, c)
}

func (h *routeHost) Run(ctx context.Context, c remote.Cmd) error {
	_, err := h.Output(ctx, c)
	return err
}

func (h *routeHost) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	h.mu.Lock()
	h.dials = append(h.dials, addr)
	h.mu.Unlock()
	var d net.Dialer
	return d.DialContext(ctx, network, h.origin)
}

type routeFixture struct {
	a      *app
	out    *bytes.Buffer
	h      *routeHost
	cf     *migratetest.Cloudflare
	store  *secrets.Store
	status int // what the origin answers through the Proxy
	seen   []*http.Request
	mu     sync.Mutex
}

func newRouteFixture(t *testing.T, ingress string) *routeFixture {
	t.Helper()
	old := release.Root
	release.Root = t.TempDir()
	t.Cleanup(func() { release.Root = old })
	for _, v := range []*time.Duration{&tunnelPublicInterval, &tunnelUndoTimeout, &tunnelPrecheckTimeout} {
		saved := *v
		t.Cleanup(func() { *v = saved })
	}
	tunnelPublicInterval = time.Millisecond
	tunnelPrecheckTimeout = 300 * time.Millisecond
	savedCheck := tunnelPublicCheck
	t.Cleanup(func() { tunnelPublicCheck = savedCheck })
	tunnelPublicCheck = func(context.Context, string) error { return nil }

	f := &routeFixture{status: http.StatusOK}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.seen = append(f.seen, r)
		status := f.status
		f.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(origin.Close)

	f.cf = migratetest.NewCloudflare(t, cutoverAPIToken, ingress)
	t.Setenv("YOHO_CLOUDFLARE_API", f.cf.URL)
	f.out = &bytes.Buffer{}
	f.a = &app{
		g: &globals{json: true}, ui: ui.New(f.out, ui.JSON, false), destName: "production",
		cfg:  &config.Config{App: "shop", Proxy: config.ProxyConfig{Tunnel: &config.TunnelConfig{TokenSecret: "CF_TUNNEL_TOKEN", APITokenSecret: "CF_API_TOKEN"}}},
		dest: config.Destination{Runtime: "compose", Servers: []string{"s1"}},
	}
	f.store = secrets.New(map[string]string{"CF_API_TOKEN": cutoverAPIToken, "CF_TUNNEL_TOKEN": f.cf.TunnelToken()})
	f.h = &routeHost{Local: &remote.Local{}, routes: routeList, networks: routeNetworks, origin: origin.Listener.Addr().String()}

	appDir := release.AppDir("shop", "production")
	relDir := release.Dir("shop", "production", "v1")
	if err := os.MkdirAll(relDir, 0o700); err != nil {
		t.Fatal(err)
	}
	planJSON := `[{"name":"web","replicas":1,"proxy":{"hosts":["app.example.com"],"health_path":"/healthz"}},{"name":"db","replicas":1}]`
	if err := os.WriteFile(path.Join(relDir, "plan.json"), []byte(planJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(relDir, path.Join(appDir, "current")); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *routeFixture) router(t *testing.T, o tunnelRouteOptions) *tunnelRouter {
	t.Helper()
	if o.timeout == 0 {
		o.timeout = 50 * time.Millisecond
	}
	target, err := tunnelTarget(o.to)
	if err != nil {
		t.Fatal(err)
	}
	cf, redact, err := f.a.tunnelRouteAPI(f.store)
	if err != nil {
		t.Fatal(err)
	}
	return &tunnelRouter{a: f.a, hosts: []plan.NamedHost{{Name: "s1", Host: f.h}}, cf: cf, o: o, target: target,
		redact: func(err error) error { return secrets.RedactError(err, redact...) }}
}

func (f *routeFixture) service(t *testing.T, host, p string) string {
	t.Helper()
	r, _, err := migrate.FindRule(f.cf.Config(), host, p)
	if err != nil {
		t.Fatal(err)
	}
	return r.Service
}

func (f *routeFixture) record(t *testing.T) *tunnelRouteRecord {
	t.Helper()
	b, err := os.ReadFile(tunnelRoutePath("shop", "production"))
	if err != nil {
		t.Fatal(err)
	}
	var rec tunnelRouteRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	return &rec
}

func routeEvents(t *testing.T, out string) []map[string]any {
	t.Helper()
	var evs []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("invalid NDJSON %q: %v", line, err)
		}
		if ev["event"] == "tunnel_route" {
			evs = append(evs, ev)
		}
	}
	return evs
}

func phases(evs []map[string]any) string {
	var p []string
	for _, ev := range evs {
		p = append(p, ev["phase"].(string))
	}
	return strings.Join(p, ",")
}

func TestTunnelRouteSwitchesOneRule(t *testing.T) {
	f := newRouteFixture(t, routeIngress)
	var checked []string
	tunnelPublicCheck = func(_ context.Context, u string) error { checked = append(checked, u); return nil }
	if err := f.router(t, tunnelRouteOptions{hostname: "app.example.com", to: "proxy"}).run(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, f.out)
	}
	if f.cf.Puts != 1 || f.service(t, "app.example.com", "") != proxy.TunnelOrigin {
		t.Fatalf("puts %d config %s", f.cf.Puts, f.cf.Config())
	}
	want, _, _ := migrate.RewriteRule(json.RawMessage(routeIngress), "app.example.com", "", proxy.TunnelOrigin)
	if migrate.Hash(f.cf.Config()) != migrate.Hash(want) {
		t.Errorf("other rules changed: %s", f.cf.Config())
	}
	// The pre-check reached yoho-proxy on the yoho network with the
	// hostname, https and the route's health path.
	if len(f.h.dials) == 0 || f.h.dials[0] != "172.18.0.2:80" {
		t.Errorf("dials %v", f.h.dials)
	}
	if len(f.seen) == 0 || f.seen[0].Host != "app.example.com" || f.seen[0].Header.Get("X-Forwarded-Proto") != "https" || f.seen[0].URL.Path != "/healthz" {
		t.Errorf("pre-check request %+v", f.seen)
	}
	if len(checked) != 1 || checked[0] != "https://app.example.com/healthz" {
		t.Errorf("public check %v", checked)
	}
	evs := routeEvents(t, f.out.String())
	if got := phases(evs); got != "prechecked,old_rule,switched,checked" {
		t.Errorf("phases %s\n%s", got, f.out)
	}
	undo := "yoho tunnel route -d production --hostname app.example.com --to http://web:3000 --check-path /healthz"
	if evs[1]["undo"] != undo || evs[1]["old_service"] != "http://web:3000" {
		t.Errorf("old_rule event %v", evs[1])
	}
	rec := f.record(t)
	if rec.Status != "done" || rec.OldService != "http://web:3000" || rec.Undo != undo || !strings.Contains(string(rec.OldRule), "noTLSVerify") {
		t.Errorf("record %+v", rec)
	}
	if !strings.Contains(f.out.String(), "path /ws keeps http://sync:8080") {
		t.Errorf("no warning about the /ws rule\n%s", f.out)
	}
	if strings.Contains(f.out.String(), cutoverAPIToken) {
		t.Error("API token in output")
	}
	if _, err := os.Stat(path.Join(release.AppDir("shop", "production"), "lock")); !os.IsNotExist(err) {
		t.Errorf("lock not released: %v", err)
	}
}

func TestTunnelRouteAlreadyRouted(t *testing.T) {
	f := newRouteFixture(t, strings.Replace(routeIngress, "http://web:3000", "http://yoho-proxy", 1))
	if err := f.router(t, tunnelRouteOptions{hostname: "APP.example.com", to: "proxy"}).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.cf.Puts != 0 || phases(routeEvents(t, f.out.String())) != "already_routed" || len(f.h.dials) != 0 {
		t.Errorf("puts %d dials %v\n%s", f.cf.Puts, f.h.dials, f.out)
	}
}

func TestTunnelRoutePrecheckFailureChangesNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(f *routeFixture)
		want  string
	}{
		{"unhealthy", func(f *routeFixture) { f.status = http.StatusServiceUnavailable }, "got 503"},
		{"no route", func(f *routeFixture) {
			f.h.routes = `{"shop-production-api":{"hosts":["api.example.com"],"targets":["c:1"]}}`
		}, "deploy the App with x-yoho.proxy.hosts"},
		{"other App", func(f *routeFixture) {
			f.h.routes = `{"blog-production-web":{"hosts":["app.example.com"],"targets":["c:1"]}}`
		}, "not this App Destination's"},
		{"no shared network", func(f *routeFixture) {
			f.h.networks = strings.Replace(routeNetworks, "/yoho-tunnel|cloudflare/cloudflared:2026.10.0|yoho=172.18.0.3", "/yoho-tunnel|cloudflare/cloudflared:2026.10.0|host=", 1)
		}, "shares no Docker network"},
		{"no connector", func(f *routeFixture) {
			f.h.networks = "/yoho-proxy|basecamp/kamal-proxy|yoho=172.18.0.2\n"
		}, "no cloudflared connector"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRouteFixture(t, routeIngress)
			tc.setup(f)
			tunnelPublicCheck = func(context.Context, string) error { t.Error("public check ran"); return nil }
			err := f.router(t, tunnelRouteOptions{hostname: "app.example.com", to: "proxy"}).run(context.Background())
			if err == nil || !strings.Contains(f.out.String(), "nothing was changed") || !strings.Contains(f.out.String(), tc.want) {
				t.Fatalf("err %v\n%s", err, f.out)
			}
			if f.cf.Puts != 0 {
				t.Errorf("puts %d", f.cf.Puts)
			}
			if _, err := os.Stat(tunnelRoutePath("shop", "production")); !os.IsNotExist(err) {
				t.Errorf("undo record written: %v", err)
			}
		})
	}
}

func TestTunnelRouteInAppConnector(t *testing.T) {
	f := newRouteFixture(t, routeIngress)
	f.h.networks = "/yoho-proxy|basecamp/kamal-proxy|yoho-shop-production-proxy=172.21.0.2\n" +
		"/yoho-shop-production-cloudflared-1|cloudflare/cloudflared:latest|yoho-shop-production_default=172.20.0.3|yoho-shop-production-proxy=172.21.0.3\n"
	if err := f.router(t, tunnelRouteOptions{hostname: "app.example.com", to: "proxy"}).run(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, f.out)
	}
	if f.h.dials[0] != "172.21.0.2:80" {
		t.Errorf("dials %v", f.h.dials)
	}
}

func TestTunnelRoutePublicCheckFailureUndoesOnlyThatRule(t *testing.T) {
	f := newRouteFixture(t, routeIngress)
	edited := false
	tunnelPublicCheck = func(context.Context, string) error {
		if !edited {
			// Someone edits another rule while the check runs.
			edited = true
			f.cf.SetConfig(strings.Replace(string(f.cf.Config()), "http://other:80", "http://other:81", 1))
		}
		return errors.New("got 502 Bad Gateway, want 2xx")
	}
	err := f.router(t, tunnelRouteOptions{hostname: "app.example.com", to: "proxy"}).run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "put back to http://web:3000") {
		t.Fatalf("err %v\n%s", err, f.out)
	}
	if f.cf.Puts != 2 || f.service(t, "app.example.com", "") != "http://web:3000" || f.service(t, "other.example.com", "") != "http://other:81" || f.service(t, "app.example.com", "/ws") != "http://sync:8080" {
		t.Errorf("puts %d config %s", f.cf.Puts, f.cf.Config())
	}
	if rec := f.record(t); rec.Status != "undone" {
		t.Errorf("record %+v", rec)
	}
	if got := phases(routeEvents(t, f.out.String())); got != "prechecked,old_rule,switched,undo" {
		t.Errorf("phases %s", got)
	}
}

func TestTunnelRouteConcurrentEditIsNotOverwritten(t *testing.T) {
	f := newRouteFixture(t, routeIngress)
	tunnelPublicCheck = func(context.Context, string) error {
		f.cf.SetConfig(strings.Replace(string(f.cf.Config()), proxy.TunnelOrigin, "http://someone-else:8080", 1))
		return errors.New("got 502 Bad Gateway, want 2xx")
	}
	err := f.router(t, tunnelRouteOptions{hostname: "app.example.com", to: "proxy"}).run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not undone") || !strings.Contains(err.Error(), "http://someone-else:8080") {
		t.Fatalf("err %v\n%s", err, f.out)
	}
	if f.cf.Puts != 1 || f.service(t, "app.example.com", "") != "http://someone-else:8080" {
		t.Errorf("puts %d config %s", f.cf.Puts, f.cf.Config())
	}
	if rec := f.record(t); rec.Status != "undo_conflict" {
		t.Errorf("record %+v", rec)
	}
}

func TestTunnelRouteInterruptUndoes(t *testing.T) {
	f := newRouteFixture(t, routeIngress)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tunnelPublicCheck = func(ctx context.Context, _ string) error {
		cancel()
		<-ctx.Done()
		return ctx.Err()
	}
	err := f.router(t, tunnelRouteOptions{hostname: "app.example.com", to: "proxy", timeout: time.Minute}).run(ctx)
	if err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("err %v\n%s", err, f.out)
	}
	if f.cf.Puts != 2 || f.service(t, "app.example.com", "") != "http://web:3000" {
		t.Errorf("puts %d config %s", f.cf.Puts, f.cf.Config())
	}
}

func TestTunnelRouteDryRun(t *testing.T) {
	f := newRouteFixture(t, routeIngress)
	tunnelPublicCheck = func(context.Context, string) error { t.Error("public check ran"); return nil }
	if err := f.router(t, tunnelRouteOptions{hostname: "app.example.com", to: "proxy", dryRun: true}).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.cf.Puts != 0 || phases(routeEvents(t, f.out.String())) != "prechecked,old_rule,dry_run" {
		t.Errorf("puts %d\n%s", f.cf.Puts, f.out)
	}
	if _, err := os.Stat(tunnelRoutePath("shop", "production")); !os.IsNotExist(err) {
		t.Errorf("undo record written: %v", err)
	}
}

func TestTunnelRoutePathRules(t *testing.T) {
	onlyPaths := `{"ingress":[{"hostname":"app.example.com","path":"/ws","service":"http://sync:8080"},{"hostname":"app.example.com","path":"/api","service":"http://api:80"},{"service":"http_status:404"}]}`
	f := newRouteFixture(t, onlyPaths)
	err := f.router(t, tunnelRouteOptions{hostname: "app.example.com", to: "proxy"}).run(context.Background())
	if err == nil || !strings.Contains(f.out.String(), "--path") || f.cf.Puts != 0 {
		t.Fatalf("err %v puts %d\n%s", err, f.cf.Puts, f.out)
	}

	f = newRouteFixture(t, routeIngress)
	if err := f.router(t, tunnelRouteOptions{hostname: "app.example.com", path: "/ws", to: "proxy", checkPath: "/ws/up"}).run(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, f.out)
	}
	if f.service(t, "app.example.com", "/ws") != proxy.TunnelOrigin || f.service(t, "app.example.com", "") != "http://web:3000" {
		t.Errorf("config %s", f.cf.Config())
	}
	if f.record(t).Undo != "yoho tunnel route -d production --hostname app.example.com --path /ws --to http://sync:8080 --check-path /ws/up" {
		t.Errorf("record %+v", f.record(t))
	}
}

func TestTunnelRouteToOriginSkipsPrecheck(t *testing.T) {
	f := newRouteFixture(t, strings.Replace(routeIngress, "http://web:3000", proxy.TunnelOrigin, 1))
	if err := f.router(t, tunnelRouteOptions{hostname: "app.example.com", to: "http://web:3000"}).run(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, f.out)
	}
	if f.cf.Puts != 1 || len(f.h.dials) != 0 || f.service(t, "app.example.com", "") != "http://web:3000" {
		t.Errorf("puts %d dials %v", f.cf.Puts, f.h.dials)
	}
}

func TestTunnelRouteRefusesBadFlags(t *testing.T) {
	for _, tc := range []struct {
		o    tunnelRouteOptions
		want string
	}{
		{tunnelRouteOptions{hostname: "*.example.com", to: "proxy", timeout: time.Second}, "wildcard"},
		{tunnelRouteOptions{hostname: "https://app.example.com", to: "proxy", timeout: time.Second}, "not a DNS hostname"},
		{tunnelRouteOptions{hostname: "app.example.com", to: "web:3000 x", timeout: time.Second}, "must be proxy or an origin"},
		{tunnelRouteOptions{hostname: "app.example.com", to: "proxy", checkPath: "up", timeout: time.Second}, "--check-path"},
	} {
		var out bytes.Buffer
		a := &app{g: &globals{json: true}, ui: ui.New(&out, ui.JSON, false), destName: "production", cfg: &config.Config{App: "shop"}}
		if err := a.tunnelRoute(context.Background(), tc.o); err == nil || !strings.Contains(out.String(), tc.want) {
			t.Errorf("%+v: %v\n%s", tc.o, err, out.String())
		}
	}
}

func TestTunnelTargetAndSameOrigin(t *testing.T) {
	for _, tc := range []struct{ a, b string }{
		{"http://yoho-proxy", "http://yoho-proxy:80"},
		{"HTTP://Yoho-Proxy:80/", proxy.TunnelOrigin},
		{"https://web", "https://web:443"},
	} {
		if !sameOrigin(tc.a, tc.b) {
			t.Errorf("%s != %s", tc.a, tc.b)
		}
	}
	if sameOrigin("http://yoho-proxy:8080", proxy.TunnelOrigin) || sameOrigin("https://yoho-proxy", proxy.TunnelOrigin) {
		t.Error("different origins compare equal")
	}
	for _, ok := range []string{"proxy", "PROXY", "http://web:3000", "http_status:404", "hello_world", "unix:/tmp/s.sock", "tcp://db:5432"} {
		if _, err := tunnelTarget(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "web", "http://", "http://a b"} {
		if _, err := tunnelTarget(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestRollbackWarnsWhenTunnelBypassesProxy(t *testing.T) {
	f := newRouteFixture(t, routeIngress)
	saved := tunnelWarnSecrets
	t.Cleanup(func() { tunnelWarnSecrets = saved })
	loads := 0
	tunnelWarnSecrets = func(context.Context, *app) (*secrets.Store, error) { loads++; return f.store, nil }

	f.a.warnTunnelBypass(context.Background(), f.h, "v1")
	out := f.out.String()
	if !strings.Contains(out, "app.example.com points at http://web:3000, not the Proxy") || !strings.Contains(out, "app.example.com path /ws points at http://sync:8080") || strings.Contains(out, "other.example.com") {
		t.Errorf("warnings:\n%s", out)
	}
	if f.cf.Puts != 0 {
		t.Error("the warning wrote the tunnel configuration")
	}

	// Pointing at the Proxy (written without :80): no warning.
	f.out.Reset()
	f.cf.SetConfig(`{"ingress":[{"hostname":"app.example.com","service":"http://yoho-proxy"},{"service":"http_status:404"}]}`)
	f.a.warnTunnelBypass(context.Background(), f.h, "v1")
	if f.out.Len() != 0 {
		t.Errorf("unexpected output %s", f.out)
	}

	// Not configured: no secrets resolved, no Cloudflare call.
	f.a.cfg.Proxy.Tunnel.APITokenSecret = ""
	before, calls := loads, len(f.cf.Bodies)
	f.a.warnTunnelBypass(context.Background(), f.h, "v1")
	if loads != before || len(f.cf.Bodies) != calls || f.out.Len() != 0 {
		t.Errorf("ran without api_token_secret: loads %d calls %d\n%s", loads-before, len(f.cf.Bodies)-calls, f.out)
	}

	// Failures stay silent: secrets, Cloudflare, missing Release.
	f.a.cfg.Proxy.Tunnel.APITokenSecret = "CF_API_TOKEN"
	tunnelWarnSecrets = func(context.Context, *app) (*secrets.Store, error) { return nil, errors.New("op: locked") }
	f.a.warnTunnelBypass(context.Background(), f.h, "v1")
	tunnelWarnSecrets = func(context.Context, *app) (*secrets.Store, error) {
		return secrets.New(map[string]string{"CF_API_TOKEN": "wrong", "CF_TUNNEL_TOKEN": f.cf.TunnelToken()}), nil
	}
	f.a.warnTunnelBypass(context.Background(), f.h, "v1")
	f.a.warnTunnelBypass(context.Background(), f.h, "v9")
	if f.out.Len() != 0 {
		t.Errorf("unexpected output %s", f.out)
	}
}

func TestRollbackWarningFallsBackToMigrateToken(t *testing.T) {
	a := &app{cfg: &config.Config{Proxy: config.ProxyConfig{Tunnel: &config.TunnelConfig{TokenSecret: "T"}}},
		dest: config.Destination{Migrate: &config.Migrate{Tunnel: &config.MigrateTunnel{APITokenSecret: "MIGRATE_CF"}}}}
	if got := a.tunnelAPITokenSecret(); got != "MIGRATE_CF" {
		t.Errorf("got %q", got)
	}
	a.cfg.Proxy.Tunnel.APITokenSecret = "CF"
	if got := a.tunnelAPITokenSecret(); got != "CF" {
		t.Errorf("got %q", got)
	}
}

type failingPut struct{ migrate.CloudflareAPI }

func (failingPut) PutConfig(context.Context, json.RawMessage) error {
	return errors.New("Cloudflare API PUT tunnel configuration: HTTP 500")
}

func TestTunnelRoutePutFailureLeavesRule(t *testing.T) {
	f := newRouteFixture(t, routeIngress)
	r := f.router(t, tunnelRouteOptions{hostname: "app.example.com", to: "proxy"})
	r.cf = failingPut{r.cf}
	err := r.run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") || !strings.Contains(err.Error(), "still points at http://web:3000") {
		t.Fatalf("err %v\n%s", err, f.out)
	}
	if f.cf.Puts != 0 || f.record(t).Status != "undone" {
		t.Errorf("puts %d record %+v", f.cf.Puts, f.record(t))
	}
}
