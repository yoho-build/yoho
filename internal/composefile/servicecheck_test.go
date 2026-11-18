package composefile

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func checkFindings(t *testing.T, runtime, services string) map[string][]string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n"+services), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Load(context.Background(), dir, nil, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, f := range Check(r, runtime, nil) {
		if f.Level == LevelError && strings.Contains(f.Message, "x-yoho.check") {
			got[f.Service] = append(got[f.Service], f.Message)
		}
	}
	return got
}

func TestServiceCheckFindings(t *testing.T) {
	const svcs = `
  sync:
    image: a
    restart: unless-stopped
    x-yoho: {check: {type: websocket, port: 8080, path: /sync, timeout: 1m}}
  mqtt: {image: a, restart: unless-stopped, x-yoho: {check: {type: tcp, port: 1883}}}
  api: {image: a, restart: unless-stopped, x-yoho: {check: {type: http, port: 9000, path: "/healthz?full=1", host: api.internal}}}
  hostnet: {image: a, restart: unless-stopped, network_mode: host, x-yoho: {check: {type: tcp, port: 9100}}}
  web:
    image: a
    healthcheck: {test: [CMD, "true"]}
    x-yoho: {proxy: {port: 3000}, check: {type: http, port: 3000}}
  none: {image: a, restart: unless-stopped, network_mode: none, x-yoho: {check: {type: tcp, port: 1}}}
  side: {image: a, restart: unless-stopped, network_mode: "service:mqtt", x-yoho: {check: {type: tcp, port: 1}}}
  badport: {image: a, restart: unless-stopped, x-yoho: {check: {type: tcp, port: 70000}}}
  badtype: {image: a, restart: unless-stopped, x-yoho: {check: {type: grpc, port: 1}}}
  tcppath: {image: a, restart: unless-stopped, x-yoho: {check: {type: tcp, port: 1, path: /x}}}
  badpath: {image: a, restart: unless-stopped, x-yoho: {check: {type: http, port: 1, path: "x y"}}}
  badtimeout: {image: a, restart: unless-stopped, x-yoho: {check: {type: tcp, port: 1, timeout: 11m}}}
  migrate: {image: a, x-yoho: {check: {type: tcp, port: 1}}}
  app:
    image: a
    restart: unless-stopped
    depends_on:
      migrate: {condition: service_completed_successfully}
`
	got := checkFindings(t, "compose", svcs)
	for _, ok := range []string{"sync", "mqtt", "api", "hostnet", "app"} {
		if len(got[ok]) > 0 {
			t.Errorf("%s: %v", ok, got[ok])
		}
	}
	for svc, want := range map[string]string{
		"web":        "remove check and set x-yoho.proxy.health_path",
		"none":       "network_mode: none",
		"side":       "declare the check on that Service",
		"badport":    "not between 1 and 65535",
		"badtype":    `"grpc" is not tcp, http or websocket`,
		"tcppath":    "only apply to http and websocket",
		"badpath":    "must be an absolute path",
		"badtimeout": "above the maximum of 10m0s",
		"migrate":    "one-shot job",
	} {
		if len(got[svc]) != 1 || !strings.Contains(got[svc][0], want) {
			t.Errorf("%s: %v, want %q", svc, got[svc], want)
		}
	}
	swarm := checkFindings(t, "swarm", "  mqtt: {image: a, x-yoho: {check: {type: tcp, port: 1883}}}\n")
	if len(swarm["mqtt"]) != 1 || !strings.Contains(swarm["mqtt"][0], "not supported with runtime swarm") {
		t.Errorf("swarm: %v", swarm)
	}
}

func TestServiceCheckUnknownKey(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n  a: {image: a, x-yoho: {check: {type: tcp, port: 1, url: http://x}}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(context.Background(), dir, nil, nil, "", "")
	if err == nil || !strings.Contains(err.Error(), `unknown key "check.url"`) {
		t.Fatalf("err = %v", err)
	}
}
