package deploy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

func TestMarkPreviewTimestampsAndMetadata(t *testing.T) {
	withRoot(t)
	ctx := context.Background()
	h := &dockerHost{local: &remote.Local{}}

	t1 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 11, 3, 14, 30, 0, 0, time.UTC)
	oldNow := previewNow
	t.Cleanup(func() { previewNow = oldNow })
	previewNow = func() time.Time { return t1 }

	p := PreviewRecord{
		App: "shop", Name: "pr-42", Destination: "preview-pr-42",
		Project: "yoho-shop-preview-pr-42", Services: []string{"web"}, Version: "v1",
		PR: 42, Branch: "feat/checkout",
	}
	dir := release.AppDir(p.App, p.Destination)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := MarkPreview(ctx, h, p); err != nil {
		t.Fatal(err)
	}
	got, err := ReadPreview(ctx, h, p.App, p.Destination)
	if err != nil {
		t.Fatal(err)
	}
	if !got.CreatedAt.Equal(t1) || !got.LastDeployedAt.Equal(t1) || got.PR != 42 || got.Branch != "feat/checkout" {
		t.Fatalf("first mark: %+v", got)
	}

	// Second deploy preserves CreatedAt, PR, Branch when omitted, and advances LastDeployedAt.
	previewNow = func() time.Time { return t2 }
	p2 := PreviewRecord{
		App: "shop", Name: "pr-42", Destination: "preview-pr-42",
		Project: "yoho-shop-preview-pr-42", Services: []string{"web"}, Version: "v2",
	}
	if err := MarkPreview(ctx, h, p2); err != nil {
		t.Fatal(err)
	}
	got, err = ReadPreview(ctx, h, p.App, p.Destination)
	if err != nil {
		t.Fatal(err)
	}
	if !got.CreatedAt.Equal(t1) || !got.LastDeployedAt.Equal(t2) || got.PR != 42 || got.Branch != "feat/checkout" {
		t.Fatalf("second mark: %+v", got)
	}
}

