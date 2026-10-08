package proxy

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/remote"
)

const quickLogs = `2026-10-08T10:00:00Z INF Thank you for trying Cloudflare Tunnel.
2026-10-08T10:00:01Z INF Requesting new quick Tunnel on trycloudflare.com...
2026-10-08T10:00:02Z INF +--------------------------------------------------------------------------------------------+
2026-10-08T10:00:02Z INF |  Your quick Tunnel has been created! Visit it at (it may take some time to be reachable):  |
2026-10-08T10:00:02Z INF |  https://random-words-here-1234.trycloudflare.com                                          |
2026-10-08T10:00:02Z INF +--------------------------------------------------------------------------------------------+
2026-10-08T10:00:05Z INF Registered tunnel connection connIndex=0 connection=abc event=0 ip=1.2.3.4 location=sin01 protocol=quic
`

func TestParseQuickURL(t *testing.T) {
	if got := ParseQuickURL(quickLogs); got != "https://random-words-here-1234.trycloudflare.com" {
		t.Errorf("got %q", got)
	}
	errLogs := `ERR Failed to request quick Tunnel error="Post \"https://api.trycloudflare.com/tunnel\": EOF"`
	if got := ParseQuickURL(errLogs); got != "" {
		t.Errorf("api URL must be ignored, got %q", got)
	}
	if ParseQuickURL("") != "" {
		t.Error("empty")
	}
}

func TestParseConnections(t *testing.T) {
	logs := quickLogs + "INF Registered tunnel connection connIndex=1 x\nINF Registered tunnel connection connIndex=1 again\n"
	if n := parseConnections(logs); n != 2 {
		t.Errorf("got %d", n)
	}
}

func TestTunnelContainers(t *testing.T) {
	if got := strings.Join(TunnelContainers(0), ","); got != "yoho-tunnel" {
		t.Error(got)
	}
	if got := strings.Join(TunnelContainers(3), ","); got != "yoho-tunnel-1,yoho-tunnel-2,yoho-tunnel-3" {
		t.Error(got)
	}
}

func TestTunnelRunArgs(t *testing.T) {
	q := strings.Join(tunnelRunArgs("yoho-tunnel", "img:1", "h", "shop/prod", true), " ")
	for _, w := range []string{"--network yoho", "--restart unless-stopped", "img:1 tunnel --no-autoupdate --url http://yoho-proxy:80", "yoho.tunnel.mode=quick"} {
		if !strings.Contains(q, w) {
			t.Errorf("quick missing %q: %s", w, q)
		}
	}
	if strings.Contains(q, "env-file") {
		t.Error("quick must not use env file")
	}
	tk := strings.Join(tunnelRunArgs("yoho-tunnel-1", "img:1", "h", "", false), " ")
	if !strings.Contains(tk, "--env-file ") || !strings.HasSuffix(tk, "img:1 tunnel --no-autoupdate run") {
		t.Errorf("token args: %s", tk)
	}
}

// tunnelFake simulates docker state for the tunnel containers.
type tunnelFake struct {
	scripts    []string
	written    map[string][]byte
	modes      map[string]os.FileMode
	containers map[string]string // name -> hash label; presence = exists (running)
	owners     map[string]string // name -> owner label
	logs       string
}

