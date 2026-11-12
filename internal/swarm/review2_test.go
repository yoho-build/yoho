package swarm

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/plan"
)

func setWebImage(d *plan.Deploy, img string) {
	s := d.Project.Services["web"]
	s.Image = img
	d.Project.Services["web"] = s
}

// A failed redeploy of the Version `current` points at must keep that
// Release's stack file, plan and record: Swarm rolled the tasks back to the
// previous spec, and plan and rollback compare against these files.
func TestRedeploySameVersionFailureKeepsRelease(t *testing.T) {
	root := withRoot(t)
	fastPolling(t)
	ctx := context.Background()
	sim := newSim()
	h := newFake(sim)
	if _, err := (Runtime{}).Deploy(ctx, testDeploy(t, h, nil)); err != nil {
		t.Fatal(err)
	}
	relDir := filepath.Join(root, "apps", "shop", "production", "releases", "v1")
	files := []string{"compose.yaml", "plan.json", "release.json"}
	before := map[string][]byte{}
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(relDir, f))
		if err != nil {
			t.Fatal(err)
		}
		before[f] = b
	}

	sim.failMain = true
	d := testDeploy(t, h, nil)
	setWebImage(d, "shop-web:other")
	if _, err := (Runtime{}).Deploy(ctx, d); err == nil {
		t.Fatal("want error")
	}
	for _, f := range files {
		b, _ := os.ReadFile(filepath.Join(relDir, f))
		if !bytes.Equal(b, before[f]) {
			t.Errorf("%s changed by the failed redeploy", f)
		}
	}
	rels, err := Runtime{}.Releases(ctx, d)
	if err != nil || len(rels) != 1 || rels[0].Status != "deployed" {
		t.Fatalf("releases %+v %v", rels, err)
	}

	// A successful redeploy replaces the record.
	sim.failMain = false
	if _, err := (Runtime{}).Deploy(ctx, d); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(relDir, "compose.yaml")); bytes.Equal(b, before["compose.yaml"]) {
		t.Error("successful redeploy did not write the new stack file")
	}
}

// A changed release_command is stripped from the stack file; the plan must
// still report it so apply rolls out and runs the command.
func TestDiffReleaseCommandChange(t *testing.T) {
	d, _ := currentFixture(t)
	web := d.Ext["web"]
	web.ReleaseCommand = []string{"bin/rails", "db:migrate", "db:seed"}
	d.Ext["web"] = web
	changes, err := Runtime{}.Diff(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	ch := findChange(changes, "service", "web")
	if ch.Action != plan.ActionUpdate || ch.Downtime || !contains(ch.Reasons, "release_command changed") {
		t.Fatalf("web: %+v", ch)
	}
}

// Failed Releases must not push the last good ones out of retention.
func TestPruneKeepsGoodReleasesAfterFailures(t *testing.T) {
	root := withRoot(t)
	fastPolling(t)
	ctx := context.Background()
	sim := newSim()
	h := newFake(sim)
	clock := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	oldNow := now
	now = func() time.Time { clock = clock.Add(time.Minute); return clock }
	t.Cleanup(func() { now = oldNow })

	deployVer := func(v string, fail bool) {
		sim.failMain = fail
		d := testDeploy(t, h, nil)
		d.Version = v
		setWebImage(d, "shop-web:"+v)
		if _, err := (Runtime{}).Deploy(ctx, d); (err != nil) != fail {
			t.Fatalf("%s: %v", v, err)
		}
	}
	deployVer("v1", false)
	deployVer("v2", false)
	deployVer("f1", true)
	deployVer("f2", true)
	deployVer("v3", false)
	rels := filepath.Join(root, "apps", "shop", "production", "releases")
	for v, want := range map[string]bool{"v3": true, "v2": true, "f2": true, "v1": false, "f1": false} {
		_, err := os.Stat(filepath.Join(rels, v))
		if (err == nil) != want {
			t.Errorf("release %s present=%v, want %v", v, err == nil, want)
		}
	}
}

// stackDeploys counts main stack deploys of the v1 stack file.
func stackDeploys(h *fakeHost) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, s := range h.scripts {
		if strings.Contains(s, "docker stack deploy") && strings.Contains(s, "releases/v1/compose.yaml") {
			n++
		}
	}
	return n
}

