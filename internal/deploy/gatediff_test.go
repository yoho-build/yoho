package deploy

import (
	"context"
	"io"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/remote"
)

// hardenLikeGate applies what gate.Policy.Harden adds (the gate package
// imports this one, so the test repeats it).
func hardenLikeGate(p *types.Project) {
	for name, s := range p.Services {
		s.CapDrop = append(s.CapDrop, "NET_RAW")
		if s.PidsLimit == 0 {
			s.PidsLimit = 512
		}
		s.PullPolicy = types.PullPolicyNever
		if s.MemLimit == 0 {
			s.MemLimit = 1 << 30
		}
		if s.CPUS == 0 {
			s.CPUS = 1
		}
		p.Services[name] = s
	}
}

// gateDeployed deploys testDeploy hardened like the Gate, as a Member.
func gateDeployedState(t *testing.T) (*dockerHost, *fakeState) {
	t.Helper()
	root := withRoot(t)
	h := &dockerHost{local: &remote.Local{}, respond: respond(nil)}
	d := testDeploy(t, h, io.Discard)
	d.Performer = "alice (gate) alice@laptop"
	hardenLikeGate(d.Project)
	if _, err := (Compose{}).Deploy(context.Background(), d); err != nil {
		t.Fatalf("deploy: %v\n%s", err, h.all())
	}
	m := regexp.MustCompile(`yoho\.proxy\.config=([0-9a-f]+)`).FindStringSubmatch(h.all())
	if m == nil {
		t.Fatal("proxy was not booted")
	}
	st := &fakeState{containers: stateFromRelease(t, root), routes: goodRoutes, proxyState: m[1] + "|true"}
	h.respond = st.respond
	return h, st
}