func (f *tunnelFake) Name() string { return "s1" }
func (f *tunnelFake) Run(_ context.Context, c remote.Cmd) error {
	f.scripts = append(f.scripts, c.Script)
	return nil
}
func (f *tunnelFake) Output(_ context.Context, c remote.Cmd) (string, error) {
	f.scripts = append(f.scripts, c.Script)
	s := c.Script
	switch {
	case strings.HasPrefix(s, "docker container inspect"):
		for name, h := range f.containers {
			if strings.Contains(s, " "+name+" ") {
				return h + "|true", nil
			}
		}
		return "", nil
	case strings.HasPrefix(s, "docker inspect"):
		return "true\n" + f.logs, nil
	case strings.HasPrefix(s, "docker ps"):
		var names []string
		for n := range f.containers {
			names = append(names, n)
		}
		if strings.Contains(s, "{{.Image}}") {
			var lines []string
			for _, n := range names {
				lines = append(lines, n+"|img|Up 1 minute|quick|"+f.containers[n]+"|"+f.owners[n])
			}
			return strings.Join(lines, "\n"), nil
		}
		if strings.Contains(s, "yoho.tunnel.owner") {
			var lines []string
			for _, n := range names {
				lines = append(lines, n+"|"+f.owners[n])
			}
			return strings.Join(lines, "\n"), nil
		}
		return strings.Join(names, "\n"), nil
	}
	return "", nil
}
func (f *tunnelFake) WriteFile(_ context.Context, p string, d []byte, m os.FileMode, _ bool) error {
	if f.written == nil {
		f.written, f.modes = map[string][]byte{}, map[string]os.FileMode{}
	}
	f.written[p], f.modes[p] = d, m
	return nil
}
func (f *tunnelFake) ReadFile(context.Context, string, bool) ([]byte, error) {
	return nil, os.ErrNotExist
}
func (f *tunnelFake) Close() error { return nil }

func fastPoll(t *testing.T) {
	old := tunnelPollInterval
	tunnelPollInterval = time.Millisecond
	t.Cleanup(func() { tunnelPollInterval = old })
}

func TestEnsureTunnelQuickCreates(t *testing.T) {
	fastPoll(t)
	h := &tunnelFake{logs: quickLogs, containers: map[string]string{}}
	var out bytes.Buffer
	st, err := EnsureTunnel(context.Background(), h, config.TunnelConfig{}, "", &out)
	if err != nil {
		t.Fatal(err)
	}
	all := strings.ReplaceAll(strings.Join(h.scripts, "\n"), "'", "")
	if !strings.Contains(all, "docker run -d") || !strings.Contains(all, "--url http://yoho-proxy:80") || !strings.Contains(all, DefaultTunnelImage) {
		t.Errorf("scripts:\n%s", all)
	}
	if len(h.written) != 0 {
		t.Error("quick tunnel writes no token file")
	}
	if !strings.Contains(out.String(), "https://random-words-here-1234.trycloudflare.com") {
		t.Errorf("out: %s", out.String())
	}
	_ = st
}

func TestEnsureTunnelSkipNetwork(t *testing.T) {
	fastPoll(t)
	for _, skip := range []bool{false, true} {
		h := &tunnelFake{logs: quickLogs, containers: map[string]string{}}
		if _, err := EnsureTunnelWith(context.Background(), h, config.TunnelConfig{}, "", nil, BootOptions{SkipNetwork: skip}); err != nil {
			t.Fatal(err)
		}
		all := strings.Join(h.scripts, "\n")
		if got := strings.Contains(all, "docker network"); got == skip {
			t.Errorf("SkipNetwork=%v: docker network used=%v\n%s", skip, got, all)
		}
		if !strings.Contains(all, "docker run -d") {
			t.Errorf("connector not started:\n%s", all)
		}
	}
}

func TestEnsureTunnelQuickKeepsMatching(t *testing.T) {
	fastPoll(t)
	h := &tunnelFake{logs: quickLogs, containers: map[string]string{TunnelContainer: tunnelHash(DefaultTunnelImage, "")}}
	st, err := EnsureTunnel(context.Background(), h, config.TunnelConfig{}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(h.scripts, "\n"), "docker run") || strings.Contains(strings.Join(h.scripts, "\n"), "docker rm") {
		t.Error("matching container must be kept")
	}
	if st.URL != "https://random-words-here-1234.trycloudflare.com" || st.Mode != "quick" {
		t.Errorf("status %+v", st)
	}
}

