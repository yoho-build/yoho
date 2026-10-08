package swarm

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

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