func serviceActions(t *testing.T, d *plan.Deploy) map[string]plan.Action {
	t.Helper()
	changes, err := Runtime{}.Diff(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]plan.Action{}
	for _, c := range changes {
		if c.Kind == "service" {
			out[c.Name] = c.Action
		}
	}
	return out
}

// Swarm rolls back only the Services whose update failed. A failed
// same-Version redeploy re-deploys the restored stack file so the converged
// Services do not keep a spec the restored record does not describe.
func TestRedeploySameVersionFailureRestoresStack(t *testing.T) {
	root := withRoot(t)
	fastPolling(t)
	ctx := context.Background()
	sim := newSim()
	h := newFake(sim)
	if _, err := (Runtime{}).Deploy(ctx, testDeploy(t, h, nil)); err != nil {
		t.Fatal(err)
	}
	relDir := filepath.Join(root, "apps", "shop", "production", "releases", "v1")
	before, _ := os.ReadFile(filepath.Join(relDir, "compose.yaml"))
	h.scripts = nil

	sim.failOnce = true
	d := testDeploy(t, h, nil)
	setWebImage(d, "shop-web:other")
	if _, err := (Runtime{}).Deploy(ctx, d); err == nil {
		t.Fatal("want error")
	}
	if n := stackDeploys(h); n != 2 {
		t.Fatalf("want the attempt and the restore, got %d stack deploys:\n%s", n, h.all())
	}
	if b, _ := os.ReadFile(filepath.Join(relDir, "compose.yaml")); !bytes.Equal(b, before) {
		t.Error("stack file not restored")
	}
	if _, err := os.Stat(filepath.Join(relDir, partialMarker)); !os.IsNotExist(err) {
		t.Errorf("marker after a successful restore: %v", err)
	}
	for svc, a := range serviceActions(t, testDeploy(t, h, nil)) {
		if a != plan.ActionNoop {
			t.Errorf("%s %s, want noop: the stack runs the record", svc, a)
		}
	}
}

// When the restore fails too, the record cannot be trusted: a marker makes
// the next plan update every Service, and a successful deploy clears it.
func TestRedeploySameVersionFailedRestoreMarksPartial(t *testing.T) {
	root := withRoot(t)
	fastPolling(t)
	ctx := context.Background()
	sim := newSim()
	h := newFake(sim)
	if _, err := (Runtime{}).Deploy(ctx, testDeploy(t, h, nil)); err != nil {
		t.Fatal(err)
	}
	relDir := filepath.Join(root, "apps", "shop", "production", "releases", "v1")
	h.scripts = nil

	sim.failMain = true
	d := testDeploy(t, h, nil)
	setWebImage(d, "shop-web:other")
	if _, err := (Runtime{}).Deploy(ctx, d); err == nil {
		t.Fatal("want error")
	}
	if n := stackDeploys(h); n != 2 {
		t.Fatalf("want the attempt and the restore, got %d stack deploys", n)
	}
	if _, err := os.Stat(filepath.Join(relDir, partialMarker)); err != nil {
		t.Fatalf("no marker: %v", err)
	}
	// Same config as the record: still every Service is an update.
	for _, svc := range []string{"cloudflared", "db", "web", "worker"} {
		if a := serviceActions(t, testDeploy(t, h, nil))[svc]; a != plan.ActionUpdate && a != plan.ActionReplace {
			t.Errorf("%s %s, want update", svc, a)
		}
	}

	sim.failMain = false
	if _, err := (Runtime{}).Deploy(ctx, testDeploy(t, h, nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(relDir, partialMarker)); !os.IsNotExist(err) {
		t.Errorf("marker survived a successful deploy: %v", err)
	}
	for svc, a := range serviceActions(t, testDeploy(t, h, nil)) {
		if a != plan.ActionNoop {
			t.Errorf("%s %s after a good deploy", svc, a)
		}
	}
}

// plan.json of v0.1.0-rc.3 and earlier has no mode; a global Service was
// recorded with replicas 0 and was routed. Rolling back to such a Release
// keeps its route.
func TestLegacyPlanKeepsGlobalRoute(t *testing.T) {
	const legacy = `{"stack":"yoho-shop-production","services":[
		{"name":"edge","replicas":0,"proxy":{"hosts":["shop.example.com"],"port":80}},
		{"name":"web","replicas":2,"proxy":{"hosts":["web.example.com"],"port":3000}},
		{"name":"worker","replicas":0}]}`
	var p stackPlan
	if err := json.Unmarshal([]byte(legacy), &p); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"edge": true, "web": true, "worker": false}
	for _, sp := range p.Services {
		if sp.proxied() != want[sp.Name] {
			t.Errorf("%s proxied = %v", sp.Name, sp.proxied())
		}
	}
	// New plans record the mode, so a parked replicated Service is told apart.
	px := &config.ServiceProxy{Port: 80}
	for _, c := range []struct {
		sp   servicePlan
		want bool
	}{
		{servicePlan{Mode: "replicated", Replicas: 0, Proxy: px}, false},
		{servicePlan{Mode: "replicated", Replicas: 1, Proxy: px}, true},
		{servicePlan{Mode: "global", Proxy: px}, true},
		{servicePlan{Mode: "global"}, false},
	} {
		if got := c.sp.proxied(); got != c.want {
			t.Errorf("%+v proxied = %v", c.sp, got)
		}
	}
	b, _ := json.Marshal(servicePlan{Name: "edge", Mode: "global"})
	if !strings.Contains(string(b), `"mode":"global"`) {
		t.Errorf("plan.json %s", b)
	}
}

