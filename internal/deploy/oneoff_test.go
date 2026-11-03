package deploy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/remote"
)

func TestOneOffScript(t *testing.T) {
	root := withRoot(t)
	s := OneOffScript("shop", "production", "v1", "web", false, []string{"bin/rails", "db:migrate", "--trace"})
	for _, want := range []string{
		"docker compose '-p' 'yoho-shop-production'",
		"'--project-directory' '" + filepath.Join(root, "apps", "shop", "production") + "'",
		"'run' '--rm' '--no-deps' '--label' 'yoho.oneoff=run' '-T' '--' 'web' 'bin/rails' 'db:migrate' '--trace'",
		"/releases/v1/compose.yaml'",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in %s", want, s)
		}
	}
	if strings.Contains(s, "--service-ports") || strings.Contains(s, " up ") {
		t.Errorf("one-off publishes ports or starts the project: %s", s)
	}
	if tty := OneOffScript("shop", "production", "v1", "web", true, []string{"sh"}); strings.Contains(tty, "'-T'") {
		t.Errorf("tty one-off passes -T: %s", tty)
	}
}

func TestCurrentServices(t *testing.T) {
	root := withRoot(t)
	h := &remote.Local{}
	if _, _, err := CurrentServices(context.Background(), h, "shop", "production"); !errors.Is(err, ErrNoCurrentRelease) {
		t.Fatalf("no Release: %v", err)
	}
	dir := filepath.Join(root, "apps", "shop", "production")
	if err := os.MkdirAll(filepath.Join(dir, "releases", "v2"), 0o700); err != nil {
		t.Fatal(err)
	}
	plan := `[{"name":"web","replicas":1,"proxy":{"port":3000}},{"name":"db","stateful":true,"replicas":1}]`
	if err := os.WriteFile(filepath.Join(dir, "releases", "v2", "plan.json"), []byte(plan), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/v2", filepath.Join(dir, "current")); err != nil {
		t.Fatal(err)
	}
	ver, svcs, err := CurrentServices(context.Background(), h, "shop", "production")
	if err != nil || ver != "v2" || len(svcs) != 2 || !svcs[0].Proxied || svcs[0].Stateful || !svcs[1].Stateful {
		t.Fatalf("%s %+v %v", ver, svcs, err)
	}
}
