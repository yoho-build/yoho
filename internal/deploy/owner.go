package deploy

import (
	"context"
	"fmt"
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
	routes, err := proxy.List(ctx, h)
	if err != nil {
		return nil // the deploy reports a Proxy that cannot be queried
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
		if o, foreign := routeOwner(ctx, h, rt, app, dest); foreign {
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
