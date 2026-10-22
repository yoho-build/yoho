package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

type probeHost struct {
	dockerHost
	out string
	err error
}

func (p *probeHost) Output(_ context.Context, _ remote.Cmd) (string, error) { return p.out, p.err }

func poolErr(msg string) error {
	return fmt.Errorf("up: %w", &remote.ExitError{Host: "s1", Code: 1, Stderr: "Error response from daemon: " + msg})
}

func TestIsPoolExhausted(t *testing.T) {
	for _, m := range []string{"all predefined address pools have been fully subnetted", "could not find an available, non-overlapping IPv4 address pool among the defaults"} {
		if !IsPoolExhausted(poolErr(m)) {
			t.Errorf("%q not detected", m)
		}
	}
	for _, err := range []error{poolErr("network foo not found"), errors.New("all predefined address pools have been fully subnetted"), nil} {
		if IsPoolExhausted(err) {
			t.Errorf("false positive for %v", err)
		}
	}
}

func TestSuggestSubnetSkipsUsedAndRoutes(t *testing.T) {
	h := &probeHost{out: "172.17.0.0/16 \n10.200.0.0/24 10.200.1.0/25 \nfd00::/64\n10.200.2.0/23\ndefault\n10.200.5.1"}
	p, used, err := SuggestSubnet(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	if p.String() != "10.200.4.0/24" {
		t.Errorf("got %s (used %v)", p, used)
	}
}

func TestSuggestSubnetFullRange(t *testing.T) {
	h := &probeHost{out: "10.200.0.0/16"}
	if _, _, err := SuggestSubnet(context.Background(), h); err == nil {
		t.Fatal("want error when the range is full")
	}
}

func TestSuggestSubnetScriptIsReadOnly(t *testing.T) {
	for _, bad := range []string{"network create", "network rm", "network connect", "docker run"} {
		if strings.Contains(usedSubnetsScript, bad) {
			t.Errorf("probe script must be read-only, has %q", bad)
		}
	}
}

func TestAnnotatePoolExhausted(t *testing.T) {
	h := &probeHost{out: "10.200.0.0/24"}
	err := AnnotatePoolExhausted(context.Background(), h, "shop", nil, poolErr("all predefined address pools have been fully subnetted"))
	var f *Failure
	if !errors.As(err, &f) || f.Network == nil || f.Network.SuggestedSubnet != "10.200.1.0/24" {
		t.Fatalf("got %#v", err)
	}
	facts := f.FailureFacts().(map[string]any)["network"].(map[string]any)
	if facts["suggested_subnet"] != "10.200.1.0/24" || facts["name"] != "shop_default" {
		t.Errorf("facts %v", facts)
	}
	if !f.Network.Default || !strings.Contains(SnippetFor(f.Network.SuggestedSubnet), "- subnet: 10.200.1.0/24") {
		t.Errorf("hint %+v", f.Network)
	}
	// An existing Failure is annotated in place.
	existing := &Failure{Cause: poolErr("all predefined address pools have been fully subnetted"), Phase: "cutover"}
	if got := AnnotatePoolExhausted(context.Background(), h, "n", nil, existing); got != error(existing) || existing.Network == nil || existing.Phase != "cutover" {
		t.Errorf("not annotated in place: %#v", existing)
	}
	// Probe failure and unrelated errors are left alone.
	other := errors.New("boom")
	if AnnotatePoolExhausted(context.Background(), h, "n", nil, other) != other {
		t.Error("unrelated error changed")
	}
	bad := &probeHost{err: errors.New("ssh down")}
	orig := poolErr("all predefined address pools have been fully subnetted")
	if AnnotatePoolExhausted(context.Background(), bad, "n", nil, orig) != orig {
		t.Error("probe failure must keep the original error")
	}
}

func TestCompilePinsDefaultSubnet(t *testing.T) {
	d := testDeploy(t, nil, nil)
	d.Project = loadProject(t, "services:\n  web:\n    image: shop-web:v1\n")
	d.Ext = map[string]config.ServiceExt{"web": {}}
	d.ServiceSecrets = nil
	d.Subnet = "10.200.7.0/24"
	out, _, _ := compileTest(t, d)
	if !strings.Contains(string(out), "subnet: 10.200.7.0/24") {
		t.Errorf("subnet not applied:\n%s", out)
	}
	d.Project = loadProject(t, "services:\n  web:\n    image: shop-web:v1\nnetworks:\n  default:\n    ipam:\n      config:\n        - subnet: 192.168.50.0/24\n")
	out, _, _ = compileTest(t, d)
	if strings.Contains(string(out), "10.200.7.0") || !strings.Contains(string(out), "192.168.50.0/24") {
		t.Errorf("user ipam must win:\n%s", out)
	}
}

func TestFailedNetworkIsIdentified(t *testing.T) {
	const pool = "all predefined address pools have been fully subnetted"
	mk := func(script, stderr string) error {
		return &remote.ExitError{Host: "s1", Code: 1, Script: script, Stderr: stderr}
	}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"compose up", mk("docker compose up -d", "Error response from daemon: "+pool), "shop_default"},
		{"compose names it", mk("docker compose up -d", " Network shop_backend  Error\nError response from daemon: "+pool), "shop_backend"},
		{"stack deploy", mk("docker stack deploy x", "failed to create network shop_default: Error response from daemon: "+pool), "shop_default"},
		{"proxy network", mk("docker network inspect yoho || docker network create yoho", "Error response from daemon: "+pool), "yoho"},
		{"unknown create", mk("docker network create mystery", "Error response from daemon: "+pool), ""},
	}
	for _, tc := range cases {
		if got := failedNetwork(tc.err, "shop", []string{"yoho"}); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
	h := &probeHost{out: ""}
	err := AnnotatePoolExhausted(context.Background(), h, "shop", []string{"yoho"}, cases[3].err)
	var f *Failure
	if !errors.As(err, &f) || f.Network.Default || f.Network.Network != "yoho" {
		t.Fatalf("proxy network must not be labelled default: %+v", f)
	}
	facts := f.FailureFacts().(map[string]any)["network"].(map[string]any)
	if facts["default"] != false || facts["name"] != "yoho" {
		t.Errorf("facts %v", facts)
	}
}

