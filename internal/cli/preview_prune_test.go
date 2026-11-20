package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/deploy"
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
