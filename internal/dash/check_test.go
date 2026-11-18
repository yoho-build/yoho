package dash

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// checkHost serves the dash fixture and a Service check of web.
type checkHost struct {
	remote.Local
}

func (h *checkHost) Output(_ context.Context, c remote.Cmd) (string, error) {
	switch {
	case strings.HasPrefix(c.Script, "docker ps -a --no-trunc"):
		return "aaa|yoho-shop-production-web-1|web||running\n", nil
	case strings.HasPrefix(c.Script, "docker inspect "):
		return "aaa|/yoho-shop-production-web-1|yoho-shop-production|yoho-shop-production_default|yoho-shop-production_default=172.18.0.5\n", nil
	}
	return fixture, nil
}

func (h *checkHost) DialContext(context.Context, string, string) (net.Conn, error) {
	a, b := net.Pipe()
	go b.Close()
	return a, nil
}

func TestCollectRunsServiceChecks(t *testing.T) {
	old := release.Root
	release.Root = t.TempDir()
	t.Cleanup(func() { release.Root = old })
	dir := release.Dir("shop", "production", "v1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plan.json"), []byte(`[{"name":"web","check":{"type":"tcp","port":3000,"timeout":"30s"}}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/v1", filepath.Join(release.AppDir("shop", "production"), "current")); err != nil {
		t.Fatal(err)
	}
	src := &Source{App: "shop", Destination: "production", Runtime: "compose", Root: release.Root,
		Targets: []Target{{Name: "web-1", Host: &checkHost{}}}}
	snap, err := Collect(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range snap.Servers[0].Containers {
		want := ""
		if c.Name == "yoho-shop-production-web-1" {
			want = "ok"
		}
		if c.Check != want {
			t.Errorf("%s: check %q, want %q", c.Name, c.Check, want)
		}
	}
}
