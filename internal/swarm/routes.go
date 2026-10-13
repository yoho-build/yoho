package swarm

import (
	"context"
	"fmt"
	"strings"

	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/proxy"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

func planRoutes(app, dest string, p stackPlan) ([]proxy.Route, error) {
	var desired []proxy.Route
	for _, sp := range p.Services {
		if !sp.proxied() {
			continue
		}
		rt, err := proxy.DesiredRoute(deploy.RouteName(app, dest, sp.Name), sp.Proxy.Hosts)
		if err != nil {
			return nil, err
		}
		desired = append(desired, rt)
	}
	return desired, proxy.CheckRouteConflicts(desired, nil)
}

// preflightRoutes runs before files, secrets, jobs, Proxy boot or stack updates.
// All Destination Servers matter: a worker's Proxy may reserve a host even
// when the manager and the other workers have no conflict. It never removes
// routes, including old routes belonging to this App.
func preflightRoutes(ctx context.Context, d *plan.Deploy, desired []proxy.Route) error {
	if err := proxy.CheckRouteConflicts(desired, nil); err != nil {
		return err
	}
	if len(desired) == 0 {
		return nil
	}
	mgr := d.Servers[0].Host
	// Names join App and Destination with '-'. Preserve labeled ownership
	// rather than treating an identical stack/route name as proof of ownership.
	owners, err := mgr.Output(ctx, remote.Cmd{Script: "set -eu\nids=$(docker service ls --filter " + remote.Quote("label="+stackLabel+"="+release.ProjectName(d.App, d.Destination)) + " --format '{{.ID}}')\n" +
		"[ -z \"$ids\" ] || docker service inspect -f '{{index .Spec.Labels \"" + deploy.LabelApp + "\"}}|{{index .Spec.Labels \"" + deploy.LabelDestination + "\"}}' $ids"})
	if err != nil {
		return fmt.Errorf("verify Swarm App ownership on Server %s: %w", d.Servers[0].Name, err)
	}
	if err := checkRouteOwners(owners, d, release.ProjectName(d.App, d.Destination)); err != nil {
		return err
	}
	for _, s := range d.Servers {
		if err := ctx.Err(); err != nil {
			return err
		}
		routes, err := proxy.List(ctx, s.Host)
		if err != nil {
			return fmt.Errorf("verify Proxy routes on Server %s before changing the App: %w", s.Name, err)
		}
		for _, want := range desired {
			for _, rt := range routes {
				if rt.Service != want.Service {
					continue
				}
				if err := deploy.CheckRouteOwnership(ctx, s.Host, rt, d.App, d.Destination); err != nil {
					return err
				}
				// VIP targets are Swarm Service names; inspect on the manager.
				// Missing/unlabeled legacy targets do not assert ownership.
				var names []string
				for _, target := range strings.Split(rt.Target, ",") {
					if name, _, _ := strings.Cut(strings.TrimSpace(target), ":"); name != "" {
						// strict_drain uses Docker's tasks.<Service> DNS alias,
						// not a container or Service name accepted by inspect.
						names = append(names, strings.TrimPrefix(name, "tasks."))
					}
				}
				if len(names) > 0 {
					// Discover exact Service names first: a genuinely absent
					// legacy/manual target claims no ownership, but a failed
					// query/inspect must not be mistaken for absence.
					script := "set -eu\nknown=$(docker service ls --format '{{.Name}}')\nfor want in " + remote.QuoteArgs(names...) + "; do\n" +
						"  for service in $known; do\n    if [ \"$service\" = \"$want\" ]; then\n" +
						"      docker service inspect -f '{{index .Spec.Labels \"" + deploy.LabelApp + "\"}}|{{index .Spec.Labels \"" + deploy.LabelDestination + "\"}}' \"$service\"\n" +
						"    fi\n  done\ndone"
					out, err := mgr.Output(ctx, remote.Cmd{Script: script})
					if err != nil {
						return fmt.Errorf("verify ownership of Proxy route %s on Server %s: %w", rt.Service, s.Name, err)
					}
					if err := checkRouteOwners(out, d, rt.Service); err != nil {
						return err
					}
				}
			}
		}
		if err := proxy.CheckRouteConflicts(desired, routes); err != nil {
			return fmt.Errorf("Server %s: %w", s.Name, err)
		}
	}
	return nil
}

func checkRouteOwners(out string, d *plan.Deploy, resource string) error {
	for _, row := range lines(out) {
		app, dest, _ := strings.Cut(row, "|")
		if app != "" && app != "<no value>" && (app != d.App || dest != d.Destination) {
			return &release.OwnershipError{Msg: fmt.Sprintf("%s already belongs to App %s Destination %s; App %s Destination %s would replace it. Rename the App or Destination", resource, app, dest, d.App, d.Destination)}
		}
	}
	return nil
}
