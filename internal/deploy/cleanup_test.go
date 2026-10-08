package deploy

import (
	"strings"
	"testing"
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

func TestCanonicalImageRef(t *testing.T) {
	if got := canonicalImageRef(" docker.io/library/nginx:1@sha256:abc "); got != "nginx:1" {
		t.Fatalf("%q", got)
	}
	if got := canonicalImageRef("docker.io/yoho/shop-web:v1"); got != "yoho/shop-web:v1" {
		t.Fatalf("%q", got)
	}
}
