package proxy

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/remote"
)

type fakeHost struct {
	scripts []string
	output  func(script string) string
}

func (f *fakeHost) Name() string { return "s1" }
func (f *fakeHost) Run(_ context.Context, c remote.Cmd) error {
	f.scripts = append(f.scripts, c.Script)
	return nil
}
func (f *fakeHost) Output(_ context.Context, c remote.Cmd) (string, error) {
	f.scripts = append(f.scripts, c.Script)
	if f.output != nil {
		return f.output(c.Script), nil
	}
	return "", nil
}
func (f *fakeHost) WriteFile(context.Context, string, []byte, os.FileMode, bool) error { return nil }
func (f *fakeHost) ReadFile(context.Context, string, bool) ([]byte, error) {
	return nil, os.ErrNotExist
}
func (f *fakeHost) Close() error { return nil }

func intp(i int) *int { return &i }

func TestRunArgsPorts(t *testing.T) {
	got := strings.Join(runArgs(config.ProxyConfig{}), " ")
	for _, want := range []string{"--publish 80:80", "--publish 443:443", "--network yoho", "--restart unless-stopped", "yoho-proxy-config:/home/kamal-proxy/.config/kamal-proxy", DefaultImage + " kamal-proxy run"} {
		if !strings.Contains(got, want) {
			t.Errorf("default args missing %q: %s", want, got)
		}
	}
	got = strings.Join(runArgs(config.ProxyConfig{HTTPPort: intp(8080), HTTPSPort: intp(0), Bind: "127.0.0.1", Image: "x/proxy:1"}), " ")
	if !strings.Contains(got, "--publish 127.0.0.1:8080:80") || strings.Contains(got, ":443") || !strings.Contains(got, "x/proxy:1 kamal-proxy run") {
		t.Errorf("custom args: %s", got)
	}
	got = strings.Join(runArgs(config.ProxyConfig{Bind: "::1"}), " ")
	if !strings.Contains(got, "--publish [::1]:80:80") {
		t.Errorf("ipv6 bind: %s", got)
	}
}

func TestBootCreatesWhenMissing(t *testing.T) {
	h := &fakeHost{}
	var out bytes.Buffer
	if err := Boot(context.Background(), h, config.ProxyConfig{}, &out); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(h.scripts, "\n")
	if !strings.Contains(all, "docker network create yoho") {
		t.Error("network not ensured")
	}
	if !strings.Contains(all, "docker run -d --label 'yoho.proxy.config=") || strings.Contains(out.String(), "recreating") {
		t.Errorf("unexpected boot script:\n%s", all)
	}
}

func TestBootIdempotentAndRecreate(t *testing.T) {
	want := configHash(runArgs(config.ProxyConfig{}))
	h := &fakeHost{output: func(string) string { return want + "|true" }}
	if err := Boot(context.Background(), h, config.ProxyConfig{}, nil); err != nil {
		t.Fatal(err)
	}
	if all := strings.Join(h.scripts, "\n"); strings.Contains(all, "docker run") || strings.Contains(all, "docker start") {
		t.Errorf("matching proxy should be left alone:\n%s", all)
	}

	h = &fakeHost{output: func(string) string { return want + "|false" }}
	_ = Boot(context.Background(), h, config.ProxyConfig{}, nil)
	if all := strings.Join(h.scripts, "\n"); !strings.Contains(all, "docker start yoho-proxy") {
		t.Errorf("stopped proxy should be started:\n%s", all)
	}

	h = &fakeHost{output: func(string) string { return "old|true" }}
	var out bytes.Buffer
	_ = Boot(context.Background(), h, config.ProxyConfig{}, &out)
	if all := strings.Join(h.scripts, "\n"); !strings.Contains(all, "docker rename yoho-proxy yoho-proxy-old") || !strings.Contains(all, "docker run -d") || !strings.Contains(all, "docker rename yoho-proxy-old yoho-proxy") {
		t.Errorf("changed proxy should be recreated:\n%s", all)
	}
	if !strings.Contains(out.String(), "downtime") {
		t.Errorf("missing downtime warning: %q", out.String())
	}
}