// Deploy v1, then v2 (current). A failed retry of --version v1 must put back
// what ran before the attempt, the stack of v2, not the retained v1 stack.
func TestRedeployNonCurrentVersionFailureRestoresCurrentStack(t *testing.T) {
	root := withRoot(t)
	fastPolling(t)
	ctx := context.Background()
	sim := newSim()
	h := newFake(sim)
	if _, err := (Runtime{}).Deploy(ctx, testDeploy(t, h, nil)); err != nil {
		t.Fatal(err)
	}
	d2 := testDeploy(t, h, nil)
	d2.Version = "v2"
	setWebImage(d2, "shop-web:v2")
	if _, err := (Runtime{}).Deploy(ctx, d2); err != nil {
		t.Fatal(err)
	}
	appDir := filepath.Join(root, "apps", "shop", "production")
	v1Dir := filepath.Join(appDir, "releases", "v1")
	v2Dir := filepath.Join(appDir, "releases", "v2")
	v1Before, _ := os.ReadFile(filepath.Join(v1Dir, "compose.yaml"))
	v2Stack, _ := os.ReadFile(filepath.Join(v2Dir, "compose.yaml"))
	if bytes.Equal(v1Before, v2Stack) {
		t.Fatal("test needs different stacks")
	}
	h.scripts = nil

	sim.failOnce = true
	d := testDeploy(t, h, nil)
	setWebImage(d, "shop-web:retry")
	if _, err := (Runtime{}).Deploy(ctx, d); err == nil {
		t.Fatal("want error")
	}
	if b, _ := os.ReadFile(filepath.Join(v1Dir, "compose.yaml")); !bytes.Equal(b, v1Before) {
		t.Error("v1 record not restored")
	}
	if link, _ := os.Readlink(filepath.Join(appDir, "current")); filepath.Base(link) != "v2" {
		t.Errorf("current = %q", link)
	}
	// The last stack deploy is the restore and it uses v2's stack file.
	var last string
	for _, s := range h.scripts {
		if strings.Contains(s, "stack deploy") {
			last = s
		}
	}
	if !strings.Contains(last, "releases/v2/compose.yaml") {
		t.Errorf("restore did not redeploy v2's stack:\n%s", last)
	}
	for _, v := range []string{v1Dir, v2Dir} {
		if _, err := os.Stat(filepath.Join(v, partialMarker)); !os.IsNotExist(err) {
			t.Errorf("marker in %s: %v", v, err)
		}
	}
}
