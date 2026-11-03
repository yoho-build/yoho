package proxy

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/remote"
)

func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
		ok   bool
	}{
		{"v0.9.2", "v0.10.2", -1, true},
		{"v0.10.2", "v0.10.2", 0, true},
		{"v0.10.10", "v0.10.2", 1, true},
		{"v0.10.0-rc1", "v0.10.0", -1, true},
		{"v0.10.0", "v0.10.0-rc1", 1, true},
		{"0.8.8-rc1", "v0.8.8-rc2", -1, true},
		{"latest", "v0.10.2", 0, false},
		{"", "v0.10.2", 0, false},
	} {
		got, ok := CompareVersions(c.a, c.b)
		if got != c.want || ok != c.ok {
			t.Errorf("CompareVersions(%q,%q) = %d,%v want %d,%v", c.a, c.b, got, ok, c.want, c.ok)
		}
	}
	if CompareImages("basecamp/kamal-proxy:v0.10.2", "reg:5000/kamal-proxy:v0.9.0") <= 0 {
		t.Error("image tags not compared")
	}
	if VersionLabel("reg:5000/kamal-proxy") != "reg:5000/kamal-proxy" || VersionLabel(DefaultImage) != "v0.10.2" {
		t.Error("VersionLabel")
	}
}

// The image is part of the configuration fingerprint, so a changed image
// recreates the Proxy and the plan names the version change.
func TestImageInFingerprintAndPlanReason(t *testing.T) {
	old := config.ProxyConfig{Image: "basecamp/kamal-proxy:v0.9.0"}
	if configHash(runArgs(old)) == configHash(runArgs(config.ProxyConfig{})) {
		t.Fatal("image is not part of the config fingerprint")
	}
	h := &fakeHost{output: func(string) string {
		return configHash(runArgs(old)) + "|true|basecamp/kamal-proxy:v0.9.0"
	}}
	need, why, err := NeedsBoot(context.Background(), h, config.ProxyConfig{})
	if err != nil || !need || !strings.HasPrefix(why, "upgrade v0.9.0 -> v0.10.2") {
		t.Fatalf("need=%v why=%q err=%v", need, why, err)
	}
	h.output = func(string) string {
		return configHash(runArgs(config.ProxyConfig{})) + "|true|" + DefaultImage
	}
	if need, _, _ := NeedsBoot(context.Background(), h, old); !need {
		t.Fatal("downgrade not detected")
	} else if _, why, _ := NeedsBoot(context.Background(), h, old); !strings.HasPrefix(why, "downgrade v0.10.2 -> v0.9.0") {
		t.Fatalf("why=%q", why)
	}
}

// upgradeHost scripts a Proxy running image `image` with the given routes.
type upgradeHost struct {
	fakeHost
	image    string
	cfg      config.ProxyConfig
	pullErr  error
	listJSON func() string
	probe    func() string
	order    []string
}

func (u *upgradeHost) Run(ctx context.Context, c remote.Cmd) error {
	switch {
	case strings.Contains(c.Script, "docker stop"):
		u.order = append(u.order, "stop")
	case strings.Contains(c.Script, "docker pull"):
		u.order = append(u.order, "pull")
		return u.pullErr
	}
	return u.fakeHost.Run(ctx, c)
}

func (u *upgradeHost) Output(_ context.Context, c remote.Cmd) (string, error) {
	switch {
	case strings.Contains(c.Script, "container inspect"):
		return configHash(runArgs(config.ProxyConfig{Image: u.image})) + "|true|" + u.image, nil
	case strings.Contains(c.Script, "LoggingDriver"):
		return "json-file", nil
	case strings.Contains(c.Script, "kamal-proxy list"):
		return u.listJSON(), nil
	case strings.Contains(c.Script, "curl"):
		return u.probe(), nil
	}
	return "", nil
}

const listTwo = `{"a-production-web":{"hosts":["a.test"],"path_prefixes":["/"],"targets":["a-web-1:80"],"state":"running"},` +
	`"b-production-web":{"hosts":[],"path_prefixes":["/"],"targets":["b-web-1:80"],"state":"running"}}`

