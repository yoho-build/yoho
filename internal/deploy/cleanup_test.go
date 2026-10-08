package deploy

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/remote"
)

func TestImageListAndRemoveScripts(t *testing.T) {
	list := imageListScript("qa-notes")
	for _, want := range []string{
		"docker image ls -a --no-trunc",
		"--filter 'label=yoho.app=qa-notes'",
		"'{{.ID}}|{{.Repository}}|{{.Tag}}|{{.Containers}}'",
	} {
		if !strings.Contains(list, want) {
			t.Errorf("list script missing %q: %s", want, list)
		}
	}
	rm := imageRmScript("sha256:aaa")
	if rm != "docker image rm 'sha256:aaa' >/dev/null 2>&1" {
		t.Fatalf("rm script %s", rm)
	}
	if strings.Contains(list+"\n"+rm, "--force") || strings.Contains(list+"\n"+rm, " -f") {
		t.Fatalf("removal must not be forced:\n%s\n%s", list, rm)
	}
}

func TestParseImageRows(t *testing.T) {
	rows, ok := parseImageRows("sha256:aaa|yoho/shop-web|v1|0\nsha256:bbb|<none>|<none>|1\n")
	if !ok || len(rows) != 2 || rows[0].ID != "sha256:aaa" || rows[1].Repo != "<none>" || rows[1].Containers != "1" {
		t.Fatalf("rows %+v ok %v", rows, ok)
	}
	if _, ok := parseImageRows("not a row\n"); ok {
		t.Fatal("unparsed line must refuse the list")
	}
	if rows, ok = parseImageRows(""); !ok || rows != nil {
		t.Fatalf("empty %+v %v", rows, ok)
	}
}

func TestSelectImageRemovalsContainerd(t *testing.T) {
	rows := []imageRow{
		// Running release, listed twice (containerd duplicate) and once under the docker.io name.
		{ID: "sha256:keep", Repo: "yoho/shop-web", Tag: "v2", Containers: "1"},
		{ID: "sha256:keep", Repo: "docker.io/yoho/shop-web", Tag: "v2", Containers: "1"},
		// Old tag, duplicate rows. Ref removal would fail if the containerd name is unqualified; ID removal is once.
		{ID: "sha256:old", Repo: "yoho/shop-web", Tag: "v1", Containers: "0"},
		{ID: "sha256:old", Repo: "yoho/shop-web", Tag: "v1", Containers: "0"},
		// Retained on another Destination. Listed with the registry prefix.
		{ID: "sha256:other", Repo: "docker.io/yoho/shop-web", Tag: "v9", Containers: "0"},
		// Dangling platform child.
		{ID: "sha256:child", Repo: "<none>", Tag: "<none>", Containers: "0"},
		// Untagged but a container still uses it.
		{ID: "sha256:busy", Repo: "<none>", Tag: "<none>", Containers: "2"},
		// Blank-ID duplicate of a name that also has a real ID (removed with that ID).
		{ID: "", Repo: "yoho/shop-web", Tag: "v1", Containers: "0"},
		// Blank-ID row whose only handle is the ref.
		{ID: "<none>", Repo: "yoho/shop-web", Tag: "ghost", Containers: "0"},
		// Blank-ID row of a kept ref: must not be removed by name.
		{ID: "", Repo: "yoho/shop-web", Tag: "v2", Containers: "0"},
		// Pinned by digest in `docker ps`, even if the tag is gone.
		{ID: "sha256:pinned", Repo: "yoho/shop-web", Tag: "gone", Containers: "0"},
	}
	keep := map[string]bool{
		"yoho/shop-web:v2": true, // release record, no registry prefix
		"yoho/shop-web:v9": true,
		"yoho/shop-web:v1": false,
		"sha256:pinned":    true, // container image id
	}
	// A Swarm task image includes the digest; the tag still matches the Release.
	keep[canonicalImageRef("yoho/shop-web:v2@sha256:keep")] = true

	got := selectImageRemovals(rows, keep)
	want := []imageRemoval{
		{Target: "sha256:child", Name: "sha256:child"},
		{Target: "yoho/shop-web:ghost", Name: "yoho/shop-web:ghost"},
		{Target: "sha256:old", Name: "yoho/shop-web:v1"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] got %+v want %+v", i, got[i], want[i])
		}
	}
}

// scriptHost records every command. PruneImages only needs Output and Run.
type scriptHost struct {
	scripts []string
	fn      func(script string) (string, error)
}

func (h *scriptHost) Name() string { return "worker" }

func (h *scriptHost) Run(ctx context.Context, c remote.Cmd) error {
	_, err := h.Output(ctx, c)
	return err
}

