package composefile

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInAppDir(t *testing.T) {
	for _, c := range []struct {
		src  string
		rel  string
		want bool
	}{
		{"/app/cfg/a.txt", "cfg/a.txt", true},
		{"/app", ".", true},
		{"/app2/x", "", false},
		{"/etc/x", "", false},
		{"/app/../etc", "", false},
		{"rel/x", "", false},
	} {
		rel, ok := InAppDir("/app", c.src)
		if ok != c.want || rel != c.rel {
			t.Errorf("InAppDir(/app, %s) = %q %v", c.src, rel, ok)
		}
	}
}

func TestCheckBinds(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "cfg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cfg", "init.sql"), []byte("select 1;"), 0o644); err != nil {
		t.Fatal(err)
	}
	compose := `
services:
  ok:
    image: alpine
    volumes:
      - ./cfg/init.sql:/init.sql:ro
      - /var/run/docker.sock:/var/run/docker.sock
      - /etc/ssl/certs:/certs:ro
  writable:
    image: alpine
    volumes: ["./cfg:/cfg"]
  missing:
    image: alpine
    volumes: ["./nope.conf:/nope.conf:ro"]
  local:
    image: alpine
    volumes: ["/Users/alice/shared/x.conf:/x.conf:ro"]
`
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Load(context.Background(), dir, nil, nil, "shop", "")
	if err != nil {
		t.Fatal(err)
	}
	fs := Check(r, "compose", nil)
	errs, warns := msgs(fs, LevelError), msgs(fs, LevelWarning)
	for _, want := range []string{
		"writable: writable bind mount ./cfg:/cfg is inside the App directory",
		`named volume (volumes: ["data:/cfg"]`,
		"missing: bind mount source ./nope.conf does not exist",
	} {
		if !strings.Contains(errs, want) {
			t.Errorf("errors missing %q:\n%s", want, errs)
		}
	}
	if strings.Contains(errs, "ok:") {
		t.Errorf("read-only App file and Server paths must pass:\n%s", errs)
	}
	if !strings.Contains(warns, "local: bind mount source /Users/alice/shared/x.conf is outside the App directory and looks like a path on this machine") {
		t.Errorf("warnings:\n%s", warns)
	}
	if strings.Contains(warns, "writable bind mount(s) "+filepath.Join(dir, "cfg")) {
		t.Errorf("unexpected warnings:\n%s", warns)
	}

	fs = Check(r, "swarm", nil)
	if e := msgs(fs, LevelError); !strings.Contains(e, "ok: bind mount ./cfg/init.sql:/init.sql comes from the App directory; the swarm runtime does not ship files") {
		t.Errorf("swarm errors:\n%s", e)
	}
}
