package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

func readRel(t *testing.T, p string) release.Release {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var rel release.Release
	if err := json.Unmarshal(b, &rel); err != nil {
		t.Fatal(err)
	}
	return rel
}

// A failed redeploy of the Version `current` points at (e.g. after rotating
// secrets) must leave that Release's record, compose and plan intact.
func TestRedeploySameVersionFailureKeepsRelease(t *testing.T) {
	root := withRoot(t)
	ctx := context.Background()
	h := &dockerHost{local: &remote.Local{}, respond: respond(nil)}
	if _, err := (Compose{}).Deploy(ctx, testDeploy(t, h, nil)); err != nil {
		t.Fatal(err)
	}
	relDir := filepath.Join(root, "apps", "shop", "production", "releases", "v1")
	before := map[string][]byte{}
	for _, f := range releaseFiles {
		b, err := os.ReadFile(filepath.Join(relDir, f))
		if err != nil {
			t.Fatal(err)
		}
		before[f] = b
	}

	h.respond = respond(errors.New("target failed to become healthy"))
	d := testDeploy(t, h, nil)
	d.ServiceSecrets["web"]["SECRET_KEY_BASE"] = "rotated-secret-VALUE"
	d.Project.Services["web"] = withEnv(d.Project.Services["web"], "NEW_SETTING", "1")
	if _, err := (Compose{}).Deploy(ctx, d); err == nil {
		t.Fatal("want error")
	}
	for _, f := range releaseFiles {
		b, _ := os.ReadFile(filepath.Join(relDir, f))
		if !bytes.Equal(b, before[f]) {
			t.Errorf("%s changed by the failed redeploy", f)
		}
	}
	if rel := readRel(t, filepath.Join(relDir, "release.json")); rel.Status != "deployed" {
		t.Errorf("status %q", rel.Status)
	}

	// A successful redeploy does replace the record.
	h.respond = respond(nil)
	if _, err := (Compose{}).Deploy(ctx, d); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(relDir, "compose.yaml")); bytes.Equal(b, before["compose.yaml"]) || !bytes.Contains(b, []byte("NEW_SETTING")) {
		t.Error("successful redeploy did not write the new compose file")
	}

	// A post-deploy Hook failure happens after the Release is committed.
	d.Project.Services["web"] = withEnv(d.Project.Services["web"], "NEWER_SETTING", "1")
	d.Hook = func(_ context.Context, name string, _ map[string]string) error {
		if name == "post-deploy" {
			return errors.New("hook failed")
		}
		return nil
	}
	if _, err := (Compose{}).Deploy(ctx, d); err == nil {
		t.Fatal("want hook error")
	}
	if b, _ := os.ReadFile(filepath.Join(relDir, "compose.yaml")); !bytes.Contains(b, []byte("NEWER_SETTING")) {
		t.Error("post-deploy Hook failure must not restore the previous record")
	}
}

func withEnv(s types.ServiceConfig, k, v string) types.ServiceConfig {
	env := types.MappingWithEquals{}
	for ek, ev := range s.Environment {
		env[ek] = ev
	}
	env[k] = &v
	s.Environment = env
	return s
}

