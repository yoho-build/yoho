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
	"time"

	"github.com/spf13/cobra"
	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/ui"
)

func TestPreviewPruneConfirmAndValidation(t *testing.T) {
	tests := []struct {
		name    string
		jsonOut bool
		args    []string
		wantErr string
	}{
		{
			name:    "requires older-than or keep",
			args:    []string{"prune", "--yes"},
			wantErr: "specify --older-than",
		},
		{
			name:    "invalid duration",
			args:    []string{"prune", "--older-than", "0d", "--yes"},
			wantErr: "invalid duration",
		},
		{name: "empty duration with keep", args: []string{"prune", "--older-than=", "--keep", "0", "--yes"}, wantErr: "invalid duration"},
		{name: "whitespace duration with keep", args: []string{"prune", "--older-than", " \t", "--keep", "0", "--yes"}, wantErr: "invalid duration"},
		{
			name:    "negative keep",
			args:    []string{"prune", "--keep", "-2", "--yes"},
			wantErr: "--keep must be 0 or greater",
		},
		{
			name:    "json requires yes or auto-approve",
			jsonOut: true,
			args:    []string{"prune", "--older-than", "7d"},
			wantErr: "--json requires --yes or --auto-approve",
		},
		{
			name:    "non-tty requires yes or auto-approve",
			args:    []string{"prune", "--keep", "1"},
			wantErr: "stdin is not a terminal",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := previewFixture(t, "")
			g.json = tc.jsonOut
			c := previewCmd(g)
			var out strings.Builder
			c.SetOut(&out)
			c.SetErr(&out)
			c.SetIn(strings.NewReader(""))
			c.SilenceErrors, c.SilenceUsage = true, true
			c.SetArgs(tc.args)
			err := c.ExecuteContext(context.Background())
			if err == nil {
				t.Fatalf("expected error containing %q, got nil\n%s", tc.wantErr, out.String())
			}
			if !strings.Contains(err.Error(), tc.wantErr) && !strings.Contains(out.String(), tc.wantErr) {
				t.Fatalf("err=%v out=%s; want %q", err, out.String(), tc.wantErr)
			}
		})
	}
}

