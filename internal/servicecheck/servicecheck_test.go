package servicecheck

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// fakeHost reads files from the local disk (release.Root is a temp dir),
// answers docker commands from respond, and dials through dial when set.
type fakeHost struct {
	local   *remote.Local
	respond func(script string) string
	mu      sync.Mutex
	scripts []string
}

func (h *fakeHost) Name() string { return "s1" }
func (h *fakeHost) Run(ctx context.Context, c remote.Cmd) error {
	_, err := h.Output(ctx, c)
	return err
}
func (h *fakeHost) Output(_ context.Context, c remote.Cmd) (string, error) {
	h.mu.Lock()
	h.scripts = append(h.scripts, c.Script)
	h.mu.Unlock()
	return h.respond(c.Script), nil
}
func (h *fakeHost) WriteFile(ctx context.Context, p string, b []byte, m os.FileMode, sudo bool) error {
	return h.local.WriteFile(ctx, p, b, m, sudo)
}
func (h *fakeHost) ReadFile(ctx context.Context, p string, sudo bool) ([]byte, error) {
	return h.local.ReadFile(ctx, p, sudo)
}
func (h *fakeHost) Close() error { return nil }

type dialHost struct {
	*fakeHost
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

func (h *dialHost) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return h.dial(ctx, network, addr)
}

const planJSON = `[
  {"name": "web", "replicas": 2, "proxy": {"port": 3000}},
  {"name": "sync", "replicas": 1, "check": {"type": "websocket", "port": 8080, "path": "/sync", "host": "localhost", "timeout": "30s"}},
  {"name": "mqtt", "replicas": 1, "check": {"type": "tcp", "port": 1883, "timeout": "30s"}}
]`

func withRelease(t *testing.T, plan string) {
	t.Helper()
	old := release.Root
	release.Root = t.TempDir()
	t.Cleanup(func() { release.Root = old })
	if plan == "" {
		return
	}
	dir := filepath.Join(release.AppDir("shop", "production"), "releases", "v1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plan.json"), []byte(plan), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/v1", filepath.Join(release.AppDir("shop", "production"), "current")); err != nil {
		t.Fatal(err)
	}
}

func docker(script string) string {
	switch {
	case strings.HasPrefix(script, "docker ps "):
		return "aaa|yoho-shop-production-web-1|web||running\nbbb|yoho-shop-production-sync-1|sync||running\n" +
			"ccc|yoho-shop-production-mqtt-1|mqtt||running\nddd|yoho-shop-production-sync-run-1|sync|True|running\n"
	case strings.HasPrefix(script, "docker inspect "):
		return "bbbfull|/yoho-shop-production-sync-1|yoho-shop-production|yoho-shop-production_default|yoho-shop-production-proxy=172.30.0.4|yoho-shop-production_default=172.18.0.5\n" +
			"cccfull|/yoho-shop-production-mqtt-1|yoho-shop-production|yoho-shop-production_default|yoho-shop-production_default=172.18.0.6\n"
	}
	return ""
}

func TestProbeOnePass(t *testing.T) {
	withRelease(t, planJSON)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	var mu sync.Mutex
	var dialed []string
	h := &dialHost{fakeHost: &fakeHost{local: &remote.Local{}, respond: docker}}
	h.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		dialed = append(dialed, addr)
		mu.Unlock()
		if addr == "172.18.0.6:1883" {
			var d net.Dialer
			return d.DialContext(ctx, network, ln.Addr().String())
		}
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	}
	st, err := Probe(context.Background(), h, "shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	if got := st["yoho-shop-production-mqtt-1"]; got.String() != "ok" || got.Check != "tcp on port 1883" || got.Service != "mqtt" {
		t.Fatalf("mqtt: %+v", got)
	}
	if got := st["yoho-shop-production-sync-1"].String(); got != "failed: websocket /sync on 172.18.0.5:8080: connection refused" {
		t.Fatalf("sync: %q", got)
	}
	if len(st) != 2 {
		t.Fatalf("statuses: %+v (one-offs and Services without a check are not probed)", st)
	}
	if len(dialed) != 2 {
		t.Fatalf("dialed %v", dialed)
	}
	// Read-only: docker ps and inspect only.
	for _, s := range h.scripts {
		if !strings.HasPrefix(s, "docker ps ") && !strings.HasPrefix(s, "docker inspect ") {
			t.Fatalf("unexpected command %q", s)
		}
	}
}