func newUpgradeHost(image string) *upgradeHost {
	return &upgradeHost{image: image, listJSON: func() string { return listTwo }, probe: func() string { return "a-production-web 200\nb-production-web 404\n" }}
}

func TestUpgradePullsBeforeStopping(t *testing.T) {
	u := newUpgradeHost("basecamp/kamal-proxy:v0.9.0")
	res, err := Upgrade(context.Background(), u, config.ProxyConfig{}, nil, UpgradeOptions{Interval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || res.Routes != 2 {
		t.Fatalf("result %+v", res)
	}
	if len(u.order) < 2 || u.order[0] != "pull" || u.order[1] != "stop" {
		t.Fatalf("order %v: pull must come before stop", u.order)
	}
}

func TestUpgradePullFailureStopsNothing(t *testing.T) {
	u := newUpgradeHost("basecamp/kamal-proxy:v0.9.0")
	u.pullErr = errors.New("registry unreachable")
	hooks := 0
	_, err := Upgrade(context.Background(), u, config.ProxyConfig{}, nil, UpgradeOptions{OnReboot: func(context.Context, string) error { hooks++; return nil }})
	if err == nil || !strings.Contains(err.Error(), "not touched") {
		t.Fatalf("err=%v", err)
	}
	if hooks != 0 {
		t.Error("Hook ran before the pull succeeded")
	}
	for _, s := range u.scripts {
		if strings.Contains(s, "docker stop") || strings.Contains(s, "docker rename") || strings.Contains(s, "docker run") {
			t.Fatalf("Proxy was changed after a failed pull: %s", s)
		}
	}
}

func TestUpgradeRefusesDowngrade(t *testing.T) {
	u := newUpgradeHost(DefaultImage)
	cfg := config.ProxyConfig{Image: "basecamp/kamal-proxy:v0.9.0"}
	_, err := Upgrade(context.Background(), u, cfg, nil, UpgradeOptions{})
	if err == nil || !strings.Contains(err.Error(), "--allow-downgrade") {
		t.Fatalf("err=%v", err)
	}
	if len(u.order) != 0 {
		t.Fatalf("downgrade refusal still acted: %v", u.order)
	}
	if _, err := Upgrade(context.Background(), u, cfg, nil, UpgradeOptions{AllowDowngrade: true, Interval: time.Millisecond}); err != nil {
		t.Fatalf("allowed downgrade: %v", err)
	}
}

func TestUpgradeNoopAndMissing(t *testing.T) {
	u := newUpgradeHost(DefaultImage)
	res, err := Upgrade(context.Background(), u, config.ProxyConfig{}, nil, UpgradeOptions{})
	if err != nil || res.Changed || len(u.order) != 0 {
		t.Fatalf("res=%+v err=%v order=%v", res, err, u.order)
	}
	h := &fakeHost{}
	if _, err := Upgrade(context.Background(), h, config.ProxyConfig{}, nil, UpgradeOptions{}); err == nil {
		t.Fatal("missing Proxy accepted")
	}
}

func TestUpgradeVerificationFailsOnLostRoute(t *testing.T) {
	u := newUpgradeHost("basecamp/kamal-proxy:v0.9.0")
	calls := 0
	u.listJSON = func() string {
		calls++
		if calls == 1 { // before the swap
			return listTwo
		}
		return `{"a-production-web":{"hosts":["a.test"],"path_prefixes":["/"],"targets":["a-web-1:80"],"state":"running"}}`
	}
	post := 0
	_, err := Upgrade(context.Background(), u, config.ProxyConfig{}, nil, UpgradeOptions{Interval: time.Millisecond, Timeout: 20 * time.Millisecond,
		OnReboot: func(_ context.Context, h string) error {
			if h == "post-proxy-reboot" {
				post++
			}
			return nil
		}})
	if err == nil || !strings.Contains(err.Error(), "b-production-web is missing") {
		t.Fatalf("err=%v", err)
	}
	if post != 0 {
		t.Error("post-proxy-reboot Hook ran although routes were not verified")
	}
}

type noJSONHost struct {
	*upgradeHost
	swapped bool
}

func (n *noJSONHost) Run(ctx context.Context, c remote.Cmd) error {
	if strings.Contains(c.Script, "docker stop") {
		n.swapped = true
	}
	return n.upgradeHost.Run(ctx, c)
}

func (n *noJSONHost) Output(ctx context.Context, c remote.Cmd) (string, error) {
	if !n.swapped && strings.Contains(c.Script, "kamal-proxy list") {
		return "", errors.New("Error: unknown flag: --json")
	}
	return n.upgradeHost.Output(ctx, c)
}

func TestUpgradeFromProxyWithoutListJSON(t *testing.T) {
	n := &noJSONHost{upgradeHost: newUpgradeHost("basecamp/kamal-proxy:v0.9.0")}
	var out strings.Builder
	if _, err := Upgrade(context.Background(), n, config.ProxyConfig{}, &out, UpgradeOptions{Interval: time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "routes cannot be compared") {
		t.Errorf("no warning: %s", out.String())
	}
	// A current version failing to list stops before any change.
	n = &noJSONHost{upgradeHost: newUpgradeHost("basecamp/kamal-proxy:v0.10.0")}
	if _, err := Upgrade(context.Background(), n, config.ProxyConfig{Image: "basecamp/kamal-proxy:v0.10.1"}, nil, UpgradeOptions{}); err == nil || n.swapped {
		t.Fatalf("err=%v swapped=%v", err, n.swapped)
	}
}

func TestDiffRoutesAndCompareProbes(t *testing.T) {
	a, _ := ParseRoutes(listTwo)
	if d := DiffRoutes(a, a); d != nil {
		t.Fatalf("identical: %v", d)
	}
	b, _ := ParseRoutes(strings.Replace(listTwo, "b-web-1:80", "b-web-2:80", 1))
	if d := DiffRoutes(a, b); len(d) != 1 || !strings.Contains(d[0], "changed targets") {
		t.Fatalf("retarget: %v", d)
	}
	p, un := CompareProbes(map[string]int{"x": 200, "y": 503, "z": 404}, map[string]int{"x": 0, "y": 503, "z": 404})
	if len(p) != 1 || !strings.Contains(p[0], "route x answered HTTP 200 before, no answer now") || len(un) != 1 || un[0] != "y" {
		t.Fatalf("problems=%v unverified=%v", p, un)
	}
}

func TestParseProbeAndScript(t *testing.T) {
	m, err := ParseProbe("a-production-web 200\nb 000\n\n")
	if err != nil || m["a-production-web"] != 200 || m["b"] != 0 || len(m) != 2 {
		t.Fatalf("%v %v", m, err)
	}
	if m, err := ParseProbe("NOCURL\n"); err != nil || len(m) != 0 {
		t.Fatalf("NOCURL: %v %v", m, err)
	}
	if _, err := ParseProbe("garbage"); err == nil {
		t.Error("garbage accepted")
	}
	routes, _ := ParseRoutes(listTwo)
	s := ProbeScript("http://127.0.0.1:18080", routes, map[string]string{"a-production-web": "/healthz"})
	for _, want := range []string{"'Host: a.test'", "http://127.0.0.1:18080/healthz", "http://127.0.0.1:18080/up"} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "Host: *") {
		t.Error("catch-all route sent a Host header")
	}
}

func TestStatusSummary(t *testing.T) {
	for _, c := range []struct {
		s    StatusInfo
		want string
	}{
		{StatusInfo{Exists: true, Image: DefaultImage, Want: DefaultImage}, "up to date"},
		{StatusInfo{Exists: true, Image: "basecamp/kamal-proxy:v0.9.0", Want: DefaultImage}, "upgrade available: v0.10.2"},
		{StatusInfo{Exists: true, Image: DefaultImage, Want: "basecamp/kamal-proxy:v0.9.0"}, "--allow-downgrade"},
		{StatusInfo{Want: DefaultImage}, "not running"},
	} {
		if got := c.s.Summary(); !strings.Contains(got, c.want) {
			t.Errorf("%+v: %q lacks %q", c.s, got, c.want)
		}
	}
}

var _ = os.ErrNotExist