func TestEnsureTunnelQuickRejectsReplicas(t *testing.T) {
	if _, err := EnsureTunnel(context.Background(), &tunnelFake{}, config.TunnelConfig{Replicas: 2}, "", nil); err == nil {
		t.Error("expected error")
	}
}

func TestEnsureTunnelTokenNeverInScripts(t *testing.T) {
	fastPoll(t)
	const token = "eyJhIjoiU0VDUkVUVE9LRU4ifQ"
	h := &tunnelFake{logs: "INF Registered tunnel connection connIndex=0\n", containers: map[string]string{}}
	var out bytes.Buffer
	if _, err := EnsureTunnel(context.Background(), h, config.TunnelConfig{TokenSecret: "T", Replicas: 2}, token, &out); err != nil {
		t.Fatal(err)
	}
	for _, s := range h.scripts {
		if strings.Contains(s, token) {
			t.Errorf("token in script: %s", s)
		}
	}
	if strings.Contains(out.String(), token) {
		t.Error("token in output")
	}
	p := TokenEnvPath()
	if string(h.written[p]) != "TUNNEL_TOKEN="+token+"\n" || h.modes[p] != 0o600 {
		t.Errorf("env file %q mode %v", h.written[p], h.modes[p])
	}
	all := strings.ReplaceAll(strings.Join(h.scripts, "\n"), "'", "")
	for _, w := range []string{"--name yoho-tunnel-1", "--name yoho-tunnel-2", "--env-file", "tunnel --no-autoupdate run"} {
		if !strings.Contains(all, w) {
			t.Errorf("missing %q", w)
		}
	}
	if !strings.Contains(out.String(), "http://yoho-proxy:80") {
		t.Error("dashboard instruction missing")
	}
}

func TestEnsureTunnelTokenRollingReplace(t *testing.T) {
	fastPoll(t)
	h := &tunnelFake{logs: "INF Registered tunnel connection connIndex=0\n", containers: map[string]string{
		"yoho-tunnel-1": "old", "yoho-tunnel-2": "old",
	}}
	if _, err := EnsureTunnel(context.Background(), h, config.TunnelConfig{Replicas: 2}, "tok", nil); err != nil {
		t.Fatal(err)
	}
	var runs []string
	for _, s := range h.scripts {
		if strings.Contains(s, "docker rm -f yoho-tunnel-1") {
			runs = append(runs, "1")
		}
		if strings.Contains(s, "docker rm -f yoho-tunnel-2") {
			runs = append(runs, "2")
		}
	}
	if strings.Join(runs, "") != "12" {
		t.Errorf("expected one-at-a-time replace, got %v", runs)
	}
}

func TestRemoveTunnel(t *testing.T) {
	h := &tunnelFake{containers: map[string]string{"yoho-tunnel": "x"}}
	if err := RemoveTunnel(context.Background(), h, nil); err != nil {
		t.Fatal(err)
	}
	last := h.scripts[len(h.scripts)-1]
	if !strings.Contains(last, "docker rm -f yoho-tunnel") || !strings.Contains(last, "token.env") {
		t.Errorf("script: %s", last)
	}
}

func TestTunnelRunArgsOwnerLabel(t *testing.T) {
	q := strings.Join(tunnelRunArgs("yoho-tunnel", "img:1", "h", TunnelOwner("shop", "prod"), true), " ")
	if !strings.Contains(q, "yoho.tunnel.owner=shop/prod") {
		t.Errorf("owner label missing: %s", q)
	}
	if strings.Contains(strings.Join(tunnelRunArgs("yoho-tunnel", "img:1", "h", "", true), " "), "owner") {
		t.Error("no owner label without owner")
	}
}

