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
	return statefulFindings(t, services, "compose", "on all interfaces")
}

// statefulFindings returns the warnings containing marker per Service.
func statefulFindings(t *testing.T, services, runtime, marker string) map[string]string {
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
	for _, f := range Check(r, runtime, nil) {
		if strings.Contains(f.Message, marker) {
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
		if !strings.Contains(msg, "127.0.0.1:5432:5432") || !strings.Contains(msg, "compose network") || !strings.Contains(msg, "backup restore and drill refuse any published port") || !strings.Contains(msg, "only if you must") {
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

func TestStatefulSwarmPortsWarning(t *testing.T) {
	const svcs = `
  public:
    image: postgres
    x-yoho: {stateful: true}
    deploy: {placement: {constraints: [node.hostname == a]}}
    ports: ["5432:5432"]
  loop:
    image: postgres
    x-yoho: {stateful: true}
    deploy: {placement: {constraints: [node.hostname == a]}}
    ports: ["127.0.0.1:5433:5432"]
  noports:
    image: postgres
    x-yoho: {stateful: true}
    deploy: {placement: {constraints: [node.hostname == a]}}
  web: {image: nginx, ports: ["8080:80"]}
`
	got := statefulFindings(t, svcs, "swarm", "routing mesh")
	for _, name := range []string{"public", "loop"} {
		msg, ok := got[name]
		if !ok {
			t.Errorf("%s: want swarm port warning", name)
			continue
		}
		if !strings.Contains(msg, "every node") || !strings.Contains(msg, "stack network") || !strings.Contains(msg, "loopback too") || strings.Contains(msg, "bind to loopback") {
			t.Errorf("%s: wrong advice for swarm: %s", name, msg)
		}
	}
	if !strings.Contains(got["loop"], "5433:5432") {
		t.Errorf("loopback bind not listed: %s", got["loop"])
	}
	for _, name := range []string{"noports", "web"} {
		if msg, ok := got[name]; ok {
			t.Errorf("%s: unexpected warning: %s", name, msg)
		}
	}
	// The compose "all interfaces" warning is not repeated on swarm.
	if all := statefulFindings(t, svcs, "swarm", "on all interfaces"); len(all) > 0 {
		t.Errorf("compose advice on swarm: %v", all)
	}
}

func TestStatefulHostNetworkWarning(t *testing.T) {
	const svcs = `
  hostdb: {image: postgres, restart: always, x-yoho: {stateful: true}, network_mode: host}
  bridgedb: {image: postgres, restart: always, x-yoho: {stateful: true}}
  hostweb: {image: nginx, restart: always, network_mode: host}
`
	got := statefulFindings(t, svcs, "compose", "network_mode: host")
	if msg, ok := got["hostdb"]; !ok || !strings.Contains(msg, "backup restore and drill refuse host networking") {
		t.Errorf("hostdb: want host network warning, got %q", msg)
	}
	for _, name := range []string{"bridgedb", "hostweb"} {
		if msg, ok := got[name]; ok {
			t.Errorf("%s: unexpected warning: %s", name, msg)
		}
	}
}