func TestProbeScriptPropagatesFailures(t *testing.T) {
	if strings.Contains(usedSubnetsScript, "|| true") {
		t.Error("an inventory failure must fail the probe, not be ignored")
	}
	if !strings.HasPrefix(usedSubnetsScript, "set -eu") {
		t.Error("probe must run under set -eu")
	}
}

func TestSubnetChangeOnDeployedAppIsRefused(t *testing.T) {
	withSubnet := []byte("services: {}\nnetworks:\n  default:\n    ipam:\n      config:\n        - subnet: 10.200.7.0/24\n")
	without := []byte("services: {}\n")
	if err := checkSubnetChange("p", withSubnet, "", false); err != nil {
		t.Errorf("first deploy: %v", err)
	}
	if err := checkSubnetChange("p", withSubnet, "10.200.7.0/24", true); err != nil {
		t.Errorf("unchanged: %v", err)
	}
	err := checkSubnetChange("p", withSubnet, "", true)
	var sc *SubnetChangeError
	if !errors.As(err, &sc) || sc.New != "10.200.7.0/24" || sc.Old != "" {
		t.Errorf("adding a subnet to a deployed App: %v", err)
	}
	if err := checkSubnetChange("p", without, "10.200.7.0/24", true); err == nil {
		t.Error("removing a subnet must be refused too")
	}
}

