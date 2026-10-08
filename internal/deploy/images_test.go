package deploy

import (
	"context"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/remote"
)

// builtWeb gives web the image name Yoho builds for it (build.ImageName).
func builtWeb(t *testing.T, d *plan.Deploy) *plan.Deploy {
	t.Helper()
	s := d.Project.Services["web"]
	s.Image = "yoho/shop-production-web:" + d.Version
	d.Project.Services["web"] = s
	d.Built = []string{"web"}
	return d
}

func TestYohoImageOnlyMatchesYohoNames(t *testing.T) {
	r := &runner{app: "Shop", dest: "production"}
	reg := &runner{app: "shop", dest: "production", registry: &config.Registry{Server: "ghcr.io/acme"}}
	for _, c := range []struct {
		r    *runner
		ref  string
		svc  string
		want bool
	}{
		{r, "yoho/shop-production-web:17", "web", true},
		{r, "yoho/shop-web:17", "web", true}, // before the Destination joined the tag
		{r, "postgres:17", "db", false},      // --version 17 must not move a shared tag
		{r, "library/postgres:17", "db", false},
		{r, "yoho/shop-production-web:16", "web", false},
		{r, "yoho/shop-production-db:17", "web", false},
		{r, "registry.old:5000/acme/shop-production-web:17", "web", true}, // Registry changed since the deploy
		{r, "shop-production-web:17", "web", true},
		{r, "evil/shop-production-api:17", "web", false},
		{r, "yoho/shop-production-web@sha256:abc", "web", false},
		{reg, "ghcr.io/acme/shop-production-web:17", "web", true},
		{reg, "ghcr.io/acme/shop-web:17", "web", true},
		{reg, "yoho/shop-production-web:17", "web", true},          // deployed before the registry
		{reg, "ghcr.io/other/shop-production-web:17", "web", true}, // any prefix: shape and tag decide
		{reg, "ghcr.io/other/shop-production-web:16", "web", false},
		{reg, "ghcr.io/acme/postgres:17", "db", false},
	} {
		if got := c.r.yohoImage(c.ref, c.svc, "17"); got != c.want {
			t.Errorf("yohoImage(%q, %q) = %v, want %v", c.ref, c.svc, got, c.want)
		}
	}
}

// A third-party image whose tag equals the Version is never re-tagged.
func TestPinImagesLeavesThirdPartyTag(t *testing.T) {
	h := &dockerHost{local: &remote.Local{}, respond: func(s string) (string, error) {
		if strings.Contains(s, "docker image inspect -f '{{.Id}}' 'postgres:17'") {
			return "sha256:moved", nil
		}
		return "", nil
	}}
	r := &runner{h: h, app: "shop", dest: "production", out: &strings.Builder{}}
	err := r.pinImages(context.Background(), map[string]string{"db": "postgres:17"}, map[string]string{"db": "sha256:pg1"}, nil, "17")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h.all(), "docker tag") {
		t.Fatalf("third-party tag moved:\n%s", h.all())
	}
}

// The Release records the image its containers run, not where the tag
// points now (a concurrent build may have moved it).
func TestImageIDsPreferContainers(t *testing.T) {
	h := &dockerHost{local: &remote.Local{}, respond: func(s string) (string, error) {
		switch {
		case strings.Contains(s, "label=com.docker.compose.project=yoho-shop-production"):
			return "web false sha256:old\n" + // older stopped container
				"web true sha256:running\n" +
				"migrate false sha256:oneshot\n" +
				"web true not-an-id\n", nil
		case strings.Contains(s, "'yoho/shop-production-web:v1'"):
			return "sha256:retagged", nil
		case strings.Contains(s, "'redis:7'"):
			return "sha256:redis", nil
		}
		return "", nil
	}}
	got := imageIDs(context.Background(), h, "yoho-shop-production", map[string]string{
		"web": "yoho/shop-production-web:v1", "migrate": "yoho/shop-production-migrate:v1", "cache": "redis:7",
	})
	want := map[string]string{"web": "sha256:running", "migrate": "sha256:oneshot", "cache": "sha256:redis"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q (all %v)", k, got[k], v, got)
		}
	}
	if strings.Contains(h.all(), "'yoho/shop-production-web:v1'") {
		t.Error("tag inspected although a container runs the Service")
	}
}

// A Service Yoho did not build is never re-tagged, even when its image is
// named like a built one (vendor/shop-web:v1 for App shop, Service web).
func TestPinImagesSkipsUnbuiltLookalike(t *testing.T) {
	h := &dockerHost{local: &remote.Local{}, respond: func(s string) (string, error) {
		if strings.Contains(s, "docker image inspect -f '{{.Id}}'") {
			return "sha256:moved", nil
		}
		return "", nil
	}}
	r := &runner{h: h, app: "shop", dest: "production", out: &strings.Builder{}}
	images, ids := map[string]string{"web": "vendor/shop-web:v1"}, map[string]string{"web": "sha256:old"}
	if err := r.pinImages(context.Background(), images, ids, []string{}, "v1"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h.all(), "docker tag") {
		t.Fatalf("third-party image retagged:\n%s", h.all())
	}
	// Built by Yoho: retagged. Unknown (older record): name shape decides.
	for _, built := range [][]string{{"web"}, nil} {
		h2 := &dockerHost{local: &remote.Local{}, respond: h.respond}
		r.h = h2
		if err := r.pinImages(context.Background(), images, ids, built, "v1"); err != nil {
			t.Fatal(err)
		}
		if built != nil && !strings.Contains(h2.all(), "docker tag") {
			t.Fatalf("built image not retagged: %v", built)
		}
	}
}
