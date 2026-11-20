package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/yoho-build/yoho/internal/audit"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// Requires an SSH Server with Docker. Uses a unique App, without a Proxy or
// volumes; cleanup removes only that App's Compose project and private root.
func TestE2ESavedPlanAndInterruptedOperation(t *testing.T) {
	if os.Getenv("YOHO_E2E") != "1" {
		t.Skip("YOHO_E2E=1 not set")
	}
	target := os.Getenv("YOHO_E2E_SSH")
	if target == "" {
		t.Skip("YOHO_E2E_SSH not set")
	}
	old := release.Root
	t.Cleanup(func() { release.Root = old })
	name := "outcome-" + audit.NewRun()
	root := "/tmp/yoho-" + name
	dir := t.TempDir()
	file := filepath.Join(dir, "yoho.json")
	cfg := fmt.Sprintf(`{"app":%q,"servers":{"one":{"ssh":%q,"root":%q}},"destinations":{"production":{"servers":["one"]}}}`, name, target, root)
	if err := os.WriteFile(file, []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	compose := filepath.Join(dir, "compose.yaml")
	writeCompose := func(marker string) {
		t.Helper()
		if err := os.WriteFile(compose, []byte("services:\n  worker:\n    image: busybox:1.37\n    command: [sh, -c, 'echo "+marker+"; sleep 300']\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeCompose("one")
	h, err := remote.NewSSH("one", target, false, remote.SSHOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		defer h.Close()
		_ = h.Run(context.Background(), remote.Cmd{Script: "set -eu\ndocker compose -p " + remote.Quote(release.ProjectName(name, "production")) + " -f " + remote.Quote(release.Dir(name, "production", "v1")+"/compose.yaml") + " down --remove-orphans 2>/dev/null || true\nrm -rf " + remote.Quote(root)})
	})
	runContext := func(ctx context.Context, args ...string) (string, error) {
		var out bytes.Buffer
		g := &globals{configPath: file, json: true}
		c := &cobra.Command{Use: "yoho", SilenceUsage: true, SilenceErrors: true}
		c.SetOut(&out)
		c.SetErr(&out)
		c.AddCommand(planCmd(g), applyCmd(g), deployCmd(g), operationStatusCmd(g))
		auditWrap(g, c)
		c.SetArgs(args)
		err := c.ExecuteContext(ctx)
		return out.String(), err
	}
	run := func(args ...string) (string, error) { return runContext(context.Background(), args...) }
	if out, err := run("deploy", "--version", "v1"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	artifact := filepath.Join(dir, "plan.json")
	if out, err := run("plan", "--version", "v2", "--out", artifact); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	writeCompose("two")
	if out, err := run("deploy", "--version", "unrelated"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if out, err := run("apply", "--plan", artifact, "--auto-approve"); mutationCode(err, false) != 15 {
		t.Fatalf("expected stale: %v\n%s", err, out)
	}
	if out, err := run("plan", "--version", "v2", "--out", artifact); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if out, err := run("apply", "--plan", artifact, "--auto-approve"); err != nil || !strings.Contains(out, `"event":"plan_verified"`) {
		t.Fatalf("%v\n%s", err, out)
	}
	// Interrupt an actual deploy during its long-running release command.
	body := "services:\n  worker:\n    image: busybox:1.37\n    command: [sh, -c, 'sleep 300']\n    x-yoho:\n      release_command: [sh, -c, 'sleep 30']\n"
	if err = os.WriteFile(compose, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() { out, err := runContext(ctx, "deploy", "--version", "interrupted"); done <- result{out, err} }()
	// The operation file proves the client has passed the mutation boundary.
	var id string
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		names, e := h.Output(context.Background(), remote.Cmd{Script: "set -eu\nfor f in " + remote.Quote(filepath.Join(root, "apps", name, "production", "operations")) + "/*.json; do [ -f \"$f\" ] || continue; cat \"$f\"; printf '\n'; done"})
		if e == nil {
			for _, line := range strings.Split(names, "\n") {
				var rec struct {
					ID    string `json:"id"`
					State string `json:"state"`
				}
				if json.Unmarshal([]byte(line), &rec) == nil && rec.State == "running" {
					id = rec.ID
					break
				}
			}
		}
		if id != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if id == "" {
		cancel()
		res := <-done
		t.Fatalf("deploy did not reach mutation: %v\n%s", res.err, res.out)
	}
	cancel()
	res := <-done
	if mutationCode(res.err, true) != 14 {
		t.Fatalf("want unknown: %v\n%s", res.err, res.out)
	}
	out, err := run("status", "--operation", id)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var final map[string]any
	if err = json.Unmarshal([]byte(lines[len(lines)-1]), &final); err != nil || final["outcome"] != "unknown" {
		t.Fatalf("%v\n%s", err, out)
	}
}
