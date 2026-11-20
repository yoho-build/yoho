package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/operation"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// Keep filesystem operations real, while modelling a Server with no Docker
// resources. Every destructive Docker command must have a persisted operation.
type operationPreviewHost struct {
	remote.Local
	t           *testing.T
	teardownErr error
}

func (h *operationPreviewHost) Output(ctx context.Context, c remote.Cmd) (string, error) {
	if strings.Contains(c.Script, "docker") {
		return "", nil
	}
	return h.Local.Output(ctx, c)
}
func (h *operationPreviewHost) Run(ctx context.Context, c remote.Cmd) error {
	if strings.Contains(c.Script, "docker") {
		r, _ := ctx.Value(auditRunKey{}).(*auditRun)
		if r == nil || r.op == nil {
			h.t.Fatal("preview teardown started without an operation")
		}
		b, err := os.ReadFile(filepath.Join(operation.Dir(r.rec.App, r.rec.Destination), r.op.ID+".json"))
		if err != nil {
			h.t.Fatal("operation not persisted before teardown:", err)
		}
		var record operation.Record
		if err := json.Unmarshal(b, &record); err != nil || record.State != "running" {
			h.t.Fatalf("operation before teardown: %s, %v", b, err)
		}
		return h.teardownErr
	}
	return h.Local.Run(ctx, c)
}

func TestPreviewPruneOperationSurvivesRemovals(t *testing.T) {
	for _, tc := range []struct {
		count        int
		disconnected bool
	}{{0, false}, {2, false}, {1, true}} {
		count := tc.count
		t.Run(fmt.Sprintf("%d-disconnected-%v", count, tc.disconnected), func(t *testing.T) {
			rootDir := t.TempDir()
			g := previewFixture(t, "")
			g.json = true
			setOperationTestRoot(t, g, rootDir)
			h := &operationPreviewHost{t: t}
			if tc.disconnected {
				h.teardownErr = &remote.ExitError{Code: 255}
			}
			previous := connectServer
			connectServer = func(string, string, bool, remote.SSHOptions) (remote.Host, error) { return h, nil }
			t.Cleanup(func() { connectServer = previous })
			release.Root = rootDir
			for i := 0; i < count; i++ {
				name := fmt.Sprintf("pr-%d", i+1)
				p := deploy.PreviewRecord{App: "shop", Name: name, Destination: "preview-" + name, Project: release.ProjectName("shop", "preview-"+name)}
				b, _ := json.Marshal(p)
				if err := h.WriteFile(context.Background(), filepath.Join(release.AppDir("shop", p.Destination), "preview.json"), b, 0600, false); err != nil {
					t.Fatal(err)
				}
			}
			root := &cobra.Command{Use: "yoho", SilenceErrors: true, SilenceUsage: true}
			root.AddCommand(previewCmd(g))
			var out strings.Builder
			root.SetOut(&out)
			root.SetErr(&out)
			auditWrap(g, root)
			root.SetArgs([]string{"preview", "prune", "--keep", "0", "--yes"})
			err := root.ExecuteContext(context.Background())
			if tc.disconnected {
				if err == nil || mutationCode(err, true) != 14 {
					t.Fatalf("lost attachment: %v\n%s", err, out.String())
				}
			} else if err != nil {
				t.Fatalf("%v\n%s", err, out.String())
			}
			files, err := filepath.Glob(filepath.Join(operation.Dir("shop", "staging"), "*.json"))
			if err != nil {
				t.Fatal(err)
			}
			if count == 0 {
				if len(files) != 0 {
					t.Fatalf("no-op prune created records: %v", files)
				}
				return
			}
			if len(files) != 1 {
				t.Fatalf("want one template operation, got %v\n%s", files, out.String())
			}
			id := strings.TrimSuffix(filepath.Base(files[0]), ".json")
			record, err := operation.Read(context.Background(), h, "shop", "staging", id)
			state, outcome := "finished", "success"
			if tc.disconnected {
				state, outcome = "running", "unknown"
			}
			if err != nil || record.Command != "preview prune" || record.State != state || record.Outcome != outcome {
				t.Fatalf("record=%+v err=%v", record, err)
			}
			previews, _ := filepath.Glob(filepath.Join(rootDir, "apps", "shop", "preview-*"))
			if tc.disconnected {
				if len(previews) != count {
					t.Fatalf("failed preview lost: %v", previews)
				}
			} else if len(previews) != 0 {
				t.Fatalf("removed previews recreated: %v", previews)
			}
		})
	}
}

type setupReadyHost struct {
	remote.Local
	t *testing.T
}

func (h *setupReadyHost) ReadFile(ctx context.Context, p string, sudo bool) ([]byte, error) {
	if p == "/etc/os-release" {
		return []byte("ID=ubuntu\nVERSION_CODENAME=noble\n"), nil
	}
	return h.Local.ReadFile(ctx, p, sudo)
}
func (h *setupReadyHost) Output(ctx context.Context, c remote.Cmd) (string, error) {
	if !preMutation(ctx) {
		h.t.Fatal("operation began during setup inspection")
	}
	if strings.Contains(c.Script, "echo root; elif") {
		return "root", nil
	}
	return "", nil // Every provisioning check reports already satisfied.
}

