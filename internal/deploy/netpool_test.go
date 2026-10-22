package deploy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/config"
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
	if err := checkSubnetChange("p", withSubnet, nil); err != nil {
		t.Errorf("first deploy: %v", err)
	}
	if err := checkSubnetChange("p", withSubnet, &currentState{subnet: "10.200.7.0/24"}); err != nil {
		t.Errorf("unchanged: %v", err)
	}
	err := checkSubnetChange("p", withSubnet, &currentState{subnet: defaultSubnet(without)})
	var sc *SubnetChangeError
	if !errors.As(err, &sc) || sc.New != "10.200.7.0/24" || sc.Old != "" {
		t.Errorf("adding a subnet to a deployed App: %v", err)
	}
	if err := checkSubnetChange("p", without, &currentState{subnet: "10.200.7.0/24"}); err == nil {
		t.Error("removing a subnet must be refused too")
	}
}
