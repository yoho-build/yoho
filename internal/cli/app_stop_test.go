package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// stopHost keeps files real and answers docker with a db that stops cleanly
// and becomes ready again.
type stopHost struct {
	remote.Local
	mu      sync.Mutex
	scripts []string
	stops   int
}

func (h *stopHost) Output(ctx context.Context, c remote.Cmd) (string, error) {
	if !strings.Contains(c.Script, "docker") {
		return h.Local.Output(ctx, c)
	}
	h.mu.Lock()
	h.scripts = append(h.scripts, c.Script)
	h.mu.Unlock()
	switch {
	case strings.Contains(c.Script, "docker stop $ids"):
		if h.stops++; h.stops > 1 {
			return "", nil // removed by the first stop
		}
		return "c1 0", nil
	case strings.Contains(c.Script, "'ps' '--all' '-q' 'db'"):
		return "db1", nil
	case strings.Contains(c.Script, "State.Status"):
		return "running healthy 0 0", nil
	}
	return "", nil
}
func (h *stopHost) Run(ctx context.Context, c remote.Cmd) error {
	_, err := h.Output(ctx, c)
	return err
}

// stopFixture is a deployed one-Server Destination with db and a proxied web.
func stopFixture(t *testing.T) (*globals, *stopHost, string) {
	t.Helper()
	g := previewFixture(t, "")
	dir := t.TempDir()
	setOperationTestRoot(t, g, dir)
	g.destination = "staging"
	g.json = true
	h := &stopHost{}
	previous := connectServer
	connectServer = func(string, string, bool, remote.SSHOptions) (remote.Host, error) { return h, nil }
	t.Cleanup(func() { connectServer = previous })
	release.Root = dir
	appDir := release.AppDir("shop", "staging")
	rel := filepath.Join(appDir, "releases", "v1")
	if err := os.MkdirAll(rel, 0o700); err != nil {
		t.Fatal(err)
	}
	plans := `[{"name":"db","replicas":1,"stateful":true},{"name":"web","replicas":1,"proxy":{"hosts":["x.example.com"],"port":80}}]`
	for name, data := range map[string]string{"plan.json": plans, "compose.yaml": "services: {}\n"} {
		if err := os.WriteFile(filepath.Join(rel, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("releases/v1", filepath.Join(appDir, "current")); err != nil {
		t.Fatal(err)
	}
	return g, h, appDir
}

func runAppStop(t *testing.T, g *globals, args ...string) (string, error) {
	t.Helper()
	root := &cobra.Command{Use: "yoho", SilenceErrors: true, SilenceUsage: true}
	root.AddCommand(appCmds(g))
	var out strings.Builder
	root.SetOut(&out)
	root.SetErr(&out)
	auditWrap(g, root)
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

func TestAppStopAndStart(t *testing.T) {
	g, h, appDir := stopFixture(t)
	out, err := runAppStop(t, g, "app", "stop", "db", "--reason", "maintenance")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var stopped deploy.Stopped
	b, err := os.ReadFile(filepath.Join(appDir, deploy.StoppedFile))
	if err != nil || json.Unmarshal(b, &stopped) != nil || stopped["db"].Reason != "maintenance" {
		t.Fatalf("%s %v", b, err)
	}
	if !strings.Contains(strings.Join(h.scripts, "\n"), deploy.StopScript("shop", "staging", "db")) {
		t.Fatalf("no stop script:\n%s", strings.Join(h.scripts, "\n---\n"))
	}
	if strings.Contains(strings.Join(h.scripts, "\n"), "volume") {
		t.Fatal("app stop touched a volume")
	}
	if !strings.Contains(out, `"service_stop"`) || !strings.Contains(out, `"changed":true`) || !strings.Contains(out, `"exit_codes":{"c1":0}`) {
		t.Fatalf("events:\n%s", out)
	}

	// Repeat: success, no change.
	out, err = runAppStop(t, g, "app", "stop", "db")
	if err != nil || !strings.Contains(out, `"changed":false`) {
		t.Fatalf("repeat stop: %v\n%s", err, out)
	}

	out, err = runAppStop(t, g, "app", "start", "db")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(appDir, deploy.StoppedFile)); !os.IsNotExist(err) {
		t.Fatal("start left the stopped record")
	}
	if !strings.Contains(out, `"service_start"`) || !strings.Contains(out, `"changed":true`) {
		t.Fatalf("events:\n%s", out)
	}
	out, err = runAppStop(t, g, "app", "start", "db")
	if err != nil || !strings.Contains(out, `"changed":false`) {
		t.Fatalf("repeat start: %v\n%s", err, out)
	}
}

func TestAppStopRefusals(t *testing.T) {
	g, h, appDir := stopFixture(t)
	for _, tc := range []struct{ args []string }{
		{[]string{"app", "stop", "web"}},  // proxied
		{[]string{"app", "stop", "nope"}}, // not in the Release
		{[]string{"app", "start", "web"}}, // proxied
		{[]string{"app", "stop", "db", "--server", "other"}},
	} {
		out, err := runAppStop(t, g, tc.args...)
		if err == nil || mutationCode(err, false) != 10 {
			t.Fatalf("%v accepted: %v\n%s", tc.args, err, out)
		}
	}
	if !strings.Contains(func() string { o, _ := runAppStop(t, g, "app", "stop", "web"); return o }(), "deploy.replicas: 0") {
		t.Fatal("proxied refusal lacks the replicas hint")
	}
	for _, s := range h.scripts {
		if strings.Contains(s, "docker stop") {
			t.Fatal("refusal mutated the Server")
		}
	}
	if _, err := os.Stat(filepath.Join(appDir, deploy.StoppedFile)); !os.IsNotExist(err) {
		t.Fatal("refusal wrote stopped.json")
	}
	if err := os.Remove(filepath.Join(appDir, "current")); err != nil {
		t.Fatal(err)
	}
	out, err := runAppStop(t, g, "app", "stop", "db")
	if err == nil || !strings.Contains(out, "no current Release") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestStoppedLines(t *testing.T) {
	got := stoppedLines(deploy.Stopped{"db": {By: "alice", Reason: "db promote primary"}}, nil)
	if len(got) != 1 || got[0] != "db: stopped by `yoho app stop` (alice: db promote primary); `yoho app start db` runs it" {
		t.Fatal(got)
	}
	// G12: an entry of a Service outside the current Release is marked.
	got = stoppedLines(deploy.Stopped{"db": {By: "alice"}, "gone": {By: "bob"}}, func(n string) bool { return n == "db" })
	if len(got) != 2 || got[1] != "gone: stopped by `yoho app stop` (bob) (not in the current Release); `yoho app start gone` clears the entry" {
		t.Fatal(got)
	}
}

// G12: `yoho app start` of a Service no longer in the current Release
// clears its orphaned entry.
func TestAppStartClearsOrphanedEntry(t *testing.T) {
	g, _, appDir := stopFixture(t)
	if err := os.WriteFile(filepath.Join(appDir, deploy.StoppedFile), []byte(`{"gone":{"by":"bob"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runAppStop(t, g, "app", "start", "gone")
	if err != nil || !strings.Contains(out, "cleared its stopped entry (not in the current Release)") {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(appDir, deploy.StoppedFile)); !os.IsNotExist(err) {
		t.Fatal("entry kept")
	}
	if out, err = runAppStop(t, g, "app", "start", "gone"); err == nil {
		t.Fatalf("unknown Service accepted: %s", out)
	}
}

// Round 6: `app start SVC --server A` reaches a Destination Server that no
// longer places SVC, to clear its orphaned entry.
func TestStartTargetsUnplacedServerForOrphan(t *testing.T) {
	old := release.Root
	release.Root = t.TempDir()
	t.Cleanup(func() { release.Root = old })
	h := &stopHost{}
	previous := connectServer
	connectServer = func(string, string, bool, remote.SSHOptions) (remote.Host, error) { return h, nil }
	t.Cleanup(func() { connectServer = previous })
	appDir := release.AppDir("shop", "production")
	rel := filepath.Join(appDir, "releases", "v1")
	os.MkdirAll(rel, 0o700)
	os.WriteFile(filepath.Join(rel, "plan.json"), []byte(`[{"name":"web","replicas":1}]`), 0o600)
	os.Symlink("releases/v1", filepath.Join(appDir, "current"))
	os.WriteFile(filepath.Join(appDir, deploy.StoppedFile), []byte(`{"db":{"by":"bob"}}`), 0o600)
	a := &app{
		cfg:      &config.Config{App: "shop", Servers: map[string]config.Server{"a": {SSH: "root@a"}, "b": {SSH: "root@b"}}},
		destName: "production",
		dest: config.Destination{Servers: []string{"a", "b"}, Roles: map[string]config.Role{
			"app":  {Services: []string{"web"}, Servers: []string{"a"}},
			"data": {Services: []string{"db"}, Servers: []string{"b"}},
		}},
		orphans: map[string]bool{},
	}
	hosts, done, err := a.stopTargets(context.Background(), "start", "db", "a")
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	if len(hosts) != 1 || hosts[0].Name != "a" || !a.orphans["a"] {
		t.Fatalf("%+v %v", hosts, a.orphans)
	}
	if _, _, err := a.stopTargets(context.Background(), "stop", "db", "a"); err == nil {
		t.Fatal("stop on an unplaced Server accepted")
	}
}
