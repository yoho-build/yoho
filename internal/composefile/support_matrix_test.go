package composefile

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
