package deploy

import (
	"context"
	"io"
	"path"
	"slices"
	"strings"

	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// heldRef names the extra tag that keeps an image alive while a deploy moves
// the Version tag. On the containerd image store, re-pointing a tag deletes
// the image it named (no dangling record is left), so the recorded image
// would be unrecoverable by ID.
func heldRef(ref string) string { return ref + "-held" }

// HoldVersionImages gives the images the Version's deployed Release records
// a second tag, before Build and Ship overwrite the Version tags. It returns
// the function that drops those extra tags again; call it once the deploy has
// either been recorded or restored (RestoreVersionImages). Best effort: with
// no deployed record, or a tag that no longer names the recorded image,
// there is nothing to hold.
func HoldVersionImages(ctx context.Context, h remote.Host, out io.Writer, server, app, dest, version string) (drop func()) {
	r := &runner{h: h, out: out, server: server, app: app, dest: dest}
	prev, err := readRelease(ctx, h, path.Join(release.Dir(app, dest, version), "release.json"))
	if err != nil || prev == nil || prev.Status != "deployed" || len(prev.ImageIDs) == 0 {
		return func() {}
	}
	var held []string
	for _, svc := range sortedKeys(prev.ImageIDs) {
		ref, id := prev.Images[svc], prev.ImageIDs[svc]
		if ref == "" || strings.Contains(ref, "@") || (prev.Built != nil && !slices.Contains(prev.Built, svc)) || !r.yohoImage(ref, svc, version) {
			continue
		}
		script := "cur=$(docker image inspect -f '{{.Id}}' " + remote.Quote(ref) + " 2>/dev/null || true)\n" +
			"[ \"$cur\" != " + remote.Quote(id) + " ] || docker tag " + remote.QuoteArgs(ref, heldRef(ref)) + " >/dev/null"
		if err := h.Run(ctx, remote.Cmd{Script: script}); err != nil {
			r.logf("warning: could not hold image %s for the deploy: %v", ref, err)
			continue
		}
		held = append(held, heldRef(ref))
	}
	return func() {
		ctx := context.WithoutCancel(ctx)
		for _, ref := range held {
			_ = h.Run(ctx, remote.Cmd{Script: "docker image rm " + remote.Quote(ref) + " >/dev/null 2>&1 || true"})
		}
	}
}