// Rollback re-points a Yoho-built tag that moved (same Version built for
// another Destination) at the image the Release ran; prune keeps that image.
func TestRollbackPinsRecordedImage(t *testing.T) {
	root := withRoot(t)
	ctx := context.Background()
	tagID := "sha256:aaaa1111"
	h := &dockerHost{local: &remote.Local{}}
	h.respond = func(s string) (string, error) {
		switch {
		case strings.Contains(s, "docker image inspect -f '{{.Id}}' 'yoho/shop-production-web:v1'"):
			return tagID, nil
		case strings.Contains(s, "docker image inspect -f '{{.Id}}' 'postgres:17'"):
			return "sha256:pg1", nil
		}
		return respond(nil)(s)
	}
	rel, err := (Compose{}).Deploy(ctx, builtWeb(t, testDeploy(t, h, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if rel.ImageIDs["web"] != "sha256:aaaa1111" || rel.ImageIDs["db"] != "sha256:pg1" {
		t.Fatalf("image ids %v", rel.ImageIDs)
	}
	if got := readRel(t, filepath.Join(root, "apps", "shop", "production", "releases", "v1", "release.json")); got.ImageIDs["web"] != tagID {
		t.Fatalf("release.json image ids %v", got.ImageIDs)
	}
	keep, err := RetainedImageRefs(ctx, h, "shop")
	if err != nil || !keep["sha256:aaaa1111"] {
		t.Fatalf("prune keep set %v %v", keep, err)
	}

	// Staging ships its own build of v1: the tag moves.
	tagID = "sha256:bbbb2222"
	h.scripts = nil
	var out bytes.Buffer
	d := testDeploy(t, h, &out)
	if _, err := (Compose{}).Rollback(ctx, d, "v1"); err != nil {
		t.Fatal(err)
	}
	all := h.all()
	retag := indexOf(t, h.scripts, "docker tag 'sha256:aaaa1111' 'yoho/shop-production-web:v1'")
	if up := indexOf(t, h.scripts, "'--scale' 'web=2'"); up < retag {
		t.Error("tag must be restored before cutover")
	}
	if strings.Contains(all, "docker tag 'sha256:pg1'") {
		t.Error("third-party tags are not Yoho's to move")
	}
}

func TestRollbackFailsWhenRecordedImageIsGone(t *testing.T) {
	withRoot(t)
	ctx := context.Background()
	tagID := "sha256:aaaa1111"
	h := &dockerHost{local: &remote.Local{}}
	h.respond = func(s string) (string, error) {
		switch {
		case strings.Contains(s, "docker image inspect -f '{{.Id}}' 'yoho/shop-production-web:v1'"):
			return tagID, nil
		case strings.Contains(s, "docker tag"):
			return "", &remote.ExitError{Host: "s1", Code: 1, Stderr: "image gone"}
		}
		return respond(nil)(s)
	}
	if _, err := (Compose{}).Deploy(ctx, builtWeb(t, testDeploy(t, h, nil))); err != nil {
		t.Fatal(err)
	}
	tagID = "sha256:bbbb2222"
	h.scripts = nil
	_, err := (Compose{}).Rollback(ctx, testDeploy(t, h, nil), "v1")
	if err == nil || !strings.Contains(err.Error(), "cannot restore image yoho/shop-production-web:v1") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(h.all(), "--scale") {
		t.Error("no cutover with the wrong image")
	}
}

// Project and route names are ambiguous across App/Destination pairs that
// differ in where the '-' falls; container labels decide ownership.
func TestOwnershipRefusesForeignProject(t *testing.T) {
	withRoot(t)
	h := &dockerHost{local: &remote.Local{}}
	h.respond = func(s string) (string, error) {
		if strings.Contains(s, `{{.Label "yoho.app"}}`) {
			return "shop-production|web\n|", nil // App shop-production, Destination web
		}
		return respond(nil)(s)
	}
	_, err := (Compose{}).Deploy(context.Background(), testDeploy(t, h, nil))
	if err == nil || !strings.Contains(err.Error(), "App shop-production Destination web") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(h.all(), "'up'") {
		t.Error("nothing may start in another App's project")
	}
	if _, err := (Compose{}).Diff(context.Background(), testDeploy(t, h, nil)); err == nil {
		t.Error("plan must report the collision too")
	}
}

func TestOwnershipRefusesForeignRoute(t *testing.T) {
	withRoot(t)
	h := &dockerHost{local: &remote.Local{}}
	h.respond = func(s string) (string, error) {
		switch {
		case strings.Contains(s, "kamal-proxy list --json"):
			return `{"shop-production-web":{"hosts":["*"],"targets":["yoho-shop-production-web-1:80"],"state":"running"}}`, nil
		case strings.Contains(s, `{{index .Config.Labels "yoho.app"}}`):
			return "shop|production-web", nil
		}
		return respond(nil)(s)
	}
	_, err := (Compose{}).Deploy(context.Background(), testDeploy(t, h, nil))
	if err == nil || !strings.Contains(err.Error(), "proxy route shop-production-web") {
		t.Fatalf("err = %v", err)
	}
}

func TestStaleRoutesKeepForeignRoute(t *testing.T) {
	h := &dockerHost{local: &remote.Local{}}
	h.respond = func(s string) (string, error) {
		switch {
		case strings.Contains(s, "kamal-proxy list --json"):
			// App shop Destination production-api, Service web: same prefix.
			return `{"shop-production-api-web":{"hosts":["*"],"targets":["yoho-shop-production-api-web-1:80"],"state":"running"},` +
				`"shop-production-old":{"hosts":["*"],"targets":["yoho-shop-production-old-1:80"],"state":"running"}}`, nil
		case strings.Contains(s, "'yoho-shop-production-api-web-1'"):
			return "shop|production-api", nil
		case strings.Contains(s, "'yoho-shop-production-old-1'"):
			return "shop|production", nil
		}
		return "", nil
	}
	r := &runner{h: h, out: io.Discard, app: "shop", dest: "production"}
	r.removeStaleRoutes(context.Background(), nil)
	all := h.all()
	if !strings.Contains(all, "'remove' 'shop-production-old'") || strings.Contains(all, "'remove' 'shop-production-api-web'") {
		t.Fatalf("stale routes:\n%s", all)
	}
}

// Cancelling while kamal-proxy drains: the route may already point at the
// new containers, which must not be deleted.
func TestProxySwitchCancelledKeepsNewContainers(t *testing.T) {
	for _, tc := range []struct {
		name, target string
		cancel, keep bool
	}{
		{"cancelled, switched", "yoho-shop-production-web-3:3000", true, true},
		{"cancelled, not yet switched", "yoho-shop-production-web-1:3000", true, true},
		{"proxy refused", "yoho-shop-production-web-1:3000", false, false},
		{"proxy refused but switched", "yoho-shop-production-web-3:3000", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withRoot(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h := &dockerHost{local: &remote.Local{}}
			h.respond = func(s string) (string, error) {
				switch {
				case strings.Contains(s, "kamal-proxy' 'deploy"):
					if tc.cancel {
						cancel()
						return "", context.Canceled
					}
					return "", errors.New("target failed to become healthy")
				case strings.Contains(s, "kamal-proxy list --json"):
					return `{"shop-production-web":{"hosts":["shop.example.com"],"targets":["` + tc.target + `"],"state":"running"}}`, nil
				}
				return respond(nil)(s)
			}
			_, err := (Compose{}).Deploy(ctx, testDeploy(t, h, nil))
			if err == nil {
				t.Fatal("want error")
			}
			removed := strings.Contains(h.all(), "docker rm -f 'new1'")
			if removed == tc.keep {
				t.Errorf("new containers removed = %v, want %v\n%s", removed, !tc.keep, h.all())
			}
			if strings.Contains(h.all(), "docker stop 'old1'") {
				t.Error("old containers must keep running")
			}
		})
	}
}

func TestReleaseCommandOutputRedacted(t *testing.T) {
	root := withRoot(t)
	h := &outputHost{dockerHost: &dockerHost{local: &remote.Local{}, respond: respond(nil)}, root: root}
	_, err := (Compose{}).Deploy(context.Background(), testDeploy(t, h, nil))
	if err == nil || !strings.Contains(err.Error(), "release command") {
		t.Fatalf("err = %v", err)
	}
	pg, _ := os.ReadFile(filepath.Join(root, "apps", "shop", "production", "generated", "POSTGRES_PASSWORD"))
	if len(pg) == 0 || strings.Contains(err.Error(), webSecret) || strings.Contains(err.Error(), string(pg)) {
		t.Errorf("error leaks a secret: %v", err)
	}
	if !strings.Contains(err.Error(), "connecting with ***") || !strings.Contains(err.Error(), "migration failed") {
		t.Errorf("output context lost: %v", err)
	}
}

// outputHost prints a release command's output, including the secrets it
// can read, to Cmd.Stdout like a real Host, then fails it.
type outputHost struct {
	*dockerHost
	root string
}

func (o *outputHost) Run(ctx context.Context, c remote.Cmd) error {
	if strings.Contains(c.Script, "'run' '--rm'") && c.Stdout != nil {
		pg, _ := os.ReadFile(filepath.Join(o.root, "apps", "shop", "production", "generated", "POSTGRES_PASSWORD"))
		_, _ = io.WriteString(c.Stdout, "connecting with "+string(pg)+"\nkey="+webSecret+"\nmigration failed\n")
		return &remote.ExitError{Host: "s1", Code: 1}
	}
	return o.dockerHost.Run(ctx, c)
}

// Rolling back to a Release without a proxied Service removes its route.
func TestRollbackRemovesStaleRoutes(t *testing.T) {
	withRoot(t)
	ctx := context.Background()
	clock := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	oldNow := now
	now = func() time.Time { clock = clock.Add(time.Minute); return clock }
	t.Cleanup(func() { now = oldNow })
	h := &dockerHost{local: &remote.Local{}}
	h.respond = func(s string) (string, error) {
		if strings.Contains(s, "kamal-proxy list --json") {
			return goodRoutes, nil
		}
		return respond(nil)(s)
	}
	noProxy := func(d *plan.Deploy) *plan.Deploy {
		ext := d.Ext["web"]
		ext.Proxy = nil
		d.Ext["web"] = ext
		return d
	}
	d1 := noProxy(testDeploy(t, h, nil))
	if _, err := (Compose{}).Deploy(ctx, d1); err != nil {
		t.Fatal(err)
	}
	d2 := testDeploy(t, h, nil)
	d2.Version = "v2"
	if _, err := (Compose{}).Deploy(ctx, d2); err != nil {
		t.Fatal(err)
	}
	h.scripts = nil
	if _, err := (Compose{}).Rollback(ctx, testDeploy(t, h, nil), "v1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.all(), "'kamal-proxy' 'remove' 'shop-production-web'") {
		t.Errorf("route of the no longer proxied web not removed:\n%s", h.all())
	}
}

// deploy.replicas: 0 on a proxied Service scales it to zero like compose
// does, instead of starting one container.
func TestProxiedServiceScaledToZero(t *testing.T) {
	withRoot(t)
	zero := 0
	h := &dockerHost{local: &remote.Local{}}
	h.respond = func(s string) (string, error) {
		if strings.Contains(s, "kamal-proxy list --json") {
			return goodRoutes, nil
		}
		return respond(nil)(s)
	}
	d := testDeploy(t, h, nil)
	web := d.Project.Services["web"]
	web.Deploy = &types.DeployConfig{Replicas: &zero}
	d.Project.Services["web"] = web
	d.Ext["web"] = config.ServiceExt{Proxy: d.Ext["web"].Proxy, Secrets: d.Ext["web"].Secrets}

	_, plans, _ := compileTest(t, d)
	for _, sp := range plans {
		if sp.Name == "web" && (sp.Replicas != 0 || sp.proxied()) {
			t.Fatalf("web plan %+v", sp)
		}
	}
	if _, err := (Compose{}).Deploy(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	all := h.all()
	if strings.Contains(all, "--scale") || strings.Contains(all, "kamal-proxy' 'deploy") {
		t.Errorf("scaled-to-zero Service must not get a proxy cutover:\n%s", all)
	}
	if !strings.Contains(all, "'up' '-d' '--no-deps' 'cloudflared' 'db' 'web'") {
		t.Errorf("compose must converge web to zero:\n%s", all)
	}
	if !strings.Contains(all, "'kamal-proxy' 'remove' 'shop-production-web'") {
		t.Errorf("route must be removed:\n%s", all)
	}

	// Plan: no containers is the desired state, not "create".
	h.respond = (&fakeState{routes: goodRoutes}).respond
	cs, err := Compose{}.Diff(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	m := byKey(cs)
	if m["service web"].Action != plan.ActionNoop {
		t.Errorf("web %+v", m["service web"])
	}
	if _, ok := m["route shop-production-web"]; !ok || m["route shop-production-web"].Action != plan.ActionDelete {
		t.Errorf("route %+v", m["route shop-production-web"])
	}
}
