package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/gate"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// planHost answers the Gate's read-only docker queries for an App that
// was never deployed, and records every script.
type planHost struct {
	mu      sync.Mutex
	scripts []string
}

func (h *planHost) Name() string { return "srv" }
func (h *planHost) Run(_ context.Context, c remote.Cmd) error {
	h.mu.Lock()
	h.scripts = append(h.scripts, c.Script)
	h.mu.Unlock()
	if c.Stdin != nil {
		io.Copy(io.Discard, c.Stdin)
	}
	out := ""
	switch {
	case strings.Contains(c.Script, "{{.LoggingDriver}}"):
		out = "json-file"
	case strings.Contains(c.Script, "kamal-proxy list --json"):
		out = "null"
	}
	if c.Stdout != nil {
		io.WriteString(c.Stdout, out)
	}
	return nil
}
func (h *planHost) Output(ctx context.Context, c remote.Cmd) (string, error) {
	var b bytes.Buffer
	c.Stdout = &b
	err := h.Run(ctx, c)
	return strings.TrimSpace(b.String()), err
}
func (h *planHost) WriteFile(context.Context, string, []byte, os.FileMode, bool) error {
	h.mu.Lock()
	h.scripts = append(h.scripts, "write")
	h.mu.Unlock()
	return nil
}
func (h *planHost) ReadFile(context.Context, string, bool) ([]byte, error) {
	return nil, os.ErrNotExist
}
func (h *planHost) Close() error { return nil }

// helloFilter drops the plan capability from hello results, as a Gate
// that predates the plan verb answers.
type helloFilter struct{ w io.Writer }

func (f helloFilter) Write(p []byte) (int, error) {
	if _, err := f.w.Write(bytes.ReplaceAll(p, []byte(`,"plan":true`), nil)); err != nil {
		return 0, err
	}
	return len(p), nil
}

// inProcessGate makes the CLI's Gate connections run gate.Serve here.
func inProcessGate(t *testing.T, h remote.Host, old bool) {
	t.Helper()
	exec := func(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) error {
		for _, a := range argv {
			if a == "-O" {
				return nil // ControlMaster stop
			}
		}
		if old {
			stdout = helloFilter{stdout}
		}
		if code := gate.Serve(ctx, gate.Options{Member: "alice", Key: "SHA256:k", Version: "test", In: stdin, Out: stdout,
			Getenv:  func(string) string { return "" },
			HostFor: func(string) (remote.Host, error) { return h, nil }}); code != 0 {
			return &exitError{code: code}
		}
		return nil
	}
	gateSSHOptions = func(o remote.SSHOptions) remote.SSHOptions {
		o.Exec, o.ControlDir = exec, t.TempDir()
		return o
	}
	t.Cleanup(func() { gateSSHOptions = nil })
}

func scopedPlanRun(t *testing.T, h remote.Host, old bool, transportMode ...string) (string, error) {
	t.Helper()
	t.Setenv("YOHO_ACCESS", "")
	oldRoot := release.Root
	release.Root = t.TempDir()
	t.Cleanup(func() { release.Root = oldRoot })
	dir := release.AccessDir("shop", "production")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(gate.MembersPath("shop", "production"), []byte("members:\n  alice: {verbs: [deploy]}\n"), 0o600)
	settings := gate.Settings{Server: "primary"}
	if len(transportMode) > 0 {
		settings.Registry = &config.Registry{Prefix: "registry.example"}
	}
	b, _ := json.Marshal(settings)
	os.WriteFile(gate.SettingsPath("shop", "production"), b, 0o600)
	inProcessGate(t, h, old)

	app := t.TempDir()
	os.WriteFile(filepath.Join(app, "yoho.yml"), []byte("app: shop\nservers: {primary: {ssh: deb@203.0.113.10, root: "+release.Root+"}}\ndestinations:\n  production: {servers: [primary], access: scoped}\n"), 0o644)
	os.WriteFile(filepath.Join(app, "compose.yaml"), []byte("services: {web: {image: nginx, restart: unless-stopped}}\n"), 0o644)
	if len(transportMode) > 0 {
		f, err := os.OpenFile(filepath.Join(app, "yoho.yml"), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, err = fmt.Fprintf(f, "transport: {mode: %s}\nregistry: {prefix: registry.example}\n", transportMode[0])
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(app, "compose.yaml"), []byte("services: {web: {build: .}}\n"), 0o644)
	}
	c := planCmd(&globals{configPath: filepath.Join(app, "yoho.yml"), json: len(transportMode) > 0})
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"--version", "v1"})
	c.SilenceErrors, c.SilenceUsage = true, true
	err := c.ExecuteContext(context.Background())
	return out.String(), err
}

// Issue #65: `yoho plan` with scoped access plans through the Gate and
// builds, ships and writes nothing.
func TestScopedPlanThroughGate(t *testing.T) {
	h := &planHost{}
	out, err := scopedPlanRun(t, h, false)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "Plan v1 through the Gate") || !strings.Contains(out, "service web") || !strings.Contains(out, "1 to add") {
		t.Errorf("output:\n%s", out)
	}
	all := strings.Join(h.scripts, "\n")
	for _, bad := range []string{"docker pull", "docker load", "docker compose", "write"} {
		if strings.Contains(all, bad) {
			t.Errorf("scoped plan ran %q:\n%s", bad, all)
		}
	}
	if _, err := os.Stat(filepath.Join(release.AccessDir("shop", "production"), "rollout")); !os.IsNotExist(err) {
		t.Errorf("scoped plan started a rollout: %v", err)
	}
}

// A Gate older than the plan verb gets a hint to update it.
func TestScopedPlanOlderGate(t *testing.T) {
	out, err := scopedPlanRun(t, &planHost{}, true)
	if err == nil || !strings.Contains(out, "the Server's Gate cannot plan") || !strings.Contains(out, "yoho access install") {
		t.Fatalf("error %v, output:\n%s", err, out)
	}
}

func TestScopedPlanTransport(t *testing.T) {
	for _, mode := range []string{"auto", "load", "registry", "pussh"} {
		t.Run(mode, func(t *testing.T) {
			out, err := scopedPlanRun(t, &planHost{}, false, mode)
			if mode == "pussh" {
				if err == nil || !strings.Contains(err.Error(), "not available through the Gate") {
					t.Fatalf("%v\n%s", err, out)
				}
				if strings.Contains(out, `"event":"transport"`) {
					t.Fatal(out)
				}
				return
			}
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			want := "load"
			if mode == "registry" {
				want = mode
			}
			found := false
			for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
				var e map[string]any
				if err := json.Unmarshal([]byte(line), &e); err != nil {
					t.Fatal(err)
				}
				if e["event"] == "transport" {
					found = true
					if e["mode"] != want {
						t.Fatal(line)
					}
				}
			}
			if !found {
				t.Fatal(out)
			}
		})
	}
}