func TestDeployArgs(t *testing.T) {
	h := &fakeHost{}
	err := Deploy(context.Background(), h, "shop-production-web", "yoho-shop-production-web-2:3000", DeployOptions{
		Hosts: []string{"a.example.com", "b.example.com"}, HealthPath: "/up", TLS: true,
		DeployTimeout: 30 * time.Second, DrainTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "'docker' 'exec' 'yoho-proxy' 'kamal-proxy' 'deploy' 'shop-production-web' '--target' 'yoho-shop-production-web-2:3000' '--host' 'a.example.com' '--host' 'b.example.com' '--health-check-path' '/up' '--tls' '--deploy-timeout' '30s' '--drain-timeout' '10s'"
	if h.scripts[0] != want {
		t.Errorf("got  %s\nwant %s", h.scripts[0], want)
	}
	if err := Deploy(context.Background(), h, "x", "y:1", DeployOptions{TLS: true}); err == nil {
		t.Error("tls without hosts should fail")
	}
	if err := Remove(context.Background(), h, "x"); err != nil || !strings.Contains(h.scripts[len(h.scripts)-1], "'remove' 'x'") {
		t.Errorf("remove: %v %v", err, h.scripts)
	}
}

func TestParseRoutesAndList(t *testing.T) {
	out := `{"b-web":{"hosts":["*"],"targets":["c-1:3000","c-2:3000"],"state":"running","tls":true},"a-web":{"hosts":["a.example.com"],"targets":["c-3:80"],"state":"stopped"}}`
	rs, err := ParseRoutes(out)
	if err != nil || len(rs) != 2 {
		t.Fatalf("%v %v", rs, err)
	}
	if rs[0].Service != "a-web" || rs[0].State != "stopped" || rs[1].Target != "c-1:3000,c-2:3000" || !rs[1].TLS {
		t.Errorf("%+v", rs)
	}
	if rs, err := ParseRoutes("  \n"); err != nil || rs != nil {
		t.Errorf("empty: %v %v", rs, err)
	}
	if _, err := ParseRoutes("not json"); err == nil {
		t.Error("want parse error")
	}
	h := &fakeHost{output: func(string) string { return out }}
	rs, err = List(context.Background(), h)
	if err != nil || len(rs) != 2 || !strings.Contains(h.scripts[0], "docker exec yoho-proxy kamal-proxy list --json") {
		t.Errorf("%v %v %v", rs, err, h.scripts)
	}
}

func TestListMissingProxy(t *testing.T) {
	// A missing container makes the script exit 0 with no output: no routes.
	h := &fakeHost{output: func(string) string { return "" }}
	rs, err := List(context.Background(), h)
	if err != nil || rs != nil {
		t.Errorf("missing proxy: %v %v", rs, err)
	}
	if !strings.Contains(h.scripts[0], "docker container inspect yoho-proxy >/dev/null 2>&1 || exit 0") {
		t.Errorf("no existence guard: %q", h.scripts[0])
	}
}

func TestNeedsBoot(t *testing.T) {
	ctx := context.Background()
	cfg := config.ProxyConfig{}
	want := configHash(runArgs(cfg))
	for _, tc := range []struct {
		state string
		need  bool
		why   string
	}{
		{"", true, "missing"},
		{"deadbeef|true", true, "configuration changed"},
		{want + "|false", true, "stopped"},
		{want + "|true", false, ""},
	} {
		h := &fakeHost{output: func(string) string { return tc.state }}
		need, why, err := NeedsBoot(ctx, h, cfg)
		if err != nil || need != tc.need || !strings.Contains(why, tc.why) {
			t.Errorf("%q: %v %q %v", tc.state, need, why, err)
		}
		for _, s := range h.scripts {
			if strings.Contains(s, "docker run") || strings.Contains(s, "docker rm") {
				t.Errorf("NeedsBoot must be read-only: %s", s)
			}
		}
	}
}

func TestBootWithSkipNetwork(t *testing.T) {
	h := &fakeHost{}
	if err := BootWith(context.Background(), h, config.ProxyConfig{}, nil, BootOptions{SkipNetwork: true}); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(h.scripts, "\n")
	if strings.Contains(all, "docker network") {
		t.Errorf("network must be left alone:\n%s", all)
	}
	if !strings.Contains(all, "docker run -d") {
		t.Errorf("proxy not booted:\n%s", all)
	}
}

func TestBootRecreateRestoresOldProxyOnFailure(t *testing.T) {
	h := &fakeHost{output: func(string) string { return "old|true" }}
	if err := Boot(context.Background(), h, config.ProxyConfig{}, nil); err != nil {
		t.Fatal(err)
	}
	script := h.scripts[len(h.scripts)-1]
	stop := strings.Index(script, "docker stop yoho-proxy")
	ren := strings.Index(script, "docker rename yoho-proxy yoho-proxy-old")
	run := strings.Index(script, "if docker run -d")
	undo := strings.LastIndex(script, "docker rename yoho-proxy-old yoho-proxy")
	start := strings.LastIndex(script, "docker start yoho-proxy")
	if !(stop >= 0 && stop < ren && ren < run && run < undo && undo < start) {
		t.Fatalf("bad recreate order:\n%s", script)
	}
	if i := strings.Index(script, "docker rm -f yoho-proxy >"); i >= 0 && i < run {
		t.Errorf("old proxy removed before the new one runs:\n%s", script)
	}
	if out, err := exec.Command("sh", "-n", "-c", script).CombinedOutput(); err != nil {
		t.Errorf("sh -n: %v %s", err, out)
	}
}

func TestBootRecreateIsLockedAndRechecked(t *testing.T) {
	h := &fakeHost{output: func(string) string { return "old|true" }}
	if err := Boot(context.Background(), h, config.ProxyConfig{}, nil); err != nil {
		t.Fatal(err)
	}
	script := h.scripts[len(h.scripts)-1]
	want := configHash(runArgs(config.ProxyConfig{}))
	lock := strings.Index(script, "flock -w 120 9")
	restore := strings.Index(script, "docker rename yoho-proxy-old yoho-proxy")
	recheck := strings.Index(script, "[ \"$cur\" = '"+want+"' ]")
	rmOld := strings.Index(script, "docker rm -f yoho-proxy-old")
	stop := strings.Index(script, "docker stop yoho-proxy")
	if !(lock >= 0 && lock < restore && restore < recheck && recheck < rmOld && rmOld < stop) {
		t.Fatalf("bad lock/recheck order:\n%s", script)
	}
	if !strings.Contains(script, "trap 'rmdir \"$lockd\"' EXIT") || strings.Contains(script, "mv ") {
		t.Errorf("mkdir fallback not released on exit or reclaims:\n%s", script)
	}
	if out, err := exec.Command("sh", "-n", "-c", script).CombinedOutput(); err != nil {
		t.Errorf("sh -n: %v %s", err, out)
	}
}

// runLockSnippet runs the generated lock snippet followed by body in sh, with
// PATH optionally restricted so flock is hidden (forcing the mkdir fallback).
func runLockSnippet(t *testing.T, base, body string, hideFlock bool) (string, error) {
	t.Helper()
	script := "set -eu\n" + proxyLockScript(base) + body
	if out, err := exec.Command("sh", "-n", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("sh -n: %v %s", err, out)
	}
	cmd := exec.Command("sh", "-c", script)
	if hideFlock {
		// A shim dir with only the tools the snippet needs.
		bin := t.TempDir()
		for _, n := range []string{"mkdir", "rmdir", "dirname", "sleep", "sh"} {
			p, err := exec.LookPath(n)
			if err != nil {
				t.Skipf("no %s", n)
			}
			if err := os.Symlink(p, bin+"/"+n); err != nil {
				t.Fatal(err)
			}
		}
		cmd.Env = []string{"PATH=" + bin}
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestProxyLockMkdirFallback(t *testing.T) {
	base := t.TempDir() + "/sub/proxy"
	out, err := runLockSnippet(t, base, "echo held\n", true)
	if err != nil || !strings.Contains(out, "held") {
		t.Fatalf("lock run: %v %s", err, out)
	}
	if _, err := os.Stat(base + ".lock.d"); err == nil {
		t.Error("lock dir not removed on exit")
	}
	// A lock dir left behind (even by a dead holder) is never reclaimed.
	if err := os.Mkdir(base+".lock.d", 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := "waited=119\n"
	script := "set -eu\n" + strings.Replace(proxyLockScript(base), "waited=0\n", cmd, 1) + "echo acquired\n"
	got, err := exec.Command("sh", "-c", script).CombinedOutput()
	if err == nil || strings.Contains(string(got), "acquired") || !strings.Contains(string(got), "rmdir") {
		t.Fatalf("stale lock must fail with guidance, got err=%v %s", err, got)
	}
	if _, err := os.Stat(base + ".lock.d"); err != nil {
		t.Error("foreign lock dir must not be removed")
	}
}

func TestProxyLockFlock(t *testing.T) {
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("no flock")
	}
	base := t.TempDir() + "/proxy"
	out, err := runLockSnippet(t, base, "echo held\n", false)
	if err != nil || !strings.Contains(out, "held") {
		t.Fatalf("flock run: %v %s", err, out)
	}
}