func TestMarkPreviewLegacyRecordKeepsUnknownCreatedAt(t *testing.T) {
	withRoot(t)
	ctx := context.Background()
	h := &dockerHost{local: &remote.Local{}}
	dir := release.AppDir("shop", "preview-pr-9")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Legacy marker without created_at or last_deployed_at.
	legacy := `{"app":"shop","name":"pr-9","destination":"preview-pr-9","project":"yoho-shop-preview-pr-9","hosts":["pr-9.example.com"],"services":["web"],"version":"v1"}`
	if err := os.WriteFile(filepath.Join(dir, "preview.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadPreview(ctx, h, "shop", "preview-pr-9")
	if err != nil {
		t.Fatal(err)
	}
	if !got.CreatedAt.IsZero() || !got.LastDeployedAt.IsZero() || !got.ActivityTime().IsZero() {
		t.Fatalf("legacy record should have zero timestamps: %+v", got)
	}

	t2 := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	oldNow := previewNow
	t.Cleanup(func() { previewNow = oldNow })
	previewNow = func() time.Time { return t2 }

	p := PreviewRecord{App: "shop", Name: "pr-9", Destination: "preview-pr-9", Project: "yoho-shop-preview-pr-9", Services: []string{"web"}, Version: "v2"}
	if err := MarkPreview(ctx, h, p); err != nil {
		t.Fatal(err)
	}
	got, err = ReadPreview(ctx, h, "shop", "preview-pr-9")
	if err != nil {
		t.Fatal(err)
	}
	if !got.CreatedAt.IsZero() || !got.LastDeployedAt.Equal(t2) {
		t.Fatalf("legacy redeploy: created=%v last=%v", got.CreatedAt, got.LastDeployedAt)
	}
	// Verify zero CreatedAt is omitted from JSON (omitzero).
	b, _ := os.ReadFile(filepath.Join(dir, "preview.json"))
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if _, exists := raw["created_at"]; exists {
		t.Fatalf("zero created_at should be omitted: %s", b)
	}
}

func TestSelectPrunePreviews(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	mk := func(name string, createdAgo, deployedAgo time.Duration) PreviewRecord {
		r := PreviewRecord{App: "shop", Name: name, Destination: "preview-" + name}
		if createdAgo > 0 {
			r.CreatedAt = now.Add(-createdAgo)
		}
		if deployedAgo > 0 {
			r.LastDeployedAt = now.Add(-deployedAgo)
		}
		return r
	}
	records := []PreviewRecord{
		mk("pr-old", 15*24*time.Hour, 15*24*time.Hour),
		mk("pr-mid", 10*24*time.Hour, 10*24*time.Hour),
		mk("pr-active-old-created", 14*24*time.Hour, 2*time.Hour),
		mk("pr-new", 1*time.Hour, 1*time.Hour),
		mk("pr-legacy", 0, 0),
	}

	names := func(cands []PreviewPruneCandidate) string {
		out := make([]string, len(cands))
		for i, c := range cands {
			out[i] = c.Record.Name
		}
		return strings.Join(out, ",")
	}

	tests := []struct {
		name        string
		opts        PreviewPruneOptions
		wantRemove  string
		wantSkipped int
	}{
		{
			name:        "older-than skips unknown age by default",
			opts:        PreviewPruneOptions{OlderThan: 7 * 24 * time.Hour, Now: now},
			wantRemove:  "pr-mid,pr-old",
			wantSkipped: 1,
		},
		{
			name:        "older-than with include-unknown-age",
			opts:        PreviewPruneOptions{OlderThan: 7 * 24 * time.Hour, IncludeUnknownAge: true, Now: now},
			wantRemove:  "pr-mid,pr-old,pr-legacy",
			wantSkipped: 0,
		},
		{
			name:        "keep 2 keeps two newest",
			opts:        PreviewPruneOptions{HasKeep: true, Keep: 2, Now: now},
			wantRemove:  "pr-mid,pr-old,pr-legacy",
			wantSkipped: 0,
		},
		{
			name:        "keep 0 removes all",
			opts:        PreviewPruneOptions{HasKeep: true, Keep: 0, Now: now},
			wantRemove:  "pr-new,pr-active-old-created,pr-mid,pr-old,pr-legacy",
			wantSkipped: 0,
		},
		{
			name:        "keep 3 with older-than 7d protects newest 3",
			opts:        PreviewPruneOptions{HasKeep: true, Keep: 3, OlderThan: 7 * 24 * time.Hour, Now: now},
			wantRemove:  "pr-old",
			wantSkipped: 1,
		},
		{
			name:        "keep 10 removes nothing",
			opts:        PreviewPruneOptions{HasKeep: true, Keep: 10, Now: now},
			wantRemove:  "",
			wantSkipped: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan := SelectPrunePreviews(records, tc.opts)
			if got := names(plan.Remove); got != tc.wantRemove {
				t.Errorf("remove = %q, want %q", got, tc.wantRemove)
			}
			if len(plan.SkippedUnknownAge) != tc.wantSkipped {
				t.Errorf("skipped = %d, want %d", len(plan.SkippedUnknownAge), tc.wantSkipped)
			}
		})
	}
}

func TestCheckMaxActive(t *testing.T) {
	active := []PreviewRecord{
		{Name: "pr-1", Destination: "preview-pr-1"},
		{Name: "pr-2", Destination: "preview-pr-2"},
	}
	tests := []struct {
		name    string
		target  string
		max     int
		wantErr bool
	}{
		{"unlimited", "preview-pr-3", 0, false},
		{"under cap", "preview-pr-3", 3, false},
		{"at cap new preview refused", "preview-pr-3", 2, true},
		{"at cap existing preview allowed", "preview-pr-2", 2, false},
		{"over cap existing preview allowed", "preview-pr-1", 1, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckMaxActive(active, tc.target, tc.max)
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestParsePreviewDuration(t *testing.T) {
	tests := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"7d", 7 * 24 * time.Hour, false},
		{"2w", 14 * 24 * time.Hour, false},
		{"48h", 48 * time.Hour, false},
		{"30m", 30 * time.Minute, false},
		{"0d", 0, true},
		{"-7d", 0, true},
		{"0h", 0, true},
		{"-1h", 0, true},
		{"abc", 0, true},
		{"", 0, true},
	}
	for _, tc := range tests {
		got, err := ParsePreviewDuration(tc.in)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("ParsePreviewDuration(%q) = (%v, %v), want (%v, err=%v)", tc.in, got, err, tc.want, tc.wantErr)
		}
	}
}

func TestFormatPreviewAge(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{-5 * time.Second, "<1m"},
		{20 * time.Second, "<1m"},
		{15 * time.Minute, "15m"},
		{5 * time.Hour, "5h"},
		{72 * time.Hour, "3d"},
	}
	for _, tc := range tests {
		if got := FormatPreviewAge(tc.d); got != tc.want {
			t.Errorf("FormatPreviewAge(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestPreviewDiskUsage(t *testing.T) {
	withRoot(t)
	dir := release.AppDir("shop", "preview-pr-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "preview.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	usage := PreviewDiskUsage(context.Background(), &remote.Local{}, "shop", []string{"preview-pr-1", "preview-absent"})
	if usage["preview-pr-1"] <= 0 {
		t.Fatalf("expected positive usage for preview-pr-1, got %+v", usage)
	}
	if _, ok := usage["preview-absent"]; ok {
		t.Fatalf("unexpected usage for absent preview: %+v", usage)
	}
}
