package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

func TestLock(t *testing.T) {
	withRoot(t)
	ctx := context.Background()
	h := &remote.Local{}
	unlock, err := AcquireLock(ctx, h, "shop", "production", LockInfo{Performer: "alice", Version: "v1", Command: "deploy"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = AcquireLock(ctx, h, "shop", "production", LockInfo{Performer: "bob"})
	var le *LockedError
	if !errors.As(err, &le) || le.Holder.Performer != "alice" || !strings.Contains(err.Error(), "alice (deploy v1)") {
		t.Fatalf("second acquire: %v", err)
	}
	// Other Destinations are independent.
	u2, err := AcquireLock(ctx, h, "shop", "staging", LockInfo{Performer: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	_ = u2(ctx)
	if err := unlock(ctx); err != nil {
		t.Fatal(err)
	}
	u3, err := AcquireLock(ctx, h, "shop", "production", LockInfo{Performer: "bob"})
	if err != nil {
		t.Fatalf("after unlock: %v", err)
	}
	_ = u3(ctx)
}

// respond simulates docker for a deploy where web has one running container.
func respond(proxyErr error) func(string) (string, error) {
	return func(s string) (string, error) {
		switch {
		case strings.Contains(s, "kamal-proxy' 'deploy"):
			return "", proxyErr
		case strings.Contains(s, "'--scale'"):
			return "old1\nnew1", nil
		case strings.Contains(s, "'rm' '-f' 'web'"):
			return "old1", nil
		case strings.Contains(s, "{{.Name}}"):
			return "/yoho-shop-production-web-3", nil
		case strings.Contains(s, "docker ps -a --filter"):
			return "old1 web\nnew1 web\nw9 worker", nil
		case strings.Contains(s, "kamal-proxy-config"), strings.Contains(s, "docker container inspect"):
			return "", nil
		}
		return "", nil
	}
}

// indexOf returns the position of the first recorded script containing sub.
func indexOf(t *testing.T, scripts []string, sub string) int {
	t.Helper()
	for i, s := range scripts {
		if strings.Contains(s, sub) {
			return i
		}
	}
	t.Fatalf("no script contains %q", sub)
	return -1
}

func TestDeployEndToEnd(t *testing.T) {
	root := withRoot(t)
	ctx := context.Background()
	h := &dockerHost{local: &remote.Local{}, respond: respond(nil)}
	var out bytes.Buffer
	var hooks []string
	d := testDeploy(t, h, &out)
	d.Hook = func(_ context.Context, name string, env map[string]string) error {
		hooks = append(hooks, name+":"+env["YOHO_SERVICE_VERSION"]+":"+env["YOHO_HOSTS"])
		return nil
	}

	rel, err := Compose{}.Deploy(ctx, d)
	if err != nil {
		t.Fatalf("%v\n%s\n%s", err, out.String(), h.all())
	}
	if rel.Status != "deployed" || rel.SecretsGeneration == "" || rel.Images["web"] != "shop-web:v1" {
		t.Errorf("release %+v", rel)
	}
	if a := rel.Secrets["web/SECRET_KEY_BASE"]; a.Ref != "op://vault/shop/skb" || len(a.Fingerprint) != 16 {
		t.Errorf("audit %+v", a)
	}
	if a := rel.Secrets["web/DATABASE_PASSWORD"]; a.Ref != "generated" {
		t.Errorf("generated audit %+v", a)
	}
	if strings.Join(hooks, ",") != "pre-deploy:shop@v1:203.0.113.10,post-deploy:shop@v1:203.0.113.10" {
		t.Errorf("hooks %v", hooks)
	}

	// Command order: proxy boot, deps, release command, plain up, scale,
	// health, proxy switch, remove old, orphans.
	s := h.scripts
	order := []string{
		"docker network create yoho",
		"'up' '-d' '--no-recreate' 'db'",
		"'run' '--rm' '--no-deps' '-T' 'web' 'bin/rails' 'db:migrate'",
		"'up' '-d' '--no-deps' 'cloudflared' 'db'",
		"'rm' '-f' 'web'",
		"'up' '-d' '--no-deps' '--no-recreate' '--scale' 'web=2' 'web'",
		"State.Health",
		"'kamal-proxy' 'deploy' 'shop-production-web' '--target' 'yoho-shop-production-web-3:3000' '--host' 'shop.example.com' '--health-check-path' '/up' '--deploy-timeout' '30s' '--drain-timeout' '30s'",
		"docker stop 'old1'",
		"docker stop 'w9'",
	}
	last := -1
	for _, sub := range order {
		i := indexOf(t, s, sub)
		if i <= last {
			t.Errorf("%q out of order (at %d, previous step at %d)\n%s", sub, i, last, h.all())
		}
		last = i
	}
	if strings.Contains(h.all(), "'down'") || strings.Contains(h.all(), "-v ") {
		t.Error("must never run compose down / remove volumes")
	}
	if !strings.Contains(s[0], "-p' 'yoho-shop-production' '--project-directory'") && !strings.Contains(h.all(), "'-p' 'yoho-shop-production'") {
		t.Error("compose project name not used")
	}

	appDir := filepath.Join(root, "apps", "shop", "production")
	cur, err := os.Readlink(filepath.Join(appDir, "current"))
	if err != nil || cur != "releases/v1" {
		t.Errorf("current -> %q, %v", cur, err)
	}
	compose, err := os.ReadFile(filepath.Join(appDir, "releases", "v1", "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{webSecret, "tunnel-TOKEN"} {
		if bytes.Contains(compose, []byte(secret)) {
			t.Error("compose.yaml on the Server contains a secret value")
		}
	}
	pg, err := os.ReadFile(filepath.Join(appDir, "generated", "POSTGRES_PASSWORD"))
	if err != nil || len(pg) != 32 {
		t.Fatalf("generated secret: %q %v", pg, err)
	}
	web, _ := os.ReadFile(filepath.Join(appDir, "secrets", rel.SecretsGeneration, "web", "DATABASE_PASSWORD"))
	if !bytes.Equal(web, pg) {
		t.Error("web did not receive the generated db password")
	}
	if _, err := os.Stat(filepath.Join(appDir, "lock")); !errors.Is(err, os.ErrNotExist) {
		t.Error("lock not released")
	}
	if strings.Contains(out.String(), webSecret) || !strings.Contains(out.String(), "[primary] ") {
		t.Errorf("progress output: %s", out.String())
	}
	// Time after the route switch must land on a labeled line, not the hook.
	progress := out.String()
	switching := strings.Index(progress, "switching proxy route")
	switched := strings.Index(progress, "switched proxy route shop-production-web")
	routes := strings.Index(progress, "checking proxy routes")
	releases := strings.Index(progress, "checking old releases")
	images := strings.Index(progress, "checking unused images")
	if switching < 0 || switched < switching || routes < switched || releases < routes || images < releases {
		t.Errorf("post-cutover progress order:\n%s", progress)
	}

	rels, err := Compose{}.Releases(ctx, d)
	if err != nil || len(rels) != 1 || rels[0].Version != "v1" {
		t.Errorf("releases %v %v", rels, err)
	}
}

func TestDeployProxyFailureKeepsOld(t *testing.T) {
	root := withRoot(t)
	h := &dockerHost{local: &remote.Local{}, respond: respond(errors.New("target failed to become healthy"))}
	var out bytes.Buffer
	d := testDeploy(t, h, &out)
	_, err := Compose{}.Deploy(context.Background(), d)
	if err == nil {
		t.Fatal("want error")
	}
	all := h.all()
	if !strings.Contains(all, "docker rm -f 'new1'") {
		t.Errorf("new container not removed:\n%s", all)
	}
	if strings.Contains(all, "docker stop 'old1'") {
		t.Error("old container must keep serving")
	}
	appDir := filepath.Join(root, "apps", "shop", "production")
	b, err := os.ReadFile(filepath.Join(appDir, "releases", "v1", "release.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rel release.Release
	_ = json.Unmarshal(b, &rel)
	if rel.Status != "failed" {
		t.Errorf("status %q", rel.Status)
	}
	if _, err := os.Lstat(filepath.Join(appDir, "current")); !errors.Is(err, os.ErrNotExist) {
		t.Error("current must not move on failure")
	}
	if _, err := os.Stat(filepath.Join(appDir, "lock")); !errors.Is(err, os.ErrNotExist) {
		t.Error("lock not released after failure")
	}
}

func TestReleaseCommandFailureAborts(t *testing.T) {
	withRoot(t)
	h := &dockerHost{local: &remote.Local{}}
	h.respond = func(s string) (string, error) {
		if strings.Contains(s, "'run' '--rm'") {
			return "", &remote.ExitError{Host: "s1", Code: 1, Stderr: "migration failed"}
		}
		return respond(nil)(s)
	}
	_, err := Compose{}.Deploy(context.Background(), testDeploy(t, h, nil))
	if err == nil || !strings.Contains(err.Error(), "release command") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(h.all(), "--scale") || strings.Contains(h.all(), "kamal-proxy' 'deploy") {
		t.Error("cutover must not start after a failed release command")
	}
}

func TestDeployLocked(t *testing.T) {
	withRoot(t)
	h := &dockerHost{local: &remote.Local{}}
	unlock, err := AcquireLock(context.Background(), h, "shop", "production", LockInfo{Performer: "carol"})
	if err != nil {
		t.Fatal(err)
	}
	defer unlock(context.Background())
	_, err = Compose{}.Deploy(context.Background(), testDeploy(t, h, nil))
	if err == nil || !strings.Contains(err.Error(), "carol") {
		t.Fatalf("err = %v", err)
	}
	if h.all() != "" {
		t.Error("no docker commands while locked")
	}
}

func TestFirstDeployNoOldContainers(t *testing.T) {
	withRoot(t)
	h := &dockerHost{local: &remote.Local{}}
	h.respond = func(s string) (string, error) {
		switch {
		case strings.Contains(s, "'--scale'"):
			return "new1", nil
		case strings.Contains(s, "'rm' '-f' 'web'"):
			return "", nil
		}
		return respond(nil)(s)
	}
	if _, err := (Compose{}).Deploy(context.Background(), testDeploy(t, h, nil)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.all(), "'--scale' 'web=1'") || strings.Contains(h.all(), "docker stop 'old1'") {
		t.Errorf("first deploy:\n%s", h.all())
	}
}

func TestRollbackAndPrune(t *testing.T) {
	t.Setenv("CLICOLOR_FORCE", "1")
	root := withRoot(t)
	ctx := context.Background()
	h := &dockerHost{local: &remote.Local{}, respond: respond(nil)}
	clock := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	oldNow := now
	now = func() time.Time { clock = clock.Add(time.Minute); return clock }
	t.Cleanup(func() { now = oldNow })

	var gens []string
	for _, v := range []string{"v1", "v2", "v3"} {
		d := testDeploy(t, h, nil)
		d.Version = v
		rel, err := Compose{}.Deploy(ctx, d)
		if err != nil {
			t.Fatal(err)
		}
		gens = append(gens, rel.SecretsGeneration)
	}
	appDir := filepath.Join(root, "apps", "shop", "production")
	// RetainReleases = 2: v1 and its secrets generation are pruned.
	if _, err := os.Stat(filepath.Join(appDir, "releases", "v1")); !errors.Is(err, os.ErrNotExist) {
		t.Error("v1 should be pruned")
	}
	if _, err := os.Stat(filepath.Join(appDir, "secrets", gens[0])); !errors.Is(err, os.ErrNotExist) {
		t.Error("v1 secrets generation should be pruned")
	}
	if _, err := os.Stat(filepath.Join(appDir, "secrets", gens[1])); err != nil {
		t.Error("v2 secrets generation must be kept")
	}

	h.scripts = nil
	d := testDeploy(t, h, nil)
	if _, err := (Compose{}).Rollback(ctx, d, "v1"); err == nil {
		t.Error("rollback to a pruned release must fail")
	}
	rel, err := Compose{}.Rollback(ctx, d, "v2")
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version != "v2" || rel.Status != "deployed" {
		t.Errorf("rollback release %+v", rel)
	}
	all := h.all()
	if strings.Contains(all, "'run' '--rm'") {
		t.Error("rollback must not run release commands")
	}
	if !strings.Contains(all, filepath.Join(appDir, "releases", "v2", "compose.yaml")) || !strings.Contains(all, "kamal-proxy' 'deploy") {
		t.Errorf("rollback cutover:\n%s", all)
	}
	if cur, _ := os.Readlink(filepath.Join(appDir, "current")); cur != "releases/v2" {
		t.Errorf("current -> %s", cur)
	}
	rels, _ := Compose{}.Releases(ctx, d)
	if len(rels) != 2 || rels[0].Version != "v2" || rels[1].Version != "v3" || rels[1].Status != "rolled_back" {
		t.Errorf("releases after rollback: %+v", rels)
	}
}
