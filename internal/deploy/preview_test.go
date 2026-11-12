package deploy

import (
	"context"
	"encoding/json"
	"fmt"
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
	if got, err := RemovePreview(ctx, h, p.App, p.Destination, false); got != nil || err != nil {
		t.Fatalf("absent: %+v %v", got, err)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("absent preview directory leaked: %v", err)
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

func TestPreviewRefusesUnmarkedContentsAndSymlink(t *testing.T) {
	for _, kind := range []string{"generated", "hmac.key", "source", "symlink", "dangling-symlink"} {
		t.Run(kind, func(t *testing.T) {
			withRoot(t)
			ctx := context.Background()
			p := PreviewRecord{App: "shop", Name: "pr-1", Destination: "preview-pr-1", Project: "yoho-shop-preview-pr-1"}
			dir := release.AppDir(p.App, p.Destination)
			target := t.TempDir()
			if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(kind, "symlink") {
				if kind == "dangling-symlink" {
					target = filepath.Join(target, "absent")
				} else {
					b, _ := json.Marshal(p)
					if err := os.WriteFile(filepath.Join(target, "preview.json"), b, 0600); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Symlink(target, dir); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, kind), []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			h := &dockerHost{local: &remote.Local{}}
			if err := MarkPreview(ctx, h, p); err == nil {
				t.Fatal("adopted unmarked or symlinked directory")
			}
			if strings.Contains(kind, "symlink") {
				if _, err := ReadPreview(ctx, h, p.App, p.Destination); err == nil {
					t.Fatal("read through symlink")
				}
			}
			if _, err := RemovePreview(ctx, h, p.App, p.Destination, false); err == nil {
				t.Fatal("removed unmarked or symlinked directory")
			}
			if _, err := os.Lstat(dir); err != nil {
				t.Fatal("original directory removed", err)
			}
		})
	}
}

func TestPreviewMissingMarkerCleansLockOnlyDirectory(t *testing.T) {
	withRoot(t)
	dir := release.AppDir("shop", "preview-pr-1")
	if err := os.MkdirAll(filepath.Join(dir, "lock"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lock", "info.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".yoho-app.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := RemovePreview(context.Background(), &dockerHost{local: &remote.Local{}}, "shop", "preview-pr-1", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("lock-only directory leaked: %v", err)
	}
}

func TestPreviewMarkerRetainsEarlierServices(t *testing.T) {
	withRoot(t)
	ctx := context.Background()
	p := PreviewRecord{App: "shop", Name: "pr-1", Destination: "preview-pr-1", Project: "yoho-shop-preview-pr-1", Services: []string{"old-web"}}
	dir := release.AppDir(p.App, p.Destination)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	h := &dockerHost{local: &remote.Local{}}
	if err := MarkPreview(ctx, h, p); err != nil {
		t.Fatal(err)
	}
	p.Services = []string{"new-web"}
	if err := MarkPreview(ctx, h, p); err != nil {
		t.Fatal(err)
	}
	got, err := ReadPreview(ctx, h, p.App, p.Destination)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Services, ",") != "new-web,old-web" {
		t.Fatalf("lost failed deploy's Services: %v", got.Services)
	}
}

func TestPreviewDownPrunesImagesButKeepsOtherReleases(t *testing.T) {
	withRoot(t)
	ctx := context.Background()
	p := PreviewRecord{App: "shop", Name: "pr-1", Destination: "preview-pr-1", Project: "yoho-shop-preview-pr-1"}
	dir := release.AppDir(p.App, p.Destination)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	other := release.Dir("shop", "production", "v1")
	if err := os.MkdirAll(other, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "release.json"), []byte(`{"images":{"web":"yoho/shop-production-web:v1"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	h := &dockerHost{local: &remote.Local{}, respond: func(s string) (string, error) {
		if strings.Contains(s, "docker image ls") {
			return "sha256:111|yoho/shop-preview-pr-1-web|v1|0\nsha256:222|registry.test/custom/shop-preview-pr-1-worker|v1|0\nsha256:333|yoho/shop-production-web|v1|0", nil
		}
		return "", nil
	}}
	if err := MarkPreview(ctx, h, p); err != nil {
		t.Fatal(err)
	}
	if _, err := RemovePreview(ctx, h, p.App, p.Destination, false); err != nil {
		t.Fatal(err)
	}
	scripts := strings.Join(h.scripts, "\n")
	if !strings.Contains(scripts, "docker image rm 'sha256:111'") || !strings.Contains(scripts, "docker image rm 'sha256:222'") {
		t.Fatalf("preview images leaked: %s", scripts)
	}
	if strings.Contains(scripts, "docker image rm 'sha256:333'") || strings.Contains(scripts, "--force") {
		t.Fatalf("pruned retained images: %s", scripts)
	}
}

func TestPreviewListValidatesDirectoryIdentity(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		t.Run(fmt.Sprint(mismatch), func(t *testing.T) {
			withRoot(t)
			p := PreviewRecord{App: "shop", Name: "pr-1", Destination: "preview-pr-1", Project: "yoho-shop-preview-pr-1"}
			dest := p.Destination
			if mismatch {
				dest = "preview-pr-2"
			}
			dir := release.AppDir(p.App, dest)
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(p)
			if err := os.WriteFile(filepath.Join(dir, "preview.json"), b, 0600); err != nil {
				t.Fatal(err)
			}
			got, err := ListPreviews(context.Background(), &remote.Local{}, p.App)
			if mismatch {
				if err == nil {
					t.Fatalf("listed marker claiming another directory: %+v", got)
				}
			} else if err != nil || len(got) != 1 || got[0].Name != "pr-1" {
				t.Fatalf("list: %+v %v", got, err)
			}
		})
	}
}