// Issue #65: an operator plan against a Gate-deployed Release says the
// replace drops the Gate's hardening, not that the config changed.
func TestDiffOperatorAgainstGateHardenedRelease(t *testing.T) {
	h, _ := gateDeployedState(t)
	d := testDeploy(t, h, io.Discard)
	db := d.Project.Services["db"]
	db.Restart = "always"
	d.Project.Services["db"] = db
	cs, err := Compose{}.Diff(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	m := byKey(cs)
	want := GateHardeningReason + "cap_drop, cpus, mem_limit, pids_limit, pull_policy"
	if c := m["service cloudflared"]; c.Action != plan.ActionReplace || len(c.Reasons) != 1 || c.Reasons[0] != want {
		t.Errorf("cloudflared %+v", c)
	}
	// A real change keeps its own reason next to the hardening one.
	if c := m["service db"]; c.Action != plan.ActionReplace || len(c.Reasons) != 2 || c.Reasons[0] != "config changed: restart" || c.Reasons[1] != want {
		t.Errorf("db %+v", c)
	}
	if w := GateHardeningWarning(cs); !strings.Contains(w, "deployed through the Gate") || !strings.Contains(w, "YOHO_ACCESS=scoped yoho plan") {
		t.Errorf("warning %q", w)
	}

	// The same compose hardened again (what the Gate's plan verb compares)
	// is no change at all.
	d = testDeploy(t, h, io.Discard)
	d.Performer = "alice (gate)"
	hardenLikeGate(d.Project)
	cs, err = Compose{}.Diff(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		if c.Action != plan.ActionNoop {
			t.Errorf("hardened plan: unexpected change %+v", c)
		}
	}
	if GateHardeningWarning(cs) != "" {
		t.Error("no warning without hardening reasons")
	}
}

// Issues #65 and #68: a Service that only adds an x-yoho.check against a
// Gate-deployed Release. Hardened like the Gate (the plan verb) it is an
// update; from an operator it keeps exactly the hardening reason plus
// "check changed", and the check never shows as a hardened or config key.
func TestDiffCheckAgainstGateRelease(t *testing.T) {
	h, _ := gateDeployedState(t)
	withCheck := func(d *plan.Deploy) {
		cf := d.Ext["cloudflared"]
		cf.Check = &config.ServiceCheck{Type: "tcp", Port: 8080}
		d.Ext["cloudflared"] = cf
	}

	d := testDeploy(t, h, io.Discard)
	d.Performer = "alice (gate)"
	hardenLikeGate(d.Project)
	withCheck(d)
	cs, err := Compose{}.Diff(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	m := byKey(cs)
	if c := m["service cloudflared"]; c.Action != plan.ActionUpdate || c.Downtime || len(c.Reasons) != 1 || c.Reasons[0] != "check changed" {
		t.Errorf("Gate plan: cloudflared %+v", c)
	}
	for k, c := range m {
		if k != "service cloudflared" && c.Action != plan.ActionNoop {
			t.Errorf("Gate plan: unexpected change %+v", c)
		}
	}
	if w := GateHardeningWarning(cs); w != "" {
		t.Errorf("Gate plan: warning %q", w)
	}

	d = testDeploy(t, h, io.Discard)
	withCheck(d)
	cs, err = Compose{}.Diff(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{GateHardeningReason + "cap_drop, cpus, mem_limit, pids_limit, pull_policy", "check changed"}
	if c := byKey(cs)["service cloudflared"]; c.Action != plan.ActionReplace || !slices.Equal(c.Reasons, want) {
		t.Errorf("operator plan: cloudflared %+v, want reasons %q", c, want)
	}
	if GateHardeningWarning(cs) == "" {
		t.Error("operator plan: no Gate warning")
	}
}

// An explicit limit the user changes is a config change, not hardening.
func TestDiffExplicitLimitChangeAgainstGateRelease(t *testing.T) {
	h, _ := gateDeployedState(t)
	d := testDeploy(t, h, io.Discard)
	hardenLikeGate(d.Project)
	db := d.Project.Services["db"]
	db.MemLimit = 2 << 30
	d.Project.Services["db"] = db
	cs, err := Compose{}.Diff(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if c := byKey(cs)["service db"]; c.Action != plan.ActionReplace || len(c.Reasons) != 1 || c.Reasons[0] != "config changed: mem_limit" {
		t.Errorf("db %+v", c)
	}
}

// Without a Gate performer the same differences stay "config changed".
func TestDiffOperatorReleaseKeepsConfigChanged(t *testing.T) {
	root := withRoot(t)
	h := &dockerHost{local: &remote.Local{}, respond: respond(nil)}
	d := testDeploy(t, h, io.Discard)
	hardenLikeGate(d.Project)
	if _, err := (Compose{}).Deploy(context.Background(), d); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	m := regexp.MustCompile(`yoho\.proxy\.config=([0-9a-f]+)`).FindStringSubmatch(h.all())
	st := &fakeState{containers: stateFromRelease(t, root), routes: goodRoutes, proxyState: m[1] + "|true"}
	h.respond = st.respond
	cs, err := Compose{}.Diff(context.Background(), testDeploy(t, h, io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	if c := byKey(cs)["service db"]; c.Action != plan.ActionReplace || c.Reasons[0] != "config changed: cap_drop, cpus, mem_limit, pids_limit, pull_policy" {
		t.Errorf("db %+v", c)
	}
	if GateHardeningWarning(cs) != "" {
		t.Error("operator Release: no Gate warning")
	}
}

func TestHardenedOnly(t *testing.T) {
	for _, c := range []struct {
		key       string
		old, want any
		ok        bool
	}{
		{"cap_drop", []any{"NET_RAW"}, nil, true},
		{"cap_drop", []any{"SYS_ADMIN", "NET_RAW"}, []any{"SYS_ADMIN"}, true},
		{"cap_drop", []any{"NET_RAW"}, []any{"SYS_ADMIN"}, false},
		{"pull_policy", "never", nil, true},
		{"pull_policy", "never", "always", true},
		{"pull_policy", "always", nil, false},
		{"mem_limit", "1073741824", nil, true},
		{"mem_limit", "1073741824", "536870912", false},
		{"cpus", 1.0, nil, true},
		{"restart", "always", nil, false},
		{"tmpfs", []any{"/tmp:size=67108864"}, []any{"/tmp"}, true},
		{"tmpfs", []any{"/tmp:mode=1777,size=67108864"}, []any{"/tmp:mode=1777"}, true},
		{"tmpfs", []any{"/run"}, []any{"/tmp"}, false},
		{"volumes", []any{map[string]any{"type": "tmpfs", "target": "/t", "tmpfs": map[string]any{"size": 64}}}, []any{map[string]any{"type": "tmpfs", "target": "/t"}}, true},
		{"volumes", []any{map[string]any{"type": "volume", "target": "/a"}}, []any{map[string]any{"type": "volume", "target": "/b"}}, false},
		{"deploy", map[string]any{"resources": map[string]any{"limits": map[string]any{"memory": "1g", "cpus": 1.0, "pids": 512}}}, map[string]any{"resources": map[string]any{"limits": map[string]any{"memory": "1g"}}}, true},
		{"deploy", map[string]any{"resources": map[string]any{"limits": map[string]any{"memory": "1g"}}}, map[string]any{"resources": map[string]any{"limits": map[string]any{"memory": "2g"}}}, false},
		{"deploy", map[string]any{"replicas": 2, "resources": map[string]any{"limits": map[string]any{"memory": "1g"}}}, map[string]any{"replicas": 3}, false},
	} {
		if got := hardenedOnly(c.key, c.old, c.want); got != c.ok {
			t.Errorf("%s %v vs %v: %v, want %v", c.key, c.old, c.want, got, c.ok)
		}
	}
}
