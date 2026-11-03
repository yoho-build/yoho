package composefile

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func publicPortFindings(t *testing.T, services string) map[string]string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n"+services), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Load(context.Background(), dir, nil, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range Check(r, "compose", nil) {
		if strings.Contains(f.Message, "on all interfaces") {
			if f.Level != LevelWarning {
				t.Errorf("%s: level %s, want warning", f.Service, f.Level)
			}
			got[f.Service] = f.Message
		}
	}
	return got
}

func TestStatefulPublicPortsWarning(t *testing.T) {
	const svcs = `
  short: {image: postgres, restart: always, x-yoho: {stateful: true}, ports: ["5432:5432"]}
  anyv4: {image: postgres, restart: always, x-yoho: {stateful: true}, ports: ["0.0.0.0:5432:5432"]}
  anyv6: {image: postgres, restart: always, x-yoho: {stateful: true}, ports: ["[::]:5432:5432"]}
  ephemeral: {image: postgres, restart: always, x-yoho: {stateful: true}, ports: ["5432"]}
  long:
    image: postgres
    restart: always
    x-yoho: {stateful: true}
    ports: [{target: 5432, published: "5432"}]
  longany:
    image: postgres
    restart: always
    x-yoho: {stateful: true}
    ports: [{target: 5432, published: "5432", host_ip: "0.0.0.0"}]
  loop4: {image: postgres, restart: always, x-yoho: {stateful: true}, ports: ["127.0.0.1:5433:5432"]}
  loop6: {image: postgres, restart: always, x-yoho: {stateful: true}, ports: ["[::1]:5434:5432"]}
  longloop:
    image: postgres
    restart: always
    x-yoho: {stateful: true}
    ports: [{target: 5432, published: "5435", host_ip: "127.0.0.1"}]
  noports: {image: postgres, restart: always, x-yoho: {stateful: true}}
  web: {image: nginx, restart: always, ports: ["8080:80"]}
`
	got := publicPortFindings(t, svcs)
	for _, name := range []string{"short", "anyv4", "anyv6", "ephemeral", "long", "longany"} {
		msg, ok := got[name]
		if !ok {
			t.Errorf("%s: want public port warning", name)
			continue
		}
		if !strings.Contains(msg, "127.0.0.1:5432:5432") || !strings.Contains(msg, "compose network") {
			t.Errorf("%s: message lacks hint: %s", name, msg)
		}
	}
	if !strings.Contains(got["short"], "5432:5432 on all") || !strings.Contains(got["ephemeral"], "port(s) 5432 on all") {
		t.Errorf("port description: %q / %q", got["short"], got["ephemeral"])
	}
	for _, name := range []string{"loop4", "loop6", "longloop", "noports", "web"} {
		if msg, ok := got[name]; ok {
			t.Errorf("%s: unexpected warning: %s", name, msg)
		}
	}
}
