package deploy

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/yoho-build/yoho/internal/proxy"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// Compose project and Proxy route names join App, Destination and Service
// with '-' (yoho-<app>-<destination>, <app>-<destination>-<service>), so App
// foo-bar / Destination prod and App foo / Destination bar-prod share the
// project yoho-foo-bar-prod. Renaming would orphan every deployed project, so
// the names stay and ownership is checked instead: every compiled container
// carries yoho.app and yoho.destination labels.

// owner is the App and Destination labels of one container.
type owner struct{ app, dest string }

func (o owner) String() string { return "App " + o.app + " Destination " + o.dest }

// foreign reports whether o is labeled for another App Destination. Unlabeled
// containers (not compiled by Yoho) are not claimed by anyone.
func (o owner) foreign(app, dest string) bool {
	return o.app != "" && (o.app != app || o.dest != dest)
}

func parseOwners(out string) []owner {
	var owners []owner
	for _, l := range lines(out) {
		a, d, _ := strings.Cut(l, "|")
		owners = append(owners, owner{app: a, dest: d})
	}
	return owners
}

// checkOwnership refuses to deploy an App Destination into a compose project
// or Proxy route that another App Destination's containers use.
func checkOwnership(ctx context.Context, h remote.Host, app, dest string, plans []servicePlan) error {
	project := release.ProjectName(app, dest)
	out, err := h.Output(ctx, remote.Cmd{Script: "docker ps -a --filter " + remote.Quote("label=com.docker.compose.project="+project) +
		` --format '{{.Label "` + LabelApp + `"}}|{{.Label "` + LabelDestination + `"}}'`})
	if err != nil {
		return fmt.Errorf("list project containers: %w", err)
	}
	for _, o := range parseOwners(out) {
		if o.foreign(app, dest) {
			return &release.OwnershipError{Msg: fmt.Sprintf("compose project %s already runs containers of %s; App %s Destination %s would replace them. Rename the App or Destination", project, o, app, dest)}
		}
	}
	var desired []proxy.Route
	for _, sp := range plans {
		if sp.proxied() {
			rt, err := proxy.DesiredRoute(RouteName(app, dest, sp.Name), sp.Proxy.Hosts)
			if err != nil {
				return err
			}
			desired = append(desired, rt)
		}
	}
	if err := proxy.CheckRouteConflicts(desired, nil); err != nil {
		return err
	}
	if len(desired) == 0 {
		return nil
	}
	routes, err := proxy.List(ctx, h)
	if err != nil {
		return fmt.Errorf("verify Proxy routes on Server %s before changing the App: %w", h.Name(), err)
	}
	byName := map[string]proxy.Route{}
	for _, rt := range routes {
		byName[rt.Service] = rt
	}
	for _, sp := range plans {
		if !sp.proxied() {
			continue
		}
		rt, ok := byName[RouteName(app, dest, sp.Name)]
		if !ok {
			continue
		}
		if err := CheckRouteOwnership(ctx, h, rt, app, dest); err != nil {
			return err
		}
	}
	return proxy.CheckRouteConflicts(desired, routes)
}

// CheckRouteOwnership checks labels of existing same-name route targets.
// Discover exact container names/IDs first so genuinely vanished or external
// legacy targets remain unclaimed, but Docker/SSH/inspect failures fail closed.
// Cleanup retains routeOwner's older best-effort policy separately.
func CheckRouteOwnership(ctx context.Context, h remote.Host, rt proxy.Route, app, dest string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var names []string
	for _, target := range strings.Split(rt.Target, ",") {
		target = strings.TrimSpace(target)
		name, _, err := net.SplitHostPort(target)
		if err != nil {
			name, _, _ = strings.Cut(target, ":")
		}
		if name != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	script := "set -eu\nknown=$(docker ps -a --format '{{.ID}}|{{.Names}}')\nfor want in " + remote.QuoteArgs(names...) + "; do\n" +
		"  for entry in $known; do\n    id=${entry%%|*}\n    name=${entry#*|}\n" +
		"    if [ \"$want\" = \"$id\" ] || [ \"$want\" = \"$name\" ]; then\n" +
		"      docker inspect -f '{{index .Config.Labels \"" + LabelApp + "\"}}|{{index .Config.Labels \"" + LabelDestination + "\"}}' \"$id\"\n" +
		"    fi\n  done\ndone"
	out, err := h.Output(ctx, remote.Cmd{Script: script})
	if err != nil {
		return fmt.Errorf("verify ownership of Proxy route %s on Server %s: %w", rt.Service, h.Name(), err)
	}
	for _, o := range parseOwners(out) {
		if o.foreign(app, dest) {
			return &release.OwnershipError{Msg: fmt.Sprintf("proxy route %s already routes to containers of %s; App %s Destination %s would take it over. Rename the App, Destination or Service", rt.Service, o, app, dest)}
		}
	}
	return nil
}

// routeOwner returns the owner of a route's target containers and whether
// it is another App Destination. Targets that no longer exist say nothing.
func routeOwner(ctx context.Context, h remote.Host, rt proxy.Route, app, dest string) (owner, bool) {
	var names []string
	for _, t := range strings.Split(rt.Target, ",") {
		if n, _, _ := strings.Cut(strings.TrimSpace(t), ":"); n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return owner{}, false
	}
	out, err := h.Output(ctx, remote.Cmd{Script: "docker inspect -f '{{index .Config.Labels \"" + LabelApp + "\"}}|{{index .Config.Labels \"" + LabelDestination + "\"}}' " +
		remote.QuoteArgs(names...) + " 2>/dev/null || true"})
	if err != nil {
		return owner{}, false
	}
	for _, o := range parseOwners(out) {
		if o.foreign(app, dest) {
			return o, true
		}
	}
	return owner{}, false
}
