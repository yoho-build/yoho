package deploy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/safepath"
)

func removeOpts() RemoveOptions {
	return RemoveOptions{App: "shop", Destination: "production", Services: []string{"web"}}
}

// Every remove script selects Docker objects by the exact project label of
// this App Destination: never a name filter or prefix that would also match
// yoho-shop-production-drill-1234 or yoho-shop-production2, never the
// shared network, never compose down -v.
func TestRemoveScriptsSelectOnlyThisProject(t *testing.T) {
	for _, swarm := range []bool{false, true} {
		o := removeOpts()
		o.Swarm = swarm
		o.Volumes = true
		all := strings.Join([]string{listScript(o), RemoveContainersScript(o), RemoveNetworksScript(o), RemoveVolumesScript(o)}, "\n")
		for _, bad := range []string{"name=^", "'name=", "down", "prune", "grep yoho-shop", "*", "drill"} {
			if strings.Contains(all, bad) {
				t.Errorf("swarm=%v: remove scripts contain %q:\n%s", swarm, bad, all)
			}
		}
		label := "'label=com.docker.compose.project=yoho-shop-production'"
		if swarm {
			label = "'label=com.docker.stack.namespace=yoho-shop-production'"
		}
		for _, s := range []string{RemoveContainersScript(o), RemoveNetworksScript(o), RemoveVolumesScript(o)} {
			if !strings.Contains(s, label) {
				t.Errorf("swarm=%v: no exact label %s in:\n%s", swarm, label, s)
			}
			// Every --filter is an exact label=key=value pair of this App Destination.
			for _, f := range regexp.MustCompile(`'label=[^']*'`).FindAllString(s, -1) {
				ok := f == label || f == "'label=com.docker.stack.namespace=yoho-shop-production-release'" ||
					f == "'label=yoho.app=shop'" || f == "'label=yoho.destination=production'" || f == "'label=yoho.proxy.attach=1'"
				if !ok {
					t.Errorf("swarm=%v: unexpected filter %s", swarm, f)
				}
			}
		}
		if !strings.Contains(RemoveNetworksScript(o), `[ "$name" = 'yoho' ] && return 0`) {
			t.Errorf("network removal does not spare the shared yoho network")
		}
	}
}

func TestInspectRemove(t *testing.T) {
	root := withRoot(t)
	rel := filepath.Join(root, "apps", "shop", "production", "releases", "v1")
	if err := os.MkdirAll(rel, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rel, "plan.json"), []byte(`[{"name":"web","replicas":1},{"name":"worker","replicas":1}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	routes := `{"shop-production-web":{"targets":["yoho-shop-production-web-1:3000"]},` +
		`"shop-production-worker":{"targets":["yoho-shop-production-worker-1:3000"]},` +
		`"shop-production-eu-web":{"targets":["yoho-shop-production-eu-web-1:3000"]},` +
		`"other-production-web":{"targets":["yoho-other-production-web-1:80"]}}`
	h := &dockerHost{local: &remote.Local{}, respond: func(s string) (string, error) {
		switch {
		case strings.Contains(s, "docker ps -a --filter"):
			return "container\tyoho-shop-production-web-1\tshop|production\ncontainer\tyoho-shop-production-db-1\tshop|production\n" +
				"network\tyoho-shop-production_default\nnetwork\tyoho\nvolume\tyoho-shop-production_pgdata\n", nil
		case strings.Contains(s, "{{.State.Running}}"):
			return "true", nil
		case strings.Contains(s, "kamal-proxy list --json"):
			return routes, nil
		}
		return "", nil
	}}
	inv, err := InspectRemove(context.Background(), h, removeOpts())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(inv.Containers, ","); got != "yoho-shop-production-web-1,yoho-shop-production-db-1" {
		t.Errorf("containers %s", got)
	}
	if got := strings.Join(inv.Networks, ","); got != "yoho-shop-production_default" {
		t.Errorf("networks %s (the shared yoho network must stay)", got)
	}
	if got := strings.Join(inv.Volumes, ","); got != "yoho-shop-production_pgdata" {
		t.Errorf("volumes %s", got)
	}
	// Routes named after a Service of this App Destination only: not the
	// eu Destination's (shop/production-eu) nor another App's.
	if got := strings.Join(inv.Routes, ","); got != "shop-production-web,shop-production-worker" {
		t.Errorf("routes %s", got)
	}
	if inv.AppDir != release.AppDir("shop", "production") {
		t.Errorf("app dir %q", inv.AppDir)
	}

	// A container of another App Destination in the project refuses.
	h.respond = func(s string) (string, error) {
		if strings.Contains(s, "docker ps -a --filter") {
			return "container\tyoho-shop-production-web-1\tshop-production|x\n", nil
		}
		return "", nil
	}
	var oe *release.OwnershipError
	if _, err := InspectRemove(context.Background(), h, removeOpts()); !errors.As(err, &oe) {
		t.Fatalf("foreign container: %v", err)
	}
}

func TestRemoveAppDirKeepsOtherDestinationsAndBackups(t *testing.T) {
	root := withRoot(t)
	mk := func(p string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, p), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	mk("apps/shop/production/releases/v1")
	mk("apps/shop/production/lock")
	mk("apps/shop/production/secrets/g1")
	mk("apps/shop/production2/releases/v1")
	mk("apps/shopx/production/releases/v1")
	mk("backups/shop/production")
	mk("audit/shop")
	if err := os.WriteFile(filepath.Join(root, "audit/shop/production.log"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mk("appdirs/shop/d/production")
	h := &remote.Local{}
	if err := RemoveAppDir(context.Background(), h, removeOpts()); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]bool{
		"apps/shop/production":      false,
		"appdirs/shop/d/production": false,
		"appdirs/shop":              false,
		"apps/shop/production2":     true,
		"apps/shopx/production":     true,
		"backups/shop/production":   true,
		"audit/shop/production.log": true,
	} {
		if _, err := os.Stat(filepath.Join(root, p)); (err == nil) != want {
			t.Errorf("%s exists=%v, want %v", p, err == nil, want)
		}
	}
	// Idempotent.
	if err := RemoveAppDir(context.Background(), h, removeOpts()); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveAppDirLink(t *testing.T) {
	root := withRoot(t)
	target := filepath.Join(t.TempDir(), "shopdata")
	if err := os.MkdirAll(filepath.Join(target, "releases", "v1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, safepath.MarkerName), []byte(safepath.Marker("shop", "production")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "apps", "shop"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "apps", "shop", "production")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAppDir(context.Background(), &remote.Local{}, removeOpts()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); err == nil {
		t.Error("App directory link kept")
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("app_dir target kept")
	}

	// A target without this App Destination's marker is left alone.
	other := filepath.Join(t.TempDir(), "notours")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "apps", "shop"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	_ = RemoveAppDir(context.Background(), &remote.Local{}, removeOpts())
	if _, err := os.Stat(filepath.Join(other, "keep")); err != nil {
		t.Error("removed an unmarked app_dir target")
	}
}