func TestPreviewMaxActiveEnforcement(t *testing.T) {
	oldRoot := release.Root
	release.Root = t.TempDir()
	t.Cleanup(func() { release.Root = oldRoot })

	mkPreview := func(name string) {
		dest := "preview-" + name
		dir := release.AppDir("shop", dest)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		rec := deploy.PreviewRecord{
			App: "shop", Name: name, Destination: dest,
			Project: release.ProjectName("shop", dest), Services: []string{"web"},
		}
		b, _ := json.Marshal(rec)
		if err := os.WriteFile(filepath.Join(dir, "preview.json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mkPreview("pr-1")
	mkPreview("pr-2")

	newApp := func(destName string, maxActive int) *app {
		return &app{
			cfg: &config.Config{
				App:          "shop",
				Destinations: map[string]config.Destination{"staging": {}},
				Previews:     &config.PreviewsConfig{From: "staging", Host: "{name}.preview.example.com", MaxActive: maxActive},
			},
			destName: destName,
			ui:       ui.Discard(),
		}
	}

	h := &remote.Local{}
	ctx := context.Background()

	// Creating a 3rd preview when max_active is 2 must fail with a prune hint.
	a := newApp("preview-pr-3", 2)
	err := a.checkPreviewMaxActive(ctx, h)
	if err == nil || !strings.Contains(err.Error(), "preview limit reached") {
		t.Fatalf("expected max_active refusal, got: %v", err)
	}

	// Updating an existing preview when at the cap must succeed.
	a = newApp("preview-pr-2", 2)
	if err := a.checkPreviewMaxActive(ctx, h); err != nil {
		t.Fatalf("updating existing preview refused: %v", err)
	}

	// Under the cap must succeed.
	a = newApp("preview-pr-3", 3)
	if err := a.checkPreviewMaxActive(ctx, h); err != nil {
		t.Fatalf("under cap refused: %v", err)
	}

	// Unlimited (0) must succeed.
	a = newApp("preview-pr-3", 0)
	if err := a.checkPreviewMaxActive(ctx, h); err != nil {
		t.Fatalf("unlimited refused: %v", err)
	}
}

func TestPreviewRecordIncludesPRAndBranch(t *testing.T) {
	g := previewFixture(t, "")
	a, err := g.loadPreview(&cobra.Command{Use: "up"}, "pr-78")
	if err != nil {
		t.Fatal(err)
	}
	a.previewPR = 78
	a.previewBranch = "feat/preview-prune"
	r, err := a.compose(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p := a.previewRecord(&session{r: r, ver: "v1"})
	if p.PR != 78 || p.Branch != "feat/preview-prune" {
		t.Fatalf("unexpected PR/Branch: %+v", p)
	}
}

func TestFormatPreviewTimestamps(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	age, last := formatPreviewTimestamps(deploy.PreviewRecord{}, now)
	if age != "unknown" || last != "unknown" {
		t.Fatalf("zero timestamps: age=%q last=%q", age, last)
	}
	age, last = formatPreviewTimestamps(deploy.PreviewRecord{
		CreatedAt:      now.Add(-5 * 24 * time.Hour),
		LastDeployedAt: now.Add(-3 * time.Hour),
	}, now)
	if age != "5d" || last != "3h ago" {
		t.Fatalf("known timestamps: age=%q last=%q", age, last)
	}
}

type previewCheckHost struct {
	remote.Host
	output func(context.Context, remote.Cmd) (string, error)
	write  func(context.Context, string, []byte, os.FileMode, bool) error
}

func (h *previewCheckHost) Output(ctx context.Context, c remote.Cmd) (string, error) {
	if h.output != nil {
		return h.output(ctx, c)
	}
	return h.Host.Output(ctx, c)
}

func (h *previewCheckHost) WriteFile(ctx context.Context, p string, b []byte, m os.FileMode, sudo bool) error {
	if h.write != nil {
		return h.write(ctx, p, b, m, sudo)
	}
	return h.Host.WriteFile(ctx, p, b, m, sudo)
}

func TestPreviewPruneRevalidatesUnderLock(t *testing.T) {
	for _, field := range []string{"last_deployed_at", "created_at", "identity"} {
		t.Run(field, func(t *testing.T) {
			oldRoot := release.Root
			release.Root = t.TempDir()
			t.Cleanup(func() { release.Root = oldRoot })
			ctx := context.WithValue(context.Background(), auditRunKey{}, &auditRun{loader: func() *auditRun {
				t.Fatal("operation started for a preview changed under the lock")
				return nil
			}})
			local := &remote.Local{}
			record := deploy.PreviewRecord{App: "shop", Name: "pr-1", Destination: "preview-pr-1", Project: release.ProjectName("shop", "preview-pr-1"), CreatedAt: time.Now().UTC().Add(-time.Hour)}
			marker := filepath.Join(release.AppDir(record.App, record.Destination), "preview.json")
			write := func(p deploy.PreviewRecord) {
				t.Helper()
				b, _ := json.Marshal(p)
				if err := local.WriteFile(ctx, marker, b, 0600, false); err != nil {
					t.Fatal(err)
				}
			}
			write(record)
			changed := record
			switch field {
			case "last_deployed_at":
				changed.LastDeployedAt = time.Now().UTC()
			case "created_at":
				changed.CreatedAt = time.Now().UTC()
			case "identity":
				changed.Version = "new-release"
			}
			locked := false
			h := &previewCheckHost{Host: local}
			h.output = func(ctx context.Context, c remote.Cmd) (string, error) {
				if strings.Contains(c.Script, "docker") {
					t.Fatal("teardown started for changed preview")
				}
				out, err := local.Output(ctx, c)
				if out == "acquired" && err == nil {
					locked = true
					write(changed)
				}
				return out, err
			}
			a := &app{cfg: &config.Config{App: "shop"}, ui: ui.Discard()}
			_, _, err := a.removePreviewOnHost(ctx, plan.NamedHost{Host: h}, record.Name, record.Destination, "preview prune", &record)
			if !locked || !errors.Is(err, errPreviewChanged) {
				t.Fatalf("locked=%v err=%v", locked, err)
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatal("changed marker removed", err)
			}
			if _, err := os.Stat(deploy.LockDir(record.App, record.Destination)); !os.IsNotExist(err) {
				t.Fatal("Destination lock leaked", err)
			}
		})
	}
}

func TestPreviewAdmissionHoldsAppLock(t *testing.T) {
	oldRoot := release.Root
	release.Root = t.TempDir()
	t.Cleanup(func() { release.Root = oldRoot })
	ctx := context.Background()
	local := &remote.Local{}
	lockDir := filepath.Join(release.AppDir("shop", ""), "preview.lock")
	assertLocked := func() {
		t.Helper()
		if _, err := os.Stat(filepath.Join(lockDir, "info.json")); err != nil {
			t.Fatal("preview count/marker accessed without App lock", err)
		}
	}
	record := deploy.PreviewRecord{App: "shop", Name: "pr-1", Destination: "preview-pr-1", Project: release.ProjectName("shop", "preview-pr-1")}
	b, _ := json.Marshal(record)
	if err := local.WriteFile(ctx, filepath.Join(release.AppDir("shop", record.Destination), "preview.json"), b, 0600, false); err != nil {
		t.Fatal(err)
	}
	h := &previewCheckHost{Host: local}
	counted, marked := false, false
	h.output = func(ctx context.Context, c remote.Cmd) (string, error) {
		if strings.Contains(c.Script, "for d in ") && strings.Contains(c.Script, "preview.json") {
			assertLocked()
			counted = true
		}
		return local.Output(ctx, c)
	}
	h.write = func(ctx context.Context, p string, b []byte, mode os.FileMode, sudo bool) error {
		assertLocked()
		marked = true
		return local.WriteFile(ctx, p, b, mode, sudo)
	}
	a := &app{cfg: &config.Config{App: "shop", Previews: &config.PreviewsConfig{MaxActive: 1}}, destName: record.Destination, ui: ui.Discard()}
	if err := a.admitPreview(ctx, h, record); err != nil {
		t.Fatal(err)
	}
	if !counted || !marked {
		t.Fatalf("counted=%v marked=%v", counted, marked)
	}
	if _, err := os.Stat(lockDir); !os.IsNotExist(err) {
		t.Fatal("App lock leaked", err)
	}
	// A different name uses the same App lock, including on the refusal path.
	a.destName = "preview-pr-2"
	record.Name, record.Destination, record.Project = "pr-2", a.destName, release.ProjectName("shop", a.destName)
	marked = false
	if err := a.admitPreview(ctx, h, record); err == nil || !strings.Contains(err.Error(), "preview limit reached") {
		t.Fatal("cap not enforced", err)
	}
	if marked {
		t.Fatal("marker written despite cap")
	}
	if _, err := os.Stat(lockDir); !os.IsNotExist(err) {
		t.Fatal("App lock leaked on refusal", err)
	}
	unlock, err := deploy.AcquirePreviewLock(ctx, local, "shop", deploy.LockInfo{})
	if err != nil {
		t.Fatal(err)
	}
	defer unlock(ctx)
	if err := a.admitPreview(ctx, h, record); err == nil {
		t.Fatal("another preview bypassed App lock")
	}
}

func TestPreviewPruneJSONOutcomes(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprintf("changed-%v", changed), func(t *testing.T) {
			g := previewFixture(t, "")
			root := t.TempDir()
			cfg, err := os.ReadFile(g.configPath)
			if err != nil {
				t.Fatal(err)
			}
			cfg = []byte(strings.Replace(string(cfg), "ssh: user@example.com", fmt.Sprintf("ssh: local, root: %q", root), 1))
			if err := os.WriteFile(g.configPath, cfg, 0600); err != nil {
				t.Fatal(err)
			}
			g.json = true
			local := &remote.Local{}
			record := deploy.PreviewRecord{App: "shop", Name: "pr-1", Destination: "preview-pr-1", Project: release.ProjectName("shop", "preview-pr-1"), CreatedAt: time.Now().UTC().Add(-48 * time.Hour)}
			marker := filepath.Join(root, "apps", "shop", record.Destination, "preview.json")
			write := func() {
				t.Helper()
				b, _ := json.Marshal(record)
				if err := local.WriteFile(context.Background(), marker, b, 0600, false); err != nil {
					t.Fatal(err)
				}
			}
			if changed {
				write()
			}
			h := &previewCheckHost{Host: local}
			h.output = func(ctx context.Context, c remote.Cmd) (string, error) {
				if strings.HasPrefix(c.Script, "docker version") {
					return "27.0.0", nil
				}
				if strings.Contains(c.Script, "docker") {
					t.Fatal("unexpected teardown")
				}
				out, err := local.Output(ctx, c)
				if changed && out == "acquired" && err == nil {
					record.LastDeployedAt = time.Now().UTC()
					write()
				}
				return out, err
			}
			oldConnect := connectServer
			connectServer = func(string, string, bool, remote.SSHOptions) (remote.Host, error) { return h, nil }
			t.Cleanup(func() { connectServer = oldConnect })
			c := previewCmd(g)
			var out strings.Builder
			c.SetOut(&out)
			c.SetErr(&out)
			c.SetArgs([]string{"prune", "--keep", "0", "--yes"})
			if err := c.ExecuteContext(context.Background()); err != nil {
				t.Fatalf("%v\n%s", err, out.String())
			}
			found := false
			for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
				var event map[string]any
				if err := json.Unmarshal([]byte(line), &event); err != nil {
					t.Fatal(err, line)
				}
				if event["event"] != "preview.prune" {
					continue
				}
				found = true
				unknown, ok := event["skipped_unknown_age"].([]any)
				if !ok || len(unknown) != 0 {
					t.Fatalf("expected empty JSON array: %s", line)
				}
				outcomes := event["outcomes"].([]any)
				if changed {
					if len(outcomes) != 1 || outcomes[0].(map[string]any)["status"] != "skipped_changed" {
						t.Fatal(line)
					}
				} else if len(outcomes) != 0 {
					t.Fatal(line)
				}
			}
			if !found {
				t.Fatalf("no prune event: %s", out.String())
			}
		})
	}
}