func TestProbeBounded(t *testing.T) {
	withRelease(t, planJSON)
	old := ProbeTimeout
	ProbeTimeout = 100 * time.Millisecond
	t.Cleanup(func() { ProbeTimeout = old })
	h := &dialHost{fakeHost: &fakeHost{local: &remote.Local{}, respond: docker}}
	h.dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done() // a dial that hangs
		return nil, ctx.Err()
	}
	start := time.Now()
	st, err := Probe(context.Background(), h, "shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("pass took %s; probes run concurrently with ProbeTimeout each", el)
	}
	for name, s := range st {
		if s.State != Failed || !strings.Contains(s.Error, "no answer in time") {
			t.Fatalf("%s: %+v", name, s)
		}
	}
}

func TestProbeWithoutChecksOrDialer(t *testing.T) {
	withRelease(t, "")
	h := &fakeHost{local: &remote.Local{}, respond: docker}
	if st, err := Probe(context.Background(), h, "shop", "production"); st != nil || err != nil || len(h.scripts) != 0 {
		t.Fatalf("no Release: %v %v %v", st, err, h.scripts)
	}
	withRelease(t, `[{"name":"web","replicas":1}]`)
	if st, err := Probe(context.Background(), h, "shop", "production"); st != nil || err != nil || len(h.scripts) != 0 {
		t.Fatalf("no checks: %v %v %v", st, err, h.scripts)
	}
	withRelease(t, planJSON)
	st, err := Probe(context.Background(), h, "shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	if s := st["yoho-shop-production-sync-1"]; s.State != Unknown {
		t.Fatalf("a Host that cannot dial: %+v", s)
	}
}

func TestPickAddr(t *testing.T) {
	for _, tc := range []struct {
		mode string
		nets []string
		want string
	}{
		{"p_default", []string{"yoho=172.19.0.2", "p_default=172.18.0.5"}, "172.18.0.5"},
		{"p_default", []string{"p-proxy=172.30.0.2"}, "172.30.0.2"},
		{"host", nil, "127.0.0.1"},
		{"p_default", []string{"p_default="}, ""},
		{"p_default", []string{"p_default=fd00::5"}, ""},
	} {
		if got := pickAddr("p", tc.mode, tc.nets); got != tc.want {
			t.Errorf("%s %v: %q, want %q", tc.mode, tc.nets, got, tc.want)
		}
	}
}