func (h *setupReadyHost) Run(ctx context.Context, c remote.Cmd) error {
	if strings.Contains(c.Script, "apt-get") || strings.Contains(c.Script, "wg ") {
		h.t.Fatal("unexpected provisioning or mesh mutation in setup test")
	}
	return h.Local.Run(ctx, c)
}

func TestSetupMeshOnlyStartsOperation(t *testing.T) {
	for _, planOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "apply", true: "plan"}[planOnly], func(t *testing.T) {
			g := previewFixture(t, "network: {mesh: wireguard}\nsetup: {firewall: false, auto_updates: false}\n")
			setOperationTestRoot(t, g, t.TempDir())
			g.destination = "staging"
			g.json = true
			h := &setupReadyHost{t: t}
			previous := connectServer
			calls := 0
			meshErr := errors.New("mesh connection stopped for test")
			connectServer = func(string, string, bool, remote.SSHOptions) (remote.Host, error) {
				calls++
				if calls == 3 { // provisioning, audit, then mesh
					files, _ := filepath.Glob(filepath.Join(operation.Dir("shop", "staging"), "*.json"))
					if len(files) != 1 {
						t.Fatalf("mesh reached without persisted operation: %v", files)
					}
					return nil, meshErr
				}
				return h, nil
			}
			t.Cleanup(func() { connectServer = previous })
			root := &cobra.Command{Use: "yoho", SilenceErrors: true, SilenceUsage: true}
			root.AddCommand(setupCmd(g))
			var out strings.Builder
			root.SetOut(&out)
			root.SetErr(&out)
			auditWrap(g, root)
			args := []string{"setup", "--yes", "--no-containerd-image-store"}
			if planOnly {
				args = append(args, "--plan")
			}
			root.SetArgs(args)
			err := root.ExecuteContext(context.Background())
			if planOnly {
				if err != nil || calls != 1 {
					t.Fatalf("plan: calls=%d err=%v\n%s", calls, err, out.String())
				}
			} else if !errors.Is(err, meshErr) || calls != 3 {
				t.Fatalf("mesh-only setup: calls=%d err=%v\n%s", calls, err, out.String())
			}
		})
	}
}

func setOperationTestRoot(t *testing.T, g *globals, dir string) {
	t.Helper()
	b, err := os.ReadFile(g.configPath)
	if err != nil {
		t.Fatal(err)
	}
	b = []byte(strings.Replace(string(b), "ssh: user@example.com", "ssh: user@example.com, endpoint: 127.0.0.1, root: "+dir, 1))
	if err := os.WriteFile(g.configPath, b, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestPreviewDownOperationBoundary(t *testing.T) {
	for _, marked := range []bool{false, true} {
		t.Run(fmt.Sprintf("marked-%v", marked), func(t *testing.T) {
			g := previewFixture(t, "")
			setOperationTestRoot(t, g, t.TempDir())
			g.json = true
			h := &operationPreviewHost{t: t}
			previous := connectServer
			calls := 0
			connectServer = func(string, string, bool, remote.SSHOptions) (remote.Host, error) { calls++; return h, nil }
			t.Cleanup(func() { connectServer = previous })
			root := &cobra.Command{Use: "yoho", SilenceErrors: true, SilenceUsage: true}
			root.AddCommand(previewCmd(g))
			var out strings.Builder
			root.SetOut(&out)
			root.SetErr(&out)
			auditWrap(g, root)
			// Loading sets the fixture's Server root before writing the marker.
			if _, err := g.loadPreview(root, "pr-1"); err != nil {
				t.Fatal(err)
			}
			if marked {
				p := deploy.PreviewRecord{App: "shop", Name: "pr-1", Destination: "preview-pr-1", Project: release.ProjectName("shop", "preview-pr-1")}
				b, _ := json.Marshal(p)
				if err := h.WriteFile(context.Background(), filepath.Join(release.AppDir("shop", p.Destination), "preview.json"), b, 0600, false); err != nil {
					t.Fatal(err)
				}
			}
			root.SetArgs([]string{"preview", "down", "--name", "pr-1", "--yes"})
			if err := root.ExecuteContext(context.Background()); err != nil {
				t.Fatalf("%v\n%s", err, out.String())
			}
			wantCalls := 1
			if marked {
				wantCalls++
			} // Only marked previews initialize the audit connection.
			if calls != wantCalls {
				t.Fatalf("connections=%d want=%d\n%s", calls, wantCalls, out.String())
			}
			if _, err := os.Stat(release.AppDir("shop", "preview-pr-1")); !os.IsNotExist(err) {
				t.Fatalf("removed preview directory remains: %v", err)
			}
		})
	}
}
