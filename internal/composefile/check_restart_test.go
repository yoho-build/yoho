package composefile

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func restartFindings(t *testing.T, runtime, services string) map[string]bool {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n"+services), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Load(context.Background(), dir, nil, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range Check(r, runtime, nil) {
		if f.Level == LevelWarning && strings.Contains(f.Message, "restart policy `no`") {
			got[f.Service] = true
		}
	}
	return got
}

func TestRestartPolicyFinding(t *testing.T) {
	const svcs = `
  unset: {image: a}
  no: {image: a, restart: "no"}
  always: {image: a, restart: always}
  unless: {image: a, restart: unless-stopped}
  onfail: {image: a, restart: on-failure}
  deployed: {image: a, deploy: {restart_policy: {condition: any}}}
  migrate: {image: a}
  prof: {image: a, profiles: [debug]}
  app:
    image: a
    restart: unless-stopped
    depends_on:
      migrate: {condition: service_completed_successfully}
`
	got := restartFindings(t, "compose", svcs)
	for _, name := range []string{"unset", "no"} {
		if !got[name] {
			t.Errorf("%s: want restart warning", name)
		}
	}
	for _, name := range []string{"always", "unless", "onfail", "deployed", "migrate", "prof", "app"} {
		if got[name] {
			t.Errorf("%s: unexpected restart warning", name)
		}
	}
	if got := restartFindings(t, "swarm", svcs); len(got) != 0 {
		t.Errorf("swarm: want none, got %v", got)
	}
}