func (h *scriptHost) Output(ctx context.Context, c remote.Cmd) (string, error) {
	h.scripts = append(h.scripts, c.Script)
	if h.fn != nil {
		return h.fn(c.Script)
	}
	return "", nil
}

func (h *scriptHost) WriteFile(context.Context, string, []byte, os.FileMode, bool) error {
	return nil
}
func (h *scriptHost) ReadFile(context.Context, string, bool) ([]byte, error) {
	return nil, os.ErrNotExist
}
func (h *scriptHost) Close() error { return nil }

func (h *scriptHost) all() string { return strings.Join(h.scripts, "\n---\n") }

func TestPruneImagesPrecomputedKeep(t *testing.T) {
	rows := strings.Join([]string{
		"sha256:stale|yoho/shop-web|old|0",
		"sha256:used|yoho/shop-web|live|0",
		"sha256:kept|yoho/shop-web|v1|0",
	}, "\n")
	h := &scriptHost{fn: func(script string) (string, error) {
		switch {
		case strings.Contains(script, "docker image ls"):
			return rows, nil
		case strings.Contains(script, "docker ps -a"):
			return "yoho/shop-web:live\n", nil
		default:
			return "", nil
		}
	}}
	keep := map[string]bool{"docker.io/yoho/shop-web:v1": true}
	var logs []string
	PruneImages(context.Background(), h, "shop", func(format string, a ...any) {
		logs = append(logs, fmt.Sprintf(format, a...))
	}, PruneOptions{Keep: keep})

	all := h.all()
	if strings.Contains(all, "release.json") {
		t.Fatalf("precomputed keep read Release records:\n%s", all)
	}
	if !strings.Contains(all, "docker image rm 'sha256:stale'") {
		t.Fatalf("unused image not removed:\n%s", all)
	}
	for _, id := range []string{"sha256:used", "sha256:kept"} {
		if strings.Contains(all, "docker image rm '"+id+"'") {
			t.Errorf("removed %s:\n%s", id, all)
		}
	}
	if strings.Contains(all, "--force") || strings.Contains(all, " -f") {
		t.Fatalf("removal was forced:\n%s", all)
	}
	if _, leaked := keep["yoho/shop-web:live"]; leaked {
		t.Fatal("container ref leaked into the caller's keep-set")
	}
	if !strings.Contains(strings.Join(logs, "\n"), "removed 1 old image(s): yoho/shop-web:old") {
		t.Fatalf("logs %q", logs)
	}

	// A second Server reuses the same map. Its container set must not inherit
	// the first Server's, and the retained ref must still be kept.
	other := &scriptHost{fn: func(script string) (string, error) {
		switch {
		case strings.Contains(script, "docker image ls"):
			return "sha256:other|yoho/shop-web|live|0\nsha256:kept|yoho/shop-web|v1|0\n", nil
		case strings.Contains(script, "docker ps -a"):
			return "", nil
		default:
			return "", nil
		}
	}}
	PruneImages(context.Background(), other, "shop", nil, PruneOptions{Keep: keep})
	if !strings.Contains(other.all(), "docker image rm 'sha256:other'") {
		t.Fatalf("second server kept an image only the first server's container used:\n%s", other.all())
	}
	if strings.Contains(other.all(), "docker image rm 'sha256:kept'") {
		t.Fatalf("second server removed a retained release image:\n%s", other.all())
	}
}

func TestPruneImagesNilKeepReadsReleases(t *testing.T) {
	h := &scriptHost{fn: func(script string) (string, error) {
		switch {
		case strings.Contains(script, "docker image ls"):
			return "sha256:stale|yoho/shop-web|old|0\nsha256:kept|yoho/shop-web|v1|0\n", nil
		case strings.Contains(script, "release.json"):
			return `{"images":{"web":"docker.io/yoho/shop-web:v1"}}`, nil
		case strings.Contains(script, "docker ps -a"):
			return "", nil
		default:
			return "", nil
		}
	}}
	PruneImages(context.Background(), h, "shop", nil, PruneOptions{})
	all := h.all()
	if !strings.Contains(all, "release.json") {
		t.Fatal("nil Keep did not read Release records")
	}
	if !strings.Contains(all, "docker image rm 'sha256:stale'") || strings.Contains(all, "docker image rm 'sha256:kept'") {
		t.Fatalf("compose keep set:\n%s", all)
	}
}

func TestCanonicalImageRef(t *testing.T) {
	if got := canonicalImageRef(" docker.io/library/nginx:1@sha256:abc "); got != "nginx:1" {
		t.Fatalf("%q", got)
	}
	if got := canonicalImageRef("docker.io/yoho/shop-web:v1"); got != "yoho/shop-web:v1" {
		t.Fatalf("%q", got)
	}
}
