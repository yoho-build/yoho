package deploy

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// x-yoho is stripped from the compiled file, so a changed release_command
// must show in the plan or `apply` skips the rollout and the migration never
// runs.
func TestDiffReleaseCommandChange(t *testing.T) {
	h, _, _ := deployed(t)
	d := testDeploy(t, h, io.Discard)
	cs, err := Compose{}.Diff(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if ch := byKey(cs)["service web"]; ch.Action != plan.ActionNoop {
		t.Fatalf("unchanged: %+v", ch)
	}
	web := d.Ext["web"]
	web.ReleaseCommand = []string{"bin/rails", "db:migrate", "db:seed"}
	d.Ext["web"] = web
	cs, err = Compose{}.Diff(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	ch := byKey(cs)["service web"]
	if ch.Action != plan.ActionUpdate || ch.Downtime || !slices.Contains(ch.Reasons, "release_command changed") {
		t.Fatalf("changed release_command: %+v", ch)
	}
}

// Once kamal-proxy has switched the route, failing to stop the old containers
// must not fail the Release: it serves traffic.
func TestOldContainerRemovalFailureIsWarning(t *testing.T) {
	root := withRoot(t)
	var out strings.Builder
	h := &dockerHost{local: &remote.Local{}}
	h.respond = func(s string) (string, error) {
		if strings.Contains(s, "docker stop 'old1'") {
			return "", &remote.ExitError{Host: "s1", Code: 1, Stderr: "cannot stop"}
		}
		return respond(nil)(s)
	}
	rel, err := (Compose{}).Deploy(context.Background(), testDeploy(t, h, &out))
	if err != nil {
		t.Fatalf("deploy failed: %v", err)
	}
	if rel.Status != "deployed" {
		t.Errorf("status %q", rel.Status)
	}
	appDir := filepath.Join(root, "apps", "shop", "production")
	if cur, _ := os.Readlink(filepath.Join(appDir, "current")); cur != "releases/v1" {
		t.Errorf("current -> %q", cur)
	}
	if got := readRel(t, filepath.Join(appDir, "releases", "v1", "release.json")); got.Status != "deployed" {
		t.Errorf("recorded %q", got.Status)
	}
	if !strings.Contains(out.String(), "warning: traffic is switched") {
		t.Errorf("no warning:\n%s", out.String())
	}
}

func TestRetainedReleasesSkipsFailed(t *testing.T) {
	rel := func(v, st string) release.Release { return release.Release{Version: v, Status: st} }
	rels := []release.Release{rel("f3", "failed"), rel("f2", "failed"), rel("v3", "deployed"), rel("f1", "failed"), rel("v2", "rolled_back"), rel("v1", "deployed")}
	got := RetainedReleases(rels, 2, "v3")
	for _, v := range []string{"v3", "v2", "f3"} {
		if !got[v] {
			t.Errorf("%s dropped: %v", v, got)
		}
	}
	for _, v := range []string{"v1", "f2", "f1"} {
		if got[v] {
			t.Errorf("%s kept: %v", v, got)
		}
	}
	// current is kept even when it is old.
	if got := RetainedReleases(rels, 1, "v1"); !got["v1"] || !got["v3"] {
		t.Errorf("current %v", got)
	}
}

// A run of failed deploys must not push the last good Releases out of
// retention at the next successful deploy.
func TestPruneKeepsGoodReleasesAfterFailures(t *testing.T) {
	root := withRoot(t)
	ctx := context.Background()
	h := &dockerHost{local: &remote.Local{}, respond: respond(nil)}
	clock := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	oldNow := now
	now = func() time.Time { clock = clock.Add(time.Minute); return clock }
	t.Cleanup(func() { now = oldNow })

	deployVer := func(v string, proxyErr error) {
		h.respond = respond(proxyErr)
		d := testDeploy(t, h, nil)
		d.Version = v
		_, err := Compose{}.Deploy(ctx, d)
		if (err != nil) != (proxyErr != nil) {
			t.Fatalf("%s: %v", v, err)
		}
	}
	deployVer("v1", nil)
	deployVer("v2", nil)
	deployVer("f1", errors.New("unhealthy"))
	deployVer("f2", errors.New("unhealthy"))
	deployVer("v3", nil)
	rels := filepath.Join(root, "apps", "shop", "production", "releases")
	for v, want := range map[string]bool{"v3": true, "v2": true, "f2": true, "v1": false, "f1": false} {
		_, err := os.Stat(filepath.Join(rels, v))
		if (err == nil) != want {
			t.Errorf("release %s present=%v, want %v", v, err == nil, want)
		}
	}
}

func TestOwnershipErrorType(t *testing.T) {
	withRoot(t)
	h := &dockerHost{local: &remote.Local{}, respond: func(s string) (string, error) {
		if strings.Contains(s, `{{.Label "yoho.app"}}`) {
			return "foo|bar-prod", nil
		}
		return respond(nil)(s)
	}}
	d := testDeploy(t, h, nil)
	_, err := Compose{}.Deploy(context.Background(), d)
	var oe *release.OwnershipError
	if !errors.As(err, &oe) {
		t.Fatalf("want OwnershipError, got %v", err)
	}
}
