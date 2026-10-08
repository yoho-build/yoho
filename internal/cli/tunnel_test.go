package cli

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/proxy"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/secrets"
	"github.com/yoho-build/yoho/internal/ui"
)

// tunnelHost fakes a Server with one token connector.
type tunnelHost struct {
	scripts []string
	owner   string
	hash    string
	exists  bool
}

func (h *tunnelHost) Name() string { return "s1" }
func (h *tunnelHost) Run(_ context.Context, c remote.Cmd) error {
	h.scripts = append(h.scripts, c.Script)
	return nil
}
func (h *tunnelHost) Output(_ context.Context, c remote.Cmd) (string, error) {
	h.scripts = append(h.scripts, c.Script)
	s := c.Script
	if strings.HasPrefix(s, "docker ps") && h.exists {
		if strings.Contains(s, "{{.Image}}") {
			return "yoho-tunnel|" + proxy.DefaultTunnelImage + "|Up 1 minute|token|" + h.hash + "|" + h.owner, nil
		}
		return "yoho-tunnel", nil
	}
	if strings.HasPrefix(s, "docker inspect") {
		return "true\nRegistered tunnel connection connIndex=0\n", nil
	}
	return "", nil
}
func (h *tunnelHost) WriteFile(context.Context, string, []byte, os.FileMode, bool) error { return nil }
func (h *tunnelHost) ReadFile(context.Context, string, bool) ([]byte, error) {
	return nil, os.ErrNotExist
}
func (h *tunnelHost) Close() error { return nil }

func (h *tunnelHost) touched() bool {
	all := strings.Join(h.scripts, "\n")
	return strings.Contains(all, "docker rm") || strings.Contains(all, "docker run")
}

func tunnelApp(name string, t *config.TunnelConfig) *app {
	return &app{
		g: &globals{}, ui: ui.Discard(), destName: "prod",
		cfg:  &config.Config{App: name, Proxy: config.ProxyConfig{Tunnel: t}},
		dest: config.Destination{Runtime: "compose"},
	}
}

func TestReconcileTunnelLeavesOtherAppsConnector(t *testing.T) {
	// App "blog" has no tunnel config; "shop" owns the connector on the Server.
	h := &tunnelHost{exists: true, owner: proxy.TunnelOwner("shop", "prod"), hash: "x"}
	a := tunnelApp("blog", nil)
	s := &session{a: a, hosts: []plan.NamedHost{{Name: "s1", Host: h}}, store: secrets.New(nil)}
	if _, err := a.reconcileTunnel(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if h.touched() {
		t.Errorf("another App's connector was removed:\n%s", strings.Join(h.scripts, "\n"))
	}
	// Legacy connector without an owner label is not ours either.
	h = &tunnelHost{exists: true, hash: "x"}
	s.hosts[0].Host = h
	if _, err := a.reconcileTunnel(context.Background(), s); err != nil || h.touched() {
		t.Errorf("legacy connector touched (err=%v)", err)
	}
	// Its own connector is removed.
	h = &tunnelHost{exists: true, owner: proxy.TunnelOwner("blog", "prod"), hash: "x"}
	s.hosts[0].Host = h
	if _, err := a.reconcileTunnel(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(h.scripts, "\n"), "docker rm -f yoho-tunnel") {
		t.Error("own connector must be removed when the config drops the tunnel")
	}
}

func TestReconcileTunnelMissingTokenKeepsConnector(t *testing.T) {
	h := &tunnelHost{exists: true, owner: proxy.TunnelOwner("shop", "prod"), hash: "x"}
	a := tunnelApp("shop", &config.TunnelConfig{TokenSecret: "CF_TUNEL_TOKEN"}) // misspelled
	s := &session{a: a, hosts: []plan.NamedHost{{Name: "s1", Host: h}}, store: secrets.New(map[string]string{"CF_TUNNEL_TOKEN": "tok"})}
	_, err := a.reconcileTunnel(context.Background(), s)
	if err == nil || !strings.Contains(err.Error(), "CF_TUNEL_TOKEN") {
		t.Fatalf("err = %v", err)
	}
	if len(h.scripts) != 0 {
		t.Errorf("Server must not be touched:\n%s", strings.Join(h.scripts, "\n"))
	}
	if _, err := tunnelToken(secrets.New(map[string]string{"X": " "}), &config.TunnelConfig{TokenSecret: "X"}); err == nil {
		t.Error("blank token must fail")
	}
	if tok, err := tunnelToken(secrets.New(nil), &config.TunnelConfig{}); err != nil || tok != "" {
		t.Errorf("quick tunnel: %q %v", tok, err)
	}
}

func TestPlanTunnelTokenRotationAndOwnership(t *testing.T) {
	cfg := &config.TunnelConfig{TokenSecret: "T"}
	owner := proxy.TunnelOwner("shop", "prod")
	a := tunnelApp("shop", cfg)
	ctx := context.Background()
	hosts := func(h *tunnelHost) []plan.NamedHost { return []plan.NamedHost{{Name: "s1", Host: h}} }

	same := &tunnelHost{exists: true, owner: owner, hash: proxy.TunnelConfigHash(*cfg, "old")}
	ch, err := a.planTunnel(ctx, hosts(same), "old")
	if err != nil || len(ch) != 1 || ch[0].Action != plan.ActionNoop {
		t.Fatalf("unchanged token: %+v %v", ch, err)
	}
	ch, err = a.planTunnel(ctx, hosts(same), "new")
	if err != nil || len(ch) != 1 || ch[0].Action != plan.ActionUpdate || !strings.Contains(strings.Join(ch[0].Reasons, ","), "token changed") {
		t.Fatalf("rotated token must be an update: %+v %v", ch, err)
	}
	for _, r := range ch[0].Reasons {
		if strings.Contains(r, "new") && r != "token changed" {
			t.Errorf("reason leaks token: %q", r)
		}
	}

	// No tunnel in config: only a connector this App owns is planned for deletion.
	b := tunnelApp("shop", nil)
	if ch, _ = b.planTunnel(ctx, hosts(same), ""); len(ch) != 1 || ch[0].Action != plan.ActionDelete {
		t.Errorf("own connector: %+v", ch)
	}
	other := &tunnelHost{exists: true, owner: proxy.TunnelOwner("blog", "prod"), hash: "x"}
	if ch, _ = b.planTunnel(ctx, hosts(other), ""); len(ch) != 0 {
		t.Errorf("foreign connector must not be planned for deletion: %+v", ch)
	}
}