func TestAppendColumn(t *testing.T) {
	table := "SERVICE   NAME                          STATUS          IMAGE\n" +
		"mqtt      yoho-shop-production-mqtt-1   Up 2 hours      eclipse-mosquitto:2\n" +
		"sync      yoho-shop-production-sync-1   Up 5 minutes    sync:v3\n" +
		"web       yoho-shop-production-web-1    Up 2 hours (healthy)   web:v3\n"
	got := AppendColumn(table, map[string]Status{
		"yoho-shop-production-mqtt-1": {State: OK},
		"yoho-shop-production-sync-1": {State: Failed, Error: "websocket /sync on 172.18.0.5:8080: got 200 OK, want 101"},
	})
	want := "SERVICE   NAME                          STATUS          IMAGE                 CHECK\n" +
		"mqtt      yoho-shop-production-mqtt-1   Up 2 hours      eclipse-mosquitto:2   ok\n" +
		"sync      yoho-shop-production-sync-1   Up 5 minutes    sync:v3               failed: websocket /sync on 172.18.0.5:8080: got 200 OK, want 101\n" +
		"web       yoho-shop-production-web-1    Up 2 hours (healthy)   web:v3         -\n"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestProbeReportsStoppedContainers(t *testing.T) {
	withRelease(t, planJSON)
	h := &dialHost{fakeHost: &fakeHost{local: &remote.Local{}, respond: func(script string) string {
		switch {
		case strings.HasPrefix(script, "docker ps "):
			all := []string{"aaa|yoho-shop-production-web-1|web||running", "bbb|yoho-shop-production-sync-1|sync||exited", "ccc|yoho-shop-production-mqtt-1|mqtt||restarting"}
			if strings.Contains(script, "status=running") {
				all = all[:1]
			}
			return strings.Join(all, "\n") + "\n"
		case strings.HasPrefix(script, "docker inspect "):
			t.Errorf("inspected stopped containers: %s", script)
		}
		return ""
	}}}
	h.dial = func(context.Context, string, string) (net.Conn, error) {
		t.Error("dialed a stopped container")
		return nil, syscall.ECONNREFUSED
	}
	st, err := Probe(context.Background(), h, "shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"yoho-shop-production-sync-1", "yoho-shop-production-mqtt-1"} {
		if got := st[name]; got.String() != "failed: not running" {
			t.Errorf("%s: %+v", name, got)
		}
	}
	if len(st) != 2 {
		t.Errorf("statuses %+v", st)
	}

	// Checks but no containers: an empty, non-nil result keeps the column.
	h.respond = func(string) string { return "" }
	if st, err := Probe(context.Background(), h, "shop", "production"); err != nil || st == nil {
		t.Errorf("no containers: %v %v", st, err)
	}
}

func TestAddressesWithEmptyFields(t *testing.T) {
	h := &fakeHost{local: &remote.Local{}, respond: func(string) string {
		// An empty network mode does not shift the networks.
		return "aaa|/p-web-1|p||p_default=172.18.0.5\n"
	}}
	cs, err := Addresses(context.Background(), h, "p", []string{"aaa"})
	if err != nil || len(cs) != 1 || cs[0].Addr != "172.18.0.5" || cs[0].Name != "p-web-1" {
		t.Fatalf("%+v %v", cs, err)
	}
	h.respond = func(string) string { return "aaa|/x-web-1||default|default=172.18.0.5\n" }
	if _, err := Addresses(context.Background(), h, "p", []string{"aaa"}); err == nil || !strings.Contains(err.Error(), "does not belong to compose project p") {
		t.Fatalf("no project label: %v", err)
	}
}

func TestProbeBoundsConcurrentProbes(t *testing.T) {
	withRelease(t, `[{"name":"mqtt","replicas":8,"check":{"type":"tcp","port":1883,"timeout":"30s"}}]`)
	var ps, inspect []string
	for i := range 8 {
		id := string(rune('a'+i)) + "id"
		ps = append(ps, id+"|yoho-shop-production-mqtt-"+id+"|mqtt||running")
		inspect = append(inspect, id+"|/yoho-shop-production-mqtt-"+id+"|yoho-shop-production|yoho-shop-production_default|yoho-shop-production_default=172.18.0.6")
	}
	h := &dialHost{fakeHost: &fakeHost{local: &remote.Local{}, respond: func(script string) string {
		if strings.HasPrefix(script, "docker ps ") {
			return strings.Join(ps, "\n")
		}
		return strings.Join(inspect, "\n")
	}}}
	var mu sync.Mutex
	open, most := 0, 0
	h.dial = func(context.Context, string, string) (net.Conn, error) {
		mu.Lock()
		open++
		most = max(most, open)
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		mu.Lock()
		open--
		mu.Unlock()
		return nil, syscall.ECONNREFUSED
	}
	st, err := Probe(context.Background(), h, "shop", "production")
	if err != nil || len(st) != 8 {
		t.Fatalf("%v %v", st, err)
	}
	if most > maxProbes {
		t.Fatalf("%d probes at once, want at most %d", most, maxProbes)
	}
}
