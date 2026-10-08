package deploy

import (
	"context"
	"fmt"
	"strings"

	"github.com/yoho-build/yoho/internal/remote"
)

// imageIDs returns Service -> local image ID for images, as present on the
// Server now. Images that cannot be inspected are left out (best effort:
// the Release still records its refs).
func imageIDs(ctx context.Context, h remote.Host, images map[string]string) map[string]string {
	out := map[string]string{}
	for _, svc := range sortedKeys(images) {
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

func imageIDScript(ref string) string {
	return "docker image inspect -f '{{.Id}}' " + remote.Quote(ref) + " 2>/dev/null || true"
}

// pinImages makes a rolled-back Release run the exact images it ran before.
// The compiled compose names images by tag, and tags move: the same Version
// built again for the same Destination, or a rebuild, re-points
// yoho/<app>-<destination>-<service>:<version> (Releases deployed before the
// Destination joined the tag record yoho/<app>-<service>:<version>). For
// Yoho-built images (tagged with the Version) the tag is pointed back at the
// recorded ID; rollback fails if that image is gone. On the containerd image
// store an overwritten multi-manifest (buildx) image survives only as a
// dangling record with another ID, so the re-tag can fail there; failing
// beats running another build. Other images (postgres:17) are not Yoho's to
// retag; a moved tag is only a warning.
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
		if !versioned(ref, version) {
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

func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		id = id[:12]
	}
	return id
}