func TestRemoveTunnelOwnedKeepsOtherAppsConnector(t *testing.T) {
	for name, tc := range map[string]struct {
		owners  map[string]string
		removed bool
	}{
		"other app": {map[string]string{"yoho-tunnel": "shop/prod"}, false},
		"legacy":    {map[string]string{}, false},
		"mine":      {map[string]string{"yoho-tunnel": "blog/prod"}, true},
	} {
		h := &tunnelFake{containers: map[string]string{"yoho-tunnel": "x"}, owners: tc.owners}
		got, err := RemoveTunnelOwned(context.Background(), h, TunnelOwner("blog", "prod"), nil)
		if err != nil || got != tc.removed {
			t.Fatalf("%s: removed=%v err=%v", name, got, err)
		}
		if rm := strings.Contains(strings.Join(h.scripts, "\n"), "docker rm -f"); rm != tc.removed {
			t.Errorf("%s: docker rm executed=%v, want %v", name, rm, tc.removed)
		}
	}
}

func TestEnsureTunnelRefusesToReplaceOtherAppsConnector(t *testing.T) {
	fastPoll(t)
	h := &tunnelFake{
		logs:       "INF Registered tunnel connection connIndex=0\n",
		containers: map[string]string{"yoho-tunnel": "old"},
		owners:     map[string]string{"yoho-tunnel": "shop/prod"},
	}
	_, err := EnsureTunnelWith(context.Background(), h, config.TunnelConfig{TokenSecret: "T"}, "tok", nil, BootOptions{Owner: "blog/prod"})
	if err == nil || !strings.Contains(err.Error(), "managed by shop/prod") {
		t.Fatalf("err = %v", err)
	}
	if s := strings.Join(h.scripts, "\n"); strings.Contains(s, "docker rm") || strings.Contains(s, "docker run") {
		t.Errorf("must not touch the connector:\n%s", s)
	}
	// Same config as the owner's: nothing to change, no error.
	h.containers["yoho-tunnel"] = tunnelHash(DefaultTunnelImage, "tok")
	if _, err := EnsureTunnelWith(context.Background(), h, config.TunnelConfig{TokenSecret: "T"}, "tok", nil, BootOptions{Owner: "blog/prod"}); err != nil {
		t.Errorf("identical config should be left alone: %v", err)
	}
}

func TestEnsureTunnelEmptyTokenForManagedTunnelFails(t *testing.T) {
	h := &tunnelFake{containers: map[string]string{"yoho-tunnel-1": "x"}}
	_, err := EnsureTunnel(context.Background(), h, config.TunnelConfig{TokenSecret: "CF_TOKEN"}, "", nil)
	if err == nil || !strings.Contains(err.Error(), "empty token") {
		t.Fatalf("err = %v", err)
	}
	if len(h.scripts) != 0 {
		t.Errorf("must not touch the Server: %v", h.scripts)
	}
}

func TestTunnelStatusReportsHashAndOwner(t *testing.T) {
	h := &tunnelFake{containers: map[string]string{"yoho-tunnel": "abc"}, owners: map[string]string{"yoho-tunnel": "shop/prod"}}
	st, err := TunnelStatusOf(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	if st.Owner != "shop/prod" || st.Containers[0].ConfigHash != "abc" || !st.OwnedBy("shop/prod") || st.OwnedBy("blog/prod") {
		t.Errorf("%+v", st)
	}
}

func TestEnsureTunnelStaleForeignConnectorRefusesBeforeCreating(t *testing.T) {
	fastPoll(t)
	h := &tunnelFake{
		logs:       "INF Registered tunnel connection connIndex=0\n",
		containers: map[string]string{"yoho-tunnel": "old"},
		owners:     map[string]string{"yoho-tunnel": "shop/prod"},
	}
	_, err := EnsureTunnelWith(context.Background(), h, config.TunnelConfig{TokenSecret: "T", Replicas: 2}, "tok", nil, BootOptions{Owner: "blog/prod"})
	if err == nil || !strings.Contains(err.Error(), "managed by shop/prod") {
		t.Fatalf("err = %v", err)
	}
	if s := strings.Join(h.scripts, "\n"); strings.Contains(s, "docker rm") || strings.Contains(s, "docker run") {
		t.Errorf("a refusal must not create or remove anything:\n%s", s)
	}
}
