package deploy

import (
	"context"
	"encoding/json"
	"path"
	"strings"

	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/proxy"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// removeStaleRoutes deletes Proxy routes of this App Destination whose
// Service is no longer proxied. Failures are warnings: traffic already
// switched.
func (r *runner) removeStaleRoutes(ctx context.Context, plans []servicePlan) {
	r.logf("checking proxy routes")
	routes, err := proxy.List(ctx, r.h)
	if err != nil {
		r.logf("warning: could not list proxy routes: %v", err)
		return
	}
	keep := map[string]bool{}
	for _, sp := range plans {
		if sp.Proxy != nil {
			keep[RouteName(r.app, r.dest, sp.Name)] = true
		}
	}
	for _, rt := range StaleRoutes(routes, r.app, r.dest, keep) {
		r.logf("removing stale proxy route %s", rt.Service)
		if err := proxy.Remove(ctx, r.h, rt.Service); err != nil {
			r.logf("warning: %v", err)
		}
	}
}

// pruneImages removes local images built for this App (label yoho.app) that
// no retained Release of any of its Destinations references and no container
// uses. Never forced: docker refuses images in use. Errors are warnings.
func pruneImages(ctx context.Context, r *runner, d *plan.Deploy) {
	r.logf("checking unused images")
	out, err := r.h.Output(ctx, remote.Cmd{Script: "docker image ls --filter " + remote.Quote("label=yoho.app="+d.App) + " --format '{{.Repository}}:{{.Tag}}'"})
	if err != nil {
		r.logf("warning: list images: %v", err)
		return
	}
	var candidates []string
	for _, ref := range lines(out) {
		if !strings.HasSuffix(ref, ":<none>") {
			candidates = append(candidates, ref)
		}
	}
	if len(candidates) == 0 {
		return
	}
	keep := map[string]bool{}
	// Retained Releases of every Destination of this App on the Server.
	appRoot := path.Join(release.Root, "apps", d.App)
	rels, err := r.h.Output(ctx, remote.Cmd{Script: "for f in " + remote.Quote(appRoot) + "/*/releases/*/release.json; do [ -f \"$f\" ] && cat \"$f\"; done; true"})
	if err != nil {
		r.logf("warning: list releases: %v", err)
		return
	}
	dec := json.NewDecoder(strings.NewReader(rels))
	for dec.More() {
		var rel release.Release
		if err := dec.Decode(&rel); err != nil {
			r.logf("warning: not pruning images, unreadable release record: %v", err)
			return
		}
		for _, im := range rel.Images {
			keep[im] = true
		}
	}
	used, err := r.h.Output(ctx, remote.Cmd{Script: "docker ps -a --format '{{.Image}}'"})
	if err != nil {
		r.logf("warning: list containers: %v", err)
		return
	}
	for _, im := range lines(used) {
		keep[im] = true
	}
	var gone []string
	for _, ref := range candidates {
		if keep[ref] {
			continue
		}
		if err := r.h.Run(ctx, remote.Cmd{Script: "docker image rm " + remote.Quote(ref) + " >/dev/null 2>&1"}); err != nil {
			r.logf("warning: could not remove image %s (in use?)", ref)
			continue
		}
		gone = append(gone, ref)
	}
	if len(gone) > 0 {
		r.logf("removed %d old image(s): %s", len(gone), strings.Join(gone, ", "))
	}
}
