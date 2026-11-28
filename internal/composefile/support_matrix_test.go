package composefile

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/config"
)

// TestSupportMatrixComposeRefusals asserts that every compose-validated "No"
// cell in docs/site/support-matrix.md fails Check with the documented error.
func TestSupportMatrixComposeRefusals(t *testing.T) {
	cases := []struct {
		name    string
		runtime string
		compose string
		files   []string
		want    string
	}{
		{
			name:    "stateful service cannot be proxied (compose)",
			runtime: "compose",
			compose: `
services:
  db:
    image: postgres:17
    healthcheck: {test: ["CMD", "true"]}
    x-yoho:
      stateful: true
      proxy: {hosts: [db.example.com], port: 5432}
`,
			want: "stateful service cannot be proxied; it is recreated stop-first and has no zero-downtime cutover",
		},
		{
			name:    "stateful service cannot be proxied (swarm)",
			runtime: "swarm",
			compose: `
services:
  db:
    image: postgres:17
    healthcheck: {test: ["CMD", "true"]}
    x-yoho:
      stateful: true
      proxy: {hosts: [db.example.com], port: 5432}
`,
			want: "stateful service cannot be proxied; it is recreated stop-first and has no zero-downtime cutover",
		},
		{
			name:    "x-yoho.check on swarm",
			runtime: "swarm",
			compose: `
services:
  mqtt:
    image: eclipse-mosquitto:2
    x-yoho:
      check: {type: tcp, port: 1883}
`,
			want: "x-yoho.check is not supported with runtime swarm yet",
		},
		{
			name:    "app-dir bind mount on swarm",
			runtime: "swarm",
			files:   []string{"cfg/init.sql"},
			compose: `
services:
  db:
    image: postgres:17
    volumes:
      - ./cfg/init.sql:/docker-entrypoint-initdb.d/init.sql:ro
    x-yoho:
      stateful: true
`,
			want: "comes from the App directory; the swarm runtime does not ship files to nodes (bake it into the image or use a named volume)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, rel := range tc.files {
				p := filepath.Join(dir, rel)
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte("ok"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(tc.compose), 0o644); err != nil {
				t.Fatal(err)
			}
			r, err := Load(context.Background(), dir, nil, nil, "", "")
			if err != nil {
				t.Fatalf("Load failed: %v", err)
			}
			var errs []string
			for _, f := range Check(r, tc.runtime, nil) {
				if f.Level == LevelError {
					errs = append(errs, f.Message)
				}
			}
			joined := strings.Join(errs, "\n")
			if !strings.Contains(joined, tc.want) {
				t.Errorf("Check errors:\n%s\nwant substring %q", joined, tc.want)
			}
		})
	}
}

// TestSupportMatrixTagsAndInstances covers the x-yoho.tags and instance
// Connection refusals (ADR 0020) enforced by CheckTags and CheckRoles.
func TestSupportMatrixTagsAndInstances(t *testing.T) {
	servers := map[string]config.Server{"a": {Tags: []string{"eu"}}, "b": {}}
	instances := config.Destination{Servers: []string{"a", "b"},
		Roles:       map[string]config.Role{"api": {Services: []string{"api"}, Servers: []string{"a", "b"}}, "lb": {Services: []string{"lb"}, Servers: []string{"b"}}},
		Connections: map[string]config.Connection{"upstream": {Service: "api", Port: 8000, Consumers: []string{"lb"}, Instances: true}}}
	cases := []struct {
		name    string
		dest    config.Destination
		compose string
		want    string
	}{
		{
			name:    "x-yoho.tags on swarm",
			dest:    config.Destination{Servers: []string{"a"}, Runtime: "swarm"},
			compose: "services:\n  web:\n    image: nginx\n    x-yoho:\n      tags:\n        eu: {env: {REGION: eu}}\n",
			want:    "x-yoho.tags supports the compose runtime only; set the environment in the Compose file instead",
		},
		{
			name:    "x-yoho.tags with scoped access",
			dest:    config.Destination{Servers: []string{"a"}, Access: "scoped"},
			compose: "services:\n  web:\n    image: nginx\n    x-yoho:\n      tags:\n        eu: {env: {REGION: eu}}\n",
			want:    "x-yoho.tags requires operator access; the Gate refuses per-Server environment",
		},
		{
			name:    "proxied instance provider",
			dest:    instances,
			compose: "services:\n  api:\n    image: nginx\n    x-yoho:\n      proxy: {hosts: [api.example.com], port: 8000, ssl: false}\n  lb:\n    image: nginx\n",
			want:    "instance connection upstream provider cannot be proxied; consumers address each Server's container directly",
		},
		{
			name:    "instance provider with replicas",
			dest:    instances,
			compose: "services:\n  api:\n    image: nginx\n    deploy: {replicas: 2}\n  lb:\n    image: nginx\n",
			want:    "instance connection upstream provider must run exactly one container per Server (replicas: 1)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(tc.compose), 0o644); err != nil {
				t.Fatal(err)
			}
			r, err := Load(context.Background(), dir, nil, nil, "", "")
			if err != nil {
				t.Fatalf("Load failed: %v", err)
			}
			var errs []string
			for _, f := range append(CheckTags(r, servers, tc.dest), CheckRoles(r, tc.dest, config.ProxyConfig{}, "shop")...) {
				if f.Level == LevelError {
					errs = append(errs, f.Message)
				}
			}
			joined := strings.Join(errs, "\n")
			if !strings.Contains(joined, tc.want) {
				t.Errorf("errors:\n%s\nwant substring %q", joined, tc.want)
			}
		})
	}
}
