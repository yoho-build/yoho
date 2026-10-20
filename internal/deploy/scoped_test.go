package deploy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// A scoped deploy keeps the App off the shared `yoho` network: proxied
// Services join the App's own network, which the runtime creates and
// connects the Proxy to; rollback asks the Gate to vet the Release.
func TestScopedDeployOwnNetworkAndVettedRollback(t *testing.T) {
	root := withRoot(t)
	ctx := context.Background()
	h := &dockerHost{local: &remote.Local{}, respond: respond(nil)}
	d := testDeploy(t, h, nil)
	offShared(d)
	d.Scoped = &plan.Scoped{PID: 42, Boot: "b1", ProxyNetwork: "yoho-shop-production-proxy"}
	if _, err := (Compose{}).Deploy(ctx, d); err != nil {
		t.Fatal(err)
	}
	compose, err := os.ReadFile(filepath.Join(root, "apps", "shop", "production", "releases", "v1", "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// No network_mode anywhere: every Service is on the project's own
	// networks (default is yoho-shop-production_default), never docker0.
	if strings.Contains(string(compose), "network_mode") || !strings.Contains(string(compose), "name: yoho-shop-production_default") {
		t.Fatalf("compiled networks:\n%s", compose)
	}
	if !strings.Contains(string(compose), "name: yoho-shop-production-proxy") || strings.Contains(string(compose), "name: yoho\n") {
		t.Fatalf("compiled networks:\n%s", compose)
	}
	boot := indexOf(t, h.scripts, "kamal-proxy:")
	connect := indexOf(t, h.scripts, "docker network connect 'yoho-shop-production-proxy' yoho-proxy")
	if connect < boot {
		t.Fatalf("the Proxy joins the App network before it boots")
	}
	if !strings.Contains(h.scripts[connect], "docker network create --label 'yoho.proxy.attach=1' --label 'yoho.app=shop' --label 'yoho.destination=production'") {
		t.Fatalf("network script:\n%s", h.scripts[connect])
	}

	vetted := false
	d2 := testDeploy(t, h, nil)
	offShared(d2)
	d2.Scoped = &plan.Scoped{ProxyNetwork: "yoho-shop-production-proxy", Vet: func(c []byte, rel *release.Release) error {
		vetted = string(c) == string(compose) && rel.Version == "v1"
		return errors.New("refused by the Gate")
	}}
	n := len(h.scripts)
	if _, err := (Compose{}).Rollback(ctx, d2, "v1"); err == nil || !strings.Contains(err.Error(), "refused by the Gate") || !vetted {
		t.Fatalf("rollback: %v (vetted %v)", err, vetted)
	}
	for _, s := range h.scripts[n:] {
		if strings.Contains(s, "kamal-proxy' 'deploy") || strings.Contains(s, "'up'") {
			t.Fatalf("a refused rollback changed the App: %s", s)
		}
	}
	if _, err := os.Stat(LockDir("shop", "production")); !os.IsNotExist(err) {
		t.Fatal("the lock outlived the refused rollback")
	}
}

// offShared drops the test project's own `yoho` network declarations, as
// the Gate's policy requires of a scoped compose project.
func offShared(d *plan.Deploy) {
	delete(d.Project.Networks, "yoho")
	for name, s := range d.Project.Services {
		delete(s.Networks, "yoho")
		d.Project.Services[name] = s
	}
}
