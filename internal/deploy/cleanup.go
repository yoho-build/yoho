package deploy

import (
	"context"
	"encoding/json"
	"path"
	"slices"
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
//
// On Docker's containerd image store (the Docker 29 default) image IDs are
// manifest digests, and `docker image ls` can print the same name twice.
// Images loaded from an unqualified containerd name (apple/container archives
// before the name was rewritten to docker.io/...) are listed as repo:tag but
// `docker image rm repo:tag` returns "No such image". Removing by ID deletes
// that record and its platform child. Untagged rows (<none>) are the dangling
// platform children that listing does show; they are removed by ID too.
func pruneImages(ctx context.Context, r *runner, d *plan.Deploy) {
	PruneImages(ctx, r.h, d.App, r.logf)
}

// PruneImages is pruneImages for a Host. Swarm uses it on the manager; the
// compose runtime uses it on the Destination's Server. logf may be nil.
func PruneImages(ctx context.Context, h remote.Host, app string, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	logf("checking unused images")
	out, err := h.Output(ctx, remote.Cmd{Script: imageListScript(app)})
	if err != nil {
		logf("warning: list images: %v", err)
		return
	}
	rows, ok := parseImageRows(out)
	if !ok {
		logf("warning: not pruning images, unreadable image list")
		return
	}
	if len(rows) == 0 {
		return
	}
	keep := map[string]bool{}
	// Retained Releases of every Destination of this App on the Server.
	// Release directories are already pruned, so only retained records remain.
	appRoot := path.Join(release.Root, "apps", app)
	rels, err := h.Output(ctx, remote.Cmd{Script: "for f in " + remote.Quote(appRoot) + "/*/releases/*/release.json; do [ -f \"$f\" ] && cat \"$f\"; done; true"})
	if err != nil {
		logf("warning: list releases: %v", err)
		return
	}
	dec := json.NewDecoder(strings.NewReader(rels))
	for dec.More() {
		var rel release.Release
		if err := dec.Decode(&rel); err != nil {
			logf("warning: not pruning images, unreadable release record: %v", err)
			return
		}
		for _, im := range rel.Images {
			keep[canonicalImageRef(im)] = true
		}
	}
	used, err := h.Output(ctx, remote.Cmd{Script: "docker ps -a --format '{{.Image}}'"})
	if err != nil {
		logf("warning: list containers: %v", err)
		return
	}
	for _, im := range lines(used) {
		keep[canonicalImageRef(im)] = true
		keep[im] = true
	}
	var removed []string
	for _, rm := range selectImageRemovals(rows, keep) {
		if err := h.Run(ctx, remote.Cmd{Script: imageRmScript(rm.Target)}); err != nil {
			logf("warning: could not remove image %s (in use?)", rm.Name)
			continue
		}
		removed = append(removed, rm.Name)
	}
	if len(removed) > 0 {
		logf("removed %d old image(s): %s", len(removed), strings.Join(removed, ", "))
	}
}

// imageListScript lists this App's images, including dangling ones (-a).
// Columns: ID, repository, tag, container count. IDs are not truncated:
// containerd image IDs are the manifest digest, and a short ID can collide
// with a name lookup (`docker image rm <short>` -> "<short>:latest").
func imageListScript(app string) string {
	return "docker image ls -a --no-trunc --filter " + remote.Quote("label="+LabelApp+"="+app) +
		` --format '{{.ID}}|{{.Repository}}|{{.Tag}}|{{.Containers}}'`
}

// imageRmScript removes one image by ID or, for a blank-ID row, by ref.
// It is not --force / -f: docker refuses an image a container still uses.
func imageRmScript(target string) string {
	return "docker image rm " + remote.Quote(target) + " >/dev/null 2>&1"
}

// imageRow is one `docker image ls` row.
type imageRow struct {
	ID         string
	Repo       string
	Tag        string
	Containers string
}

// imageRemoval is one `docker image rm` invocation.
type imageRemoval struct {
	Target string // ID, or a ref when the row has no ID
	Name   string // human name for the log
}

// parseImageRows parses imageListScript output. ok is false when a line is
// not four pipe-separated fields: pruning nothing is safer than guessing.
func parseImageRows(out string) ([]imageRow, bool) {
	var rows []imageRow
	for _, l := range lines(out) {
		parts := strings.Split(l, "|")
		if len(parts) != 4 {
			return nil, false
		}
		rows = append(rows, imageRow{ID: parts[0], Repo: parts[1], Tag: parts[2], Containers: parts[3]})
	}
	return rows, true
}

// selectImageRemovals returns images to remove, one per ID. A row is kept
// when its ref or ID is in keep, or when docker reports a container using it.
// Duplicate rows (containerd lists some names twice) collapse to one removal.
// Untagged platform children (repository and tag `<none>`) are removed by ID.
// A row with an empty ID is removed by ref only when that ref is not kept
// and no listed ID already covers it.
func selectImageRemovals(rows []imageRow, keep map[string]bool) []imageRemoval {
	type group struct {
		id    string
		names []string
		hold  bool
	}
	order := []string{}
	byID := map[string]*group{}
	var blanks []imageRow
	for _, row := range rows {
		ref := row.ref()
		held := row.inUse() || (ref != "" && keep[ref]) || (row.ID != "" && (keep[row.ID] || keep[canonicalImageRef(row.ID)]))
		if row.ID == "" || row.ID == "<none>" {
			if !held {
				blanks = append(blanks, row)
			}
			continue
		}
		g := byID[row.ID]
		if g == nil {
			g = &group{id: row.ID}
			byID[row.ID] = g
			order = append(order, row.ID)
		}
		if held {
			g.hold = true
		}
		if ref != "" && !slices.Contains(g.names, ref) {
			g.names = append(g.names, ref)
		}
	}
	var out []imageRemoval
	seenRef := map[string]bool{}
	for _, id := range order {
		g := byID[id]
		if g.hold {
			for _, n := range g.names {
				seenRef[n] = true
			}
			continue
		}
		slices.Sort(g.names)
		name := id
		if len(g.names) > 0 {
			name = strings.Join(g.names, ", ")
			for _, n := range g.names {
				seenRef[n] = true
			}
		}
		out = append(out, imageRemoval{Target: id, Name: name})
	}
	for _, row := range blanks {
		ref := row.ref()
		if ref == "" || seenRef[ref] || keep[ref] {
			continue
		}
		seenRef[ref] = true
		out = append(out, imageRemoval{Target: ref, Name: ref})
	}
	slices.SortFunc(out, func(a, b imageRemoval) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func (r imageRow) ref() string {
	if r.Repo == "" || r.Repo == "<none>" || r.Tag == "" || r.Tag == "<none>" {
		return ""
	}
	return canonicalImageRef(r.Repo + ":" + r.Tag)
}

func (r imageRow) inUse() bool {
	switch r.Containers {
	case "", "0", "<none>":
		return false
	default:
		return true
	}
}

// canonicalImageRef strips the digest and Docker Hub's implicit prefixes so
// a Release record (yoho/app:v1) matches a containerd listing
// (docker.io/yoho/app:v1) and a Swarm container (yoho/app:v1@sha256:...).
func canonicalImageRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	ref = strings.TrimPrefix(ref, "docker.io/library/")
	ref = strings.TrimPrefix(ref, "docker.io/")
	return ref
}
