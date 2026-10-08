package proxy

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yoho-dev/yoho/internal/config"
	"github.com/yoho-dev/yoho/internal/remote"
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
	if !strings.Contains(all, "docker run -d --label 'yoho.proxy.config=") || strings.Contains(all, "docker rm -f") {
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
	if all := strings.Join(h.scripts, "\n"); !strings.Contains(all, "docker rm -f yoho-proxy") || !strings.Contains(all, "docker run -d") {
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