// A current Release whose compose file cannot be read or parsed must not
// pass as "no current Release": that would let a subnet change through.
func TestCurrentSubnetFailsClosed(t *testing.T) {
	ctx := context.Background()
	h := &remote.Local{}
	withSubnet := []byte("services: {}\nnetworks:\n  default:\n    ipam:\n      config:\n        - subnet: 10.200.7.0/24\n")
	setup := func(t *testing.T, compose []byte) {
		withRoot(t)
		dir := release.AppDir("shop", "production")
		rel := filepath.Join(dir, "releases", "v1")
		if err := os.MkdirAll(rel, 0o755); err != nil {
			t.Fatal(err)
		}
		if compose != nil {
			if err := os.WriteFile(filepath.Join(rel, "compose.yaml"), compose, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink(rel, filepath.Join(dir, "current")); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("no current Release", func(t *testing.T) {
		withRoot(t)
		if _, ok, err := currentSubnet(ctx, h, "shop", "production"); ok || err != nil {
			t.Fatalf("ok=%v err=%v", ok, err)
		}
	})
	t.Run("unchanged", func(t *testing.T) {
		setup(t, withSubnet)
		sub, ok, err := currentSubnet(ctx, h, "shop", "production")
		if err != nil || !ok || sub != "10.200.7.0/24" {
			t.Fatalf("sub=%q ok=%v err=%v", sub, ok, err)
		}
	})
	t.Run("compose file missing", func(t *testing.T) {
		setup(t, nil)
		if _, _, err := currentSubnet(ctx, h, "shop", "production"); err == nil {
			t.Fatal("unreadable compose file must fail")
		}
	})
	t.Run("compose file unparsable", func(t *testing.T) {
		setup(t, []byte("networks: [\n"))
		if _, _, err := currentSubnet(ctx, h, "shop", "production"); err == nil {
			t.Fatal("unparsable compose file must fail")
		}
	})
	t.Run("current is not a link", func(t *testing.T) {
		withRoot(t)
		if err := os.MkdirAll(filepath.Join(release.AppDir("shop", "production"), "current"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, _, err := currentSubnet(ctx, h, "shop", "production"); err == nil {
			t.Fatal("a current that is not a link must fail")
		}
	})
	t.Run("deploy refuses", func(t *testing.T) {
		setup(t, []byte("networks: [\n"))
		d := testDeploy(t, &dockerHost{local: h, respond: respond(nil)}, nil)
		d.Subnet = "10.200.7.0/24"
		if _, err := (Compose{}).Deploy(ctx, d); err == nil || !strings.Contains(err.Error(), "default network subnet") {
			t.Fatalf("deploy over an unreadable current Release: %v", err)
		}
	})
}

// stubHost runs the probe script locally with only the given stub commands
// on PATH.
type stubHost struct {
	dockerHost
	path string
}

func (s *stubHost) Output(ctx context.Context, c remote.Cmd) (string, error) {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", c.Script)
	cmd.Env = []string{"PATH=" + s.path}
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func stubCommands(t *testing.T, cmds map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range cmds {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const stubDocker = `case "$1 $2" in
"network ls") echo n1 ;;
"network inspect") echo "10.200.3.0/24 " ;;
esac
`

func TestProbeCountsTypedRoutes(t *testing.T) {
	ip := "printf '%s\\n' " +
		"'default via 192.168.1.1 dev eth0 proto dhcp' " +
		"'blackhole 10.200.0.0/24 proto static' " +
		"'unreachable 10.200.1.0/24' " +
		"'prohibit 10.200.2.0/24 metric 5' " +
		"'throw 10.200.4.0/24' " +
		"'10.200.5.0/24 dev br0 proto kernel scope link src 10.200.5.1'\n"
	h := &stubHost{path: stubCommands(t, map[string]string{"docker": stubDocker, "ip": ip})}
	p, used, err := SuggestSubnet(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	if p.String() != "10.200.6.0/24" {
		t.Errorf("got %s, used %v", p, used)
	}
}

func TestProbeFailsWithoutInventory(t *testing.T) {
	ip := "echo '10.0.0.0/8 dev eth0'\n"
	for name, cmds := range map[string]map[string]string{
		"no ip":        {"docker": stubDocker},
		"no docker":    {"ip": ip},
		"docker fails": {"ip": ip, "docker": "exit 1\n"},
		"ip fails":     {"docker": stubDocker, "ip": "exit 1\n"},
	} {
		h := &stubHost{path: stubCommands(t, cmds)}
		if p, _, err := SuggestSubnet(context.Background(), h); err == nil {
			t.Errorf("%s: probe must fail, suggested %s", name, p)
		}
		orig := poolErr("all predefined address pools have been fully subnetted")
		if got := AnnotatePoolExhausted(context.Background(), h, "shop", nil, orig); got != orig {
			t.Errorf("%s: no suggestion allowed, got %v", name, got)
		}
	}
}

func TestOutputTailFindsMergedPoolLine(t *testing.T) {
	const secret = "hunter2-secret"
	tail := &OutputTail{Redact: func(s string) string { return strings.ReplaceAll(s, secret, "***") }}
	_, _ = io.WriteString(tail, "using "+secret+"\nfailed to create network shop_default: Error response from daemon: all predefined address pools have been fully subnetted ("+secret+")\nother output\n")
	err := tail.Annotate(fmt.Errorf("x: %w", &remote.ExitError{Host: "s1", Code: 1}))
	if !IsPoolExhausted(err) || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "other output") {
		t.Fatalf("got %v", err)
	}
	if n := failedNetwork(err, "shop", nil); n != "shop_default" {
		t.Errorf("network %q", n)
	}
	plain := &OutputTail{}
	_, _ = io.WriteString(plain, "migration failed "+secret+"\n")
	orig := &remote.ExitError{Host: "s1", Code: 1}
	_ = plain.Annotate(orig)
	if orig.Stderr != "" {
		t.Errorf("unrelated output copied: %q", orig.Stderr)
	}
}

// A release command runs with 2>&1; Docker's pool message on the merged
// output must still be detected, and secrets around it must not leak.
func TestReleaseCommandPoolExhaustionDetected(t *testing.T) {
	withRoot(t)
	h := &poolOutputHost{dockerHost: &dockerHost{local: &remote.Local{}, respond: respond(nil)}}
	_, err := (Compose{}).Deploy(context.Background(), testDeploy(t, h, nil))
	if !IsPoolExhausted(err) {
		t.Fatalf("pool exhaustion missed: %v", err)
	}
	if strings.Contains(err.Error(), webSecret) {
		t.Errorf("error leaks a secret: %v", err)
	}
}

type poolOutputHost struct{ *dockerHost }

func (o *poolOutputHost) Run(ctx context.Context, c remote.Cmd) error {
	if strings.Contains(c.Script, "'run' '--rm'") && c.Stdout != nil {
		_, _ = io.WriteString(c.Stdout, "key="+webSecret+"\nError response from daemon: all predefined address pools have been fully subnetted "+webSecret+"\n")
		return &remote.ExitError{Host: "s1", Code: 1}
	}
	return o.dockerHost.Run(ctx, c)
}
