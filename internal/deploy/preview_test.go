package deploy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

func TestPreviewMarkerAndTeardown(t *testing.T) {
	withRoot(t)
	ctx := context.Background()
	h := &dockerHost{local: &remote.Local{}}
	p := PreviewRecord{App: "shop", Name: "pr-1", Destination: "preview-pr-1", Project: "yoho-shop-preview-pr-1", Hosts: []string{"pr-1.example.com"}, Services: []string{"web"}, Version: "v1"}
	dir := release.AppDir(p.App, p.Destination)
	if err := os.MkdirAll(filepath.Join(dir, "lock"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := MarkPreview(ctx, h, p); err != nil {
		t.Fatal(err)
	}
	p.Version = "v2"
	if err := MarkPreview(ctx, h, p); err != nil {
		t.Fatal(err)
	}
	got, err := ReadPreview(ctx, h, p.App, p.Destination)
	if err != nil || got.Version != "v2" {
		t.Fatalf("marker update: %+v %v", got, err)
	}
	other := release.AppDir(p.App, "production")
	if err := os.MkdirAll(other, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := RemovePreview(ctx, h, p.App, p.Destination, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("preview remains: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("other Destination removed: %v", err)
	}
	scripts := strings.Join(h.scripts, "\n")
	for _, want := range []string{"'label=com.docker.compose.project=yoho-shop-preview-pr-1'", "docker rm -fv $ids", "docker volume rm"} {
		if !strings.Contains(scripts, want) {
			t.Errorf("missing %s in %s", want, scripts)
		}
	}
	if strings.Contains(scripts, "production") || strings.Contains(scripts, "prune") {
		t.Fatal("teardown targets other resources")
	}
	// Simulate the lock directory created by the second down invocation.
	if err := os.MkdirAll(filepath.Join(dir, "lock"), 0700); err != nil {
		t.Fatal(err)
	}
	if got, err := RemovePreview(ctx, h, p.App, p.Destination, false); got != nil || err != nil {
		t.Fatalf("absent: %+v %v", got, err)
	}
}

func TestPreviewRefusesUnmarkedAndForgedDestinations(t *testing.T) {
	for _, kind := range []string{"releases", "containers", "bad-marker", "foreign-owner", "unmarked-volumes"} {
		t.Run(kind, func(t *testing.T) {
			withRoot(t)
			ctx := context.Background()
			p := PreviewRecord{App: "shop", Name: "pr-1", Destination: "preview-pr-1", Project: "yoho-shop-preview-pr-1"}
			dir := release.AppDir(p.App, p.Destination)
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			h := &dockerHost{local: &remote.Local{}, respond: func(s string) (string, error) {
				if strings.Contains(s, "docker ps -a --filter") {
					switch kind {
					case "containers":
						return "container\tc1\tshop|preview-pr-1", nil
					case "foreign-owner":
						return "container\tc1\tother|production", nil
					case "unmarked-volumes":
						return "volume\tdata", nil
					}
				}
				return "", nil
			}}
			if kind == "releases" {
				if err := os.Mkdir(filepath.Join(dir, "releases"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "bad-marker" || kind == "foreign-owner" {
				b, _ := json.Marshal(p)
				if kind == "bad-marker" {
					b = []byte(`{"app":"shop","name":"production","destination":"production"}`)
				}
				if err := os.WriteFile(filepath.Join(dir, "preview.json"), b, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := RemovePreview(ctx, h, p.App, p.Destination, false); err == nil {
				t.Fatal("unsafe teardown accepted")
			}
			if kind != "bad-marker" && kind != "foreign-owner" {
				if err := MarkPreview(ctx, h, p); err == nil {
					t.Fatal("adopted unmarked resources")
				}
			}
			for _, s := range h.scripts {
				if strings.Contains(s, "docker stop") || strings.Contains(s, "docker volume rm") {
					t.Fatal("destructive work before marker/ownership check")
				}
			}
		})
	}
}
