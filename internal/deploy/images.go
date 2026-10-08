package deploy

import (
	"context"
	"fmt"
	"strings"

	"github.com/yoho-build/yoho/internal/build"
	"github.com/yoho-build/yoho/internal/remote"
)

// imageIDs returns Service -> local image ID for images, as the deployed
// containers run them. Builds happen before the Destination lock, so a
// concurrent build of the same Version can re-point the tag between cutover
// and this read; the containers' .Image is what this Release actually runs.
// Only Services without a container (none created) fall back to the tag.
// IDs that cannot be read are left out (best effort: the Release still
// records its refs).
func imageIDs(ctx context.Context, h remote.Host, project string, images map[string]string) map[string]string {
	running := containerImageIDs(ctx, h, project)
	out := map[string]string{}
	for _, svc := range sortedKeys(images) {
		if id, ok := running[svc]; ok {
			out[svc] = id
			continue
		}
		id, err := h.Output(ctx, remote.Cmd{Script: imageIDScript(images[svc])})
		if id = strings.TrimSpace(id); err == nil && strings.HasPrefix(id, "sha256:") {
			out[svc] = id
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// containerImageIDs returns compose Service -> image ID of the project's
// containers: a running container's when there is one, else the newest.
func containerImageIDs(ctx context.Context, h remote.Host, project string) map[string]string {
	script := "ids=$(docker ps -aq --filter " + remote.Quote("label=com.docker.compose.project="+project) + ")\n" +
		`[ -z "$ids" ] || docker inspect -f '{{index .Config.Labels "com.docker.compose.service"}} {{.State.Running}} {{.Image}}' $ids`
	out, err := h.Output(ctx, remote.Cmd{Script: script})
	if err != nil {
		return nil
	}
	ids := map[string]string{}
	runningSeen := map[string]bool{}
	// docker ps lists newest first and inspect keeps that order.
	for line := range strings.Lines(out) {
		f := strings.Fields(line)
		if len(f) != 3 || !strings.HasPrefix(f[2], "sha256:") {
			continue
		}
		svc, running, id := f[0], f[1] == "true", f[2]
		if runningSeen[svc] {
			continue
		}
		if _, ok := ids[svc]; !ok || running {
			ids[svc] = id
			runningSeen[svc] = running
		}
	}
	return ids
}

func imageIDScript(ref string) string {
	return "docker image inspect -f '{{.Id}}' " + remote.Quote(ref) + " 2>/dev/null || true"
}

// pinImages makes a rolled-back Release run the exact images it ran before.
// The compiled compose names images by tag, and tags move: the same Version
// built again for the same Destination, or a rebuild, re-points
// yoho/<app>-<destination>-<service>:<version> (Releases deployed before the
// Destination joined the tag record yoho/<app>-<service>:<version>). For
// Yoho-built images (exactly those names, see yohoImage) the tag is pointed
// back at the recorded ID; rollback fails if that image is gone. On the containerd image
// store an overwritten multi-manifest (buildx) image survives only as a
// dangling record with another ID, so the re-tag can fail there; failing
// beats running another build. Other images (postgres:17, even deployed with
// --version 17) are not Yoho's to retag; a moved tag is only a warning.
func (r *runner) pinImages(ctx context.Context, images, ids map[string]string, version string) error {
	for _, svc := range sortedKeys(ids) {
		ref, id := images[svc], ids[svc]
		if ref == "" || strings.Contains(ref, "@") {
			continue // pinned by digest already
		}
		cur, err := r.h.Output(ctx, remote.Cmd{Script: imageIDScript(ref)})
		if err != nil {
			return fmt.Errorf("inspect image %s: %w", ref, err)
		}
		if strings.TrimSpace(cur) == id {
			continue
		}
		if !r.yohoImage(ref, svc, version) {
			r.logf("warning: %s now points at a different image than when %s was deployed; rolling back with the current one", ref, version)
			continue
		}
		r.logf("re-tagging %s to the image Release %s ran (%s)", ref, version, shortID(id))
		script := "set -e\ndocker image inspect " + remote.Quote(id) + " >/dev/null 2>&1 || { echo " + remote.Quote("image "+id+" is gone") + " >&2; exit 1; }\n" +
			"docker tag " + remote.QuoteArgs(id, ref)
		if err := r.h.Run(ctx, remote.Cmd{Script: script}); err != nil {
			return fmt.Errorf("release %s: cannot restore image %s for %s: the tag now names another build (the same Version deployed to another Destination, or a rebuild) and the original is gone; deploy that commit again instead: %w", version, ref, svc, err)
		}
	}
	return nil
}

// yohoImage reports whether ref is an image Yoho built for svc at version,
// whatever Registry or prefix it carried when the Release was deployed: the
// last path component is the per-Destination or legacy <app>-<service> name
// from build.ImageName and the tag is the Release Version. A tag alone is
// not enough: postgres:17 deployed as Version 17 is a shared third-party tag.
func (r *runner) yohoImage(ref, svc, version string) bool {
	// Strip a digest, then split the tag off the last component.
	if i := strings.Index(ref, "@"); i >= 0 {
		return false
	}
	slash := strings.LastIndex(ref, "/")
	colon := strings.LastIndex(ref, ":")
	if colon <= slash || ref[colon+1:] != strings.ToLower(version) {
		return false
	}
	repo := ref[slash+1 : colon]
	for _, dest := range []string{r.dest, ""} {
		want := build.ImageName(nil, r.app, dest, svc, version)
		want = want[strings.LastIndex(want, "/")+1 : strings.LastIndex(want, ":")]
		if repo == want {
			return true
		}
	}
	return false
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		id = id[:12]
	}
	return id
}
