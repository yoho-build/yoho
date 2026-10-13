package proxy

import (
	"encoding/csv"
	"fmt"
	"slices"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/yoho-build/yoho/internal/config"
)

// RouteConflictError describes a hostname/path reservation, not ownership.
// Even an App's own obsolete route reserves its hosts until explicitly removed.
type RouteConflictError struct {
	Desired, Existing, Host, Path string
	// List displays both a catch-all (empty host) and a literal "*" as "*".
	DisplayAmbiguous bool
}

func (e *RouteConflictError) Error() string {
	host := e.Host
	if host == "" {
		host = "<catch-all>"
	}
	if e.DisplayAmbiguous {
		return fmt.Sprintf("Proxy route %s cannot verify hostname %q at path %q: existing route %s lists '*' (kamal-proxy displays catch-all and literal '*' identically; either may reserve this hostname). Nothing was changed", e.Desired, host, e.Path, e.Existing)
	}
	return fmt.Sprintf("Proxy route %s cannot reserve hostname %q at path %q: already reserved by different route %s (nothing was changed)", e.Desired, host, e.Path, e.Existing)
}

// RouteConflictHint is deliberately not an automatic cutover: deleting even an
// owned old route stops traffic until its replacement is healthy.
const RouteConflictHint = "keep the existing Service name, or phase the rename using a different hostname first; to reuse the hostname, verify ownership and manually remove only your obsolete route on every affected Server with `docker exec yoho-proxy kamal-proxy remove <verified-owned-old-route>` during a maintenance window (traffic is interrupted until the replacement is healthy); never remove another App's route"

// DesiredRoute models the actual --host StringSlice flags in kamal-proxy
// v0.10.2: each flag is CSV, and only an empty resulting list is normalized to
// the catch-all host. Hosts are NOT lowercased, trimmed, or stripped of ports.
func DesiredRoute(name string, hosts []string) (Route, error) {
	r := Route{Service: name, PathPrefixes: []string{"/"}}
	for _, flag := range hosts {
		if flag == "" { // pflag.readAsCSV special-cases an empty flag
			continue
		}
		hs, err := csv.NewReader(strings.NewReader(flag)).Read()
		if err != nil {
			return Route{}, fmt.Errorf("Proxy route %s: invalid --host value: %w", name, err)
		}
		r.Hosts = append(r.Hosts, hs...)
	}
	if len(r.Hosts) == 0 {
		r.Hosts = []string{""}
	}
	return r, nil
}

// ProjectRoutes collects only routes the desired Release will actively deploy.
// Defaults/explicit zero use compose-go's GetScale, like both compilers; Swarm
// global Services route even though their retained replica count is zero.
func ProjectRoutes(app, dest string, p *types.Project, ext map[string]config.ServiceExt, runtime string) ([]Route, error) {
	var routes []Route
	names := p.ServiceNames()
	slices.Sort(names)
	for _, name := range names {
		s := p.Services[name]
		pr := ext[name].Proxy
		replicas := s.GetScale()
		mode := "replicated"
		if runtime == "swarm" {
			if s.Deploy != nil && s.Deploy.Mode != "" {
				mode = s.Deploy.Mode
			}
			if replicas < 0 {
				replicas = 1
			}
		}
		if pr == nil || (mode != "global" && (mode != "replicated" || replicas <= 0)) {
			continue
		}
		rt, err := DesiredRoute(app+"-"+dest+"-"+name, pr.Hosts)
		if err != nil {
			return nil, err
		}
		routes = append(routes, rt)
	}
	return routes, CheckRouteConflicts(routes, nil)
}

type routeBinding struct{ host, path string }

func routeBindings(rt Route) []routeBinding {
	hs, ps := rt.Hosts, rt.PathPrefixes
	if len(hs) == 0 {
		hs = []string{""}
	}
	if len(ps) == 0 {
		ps = []string{"/"}
	}
	var keys []routeBinding
	for _, host := range hs {
		for _, prefix := range ps {
			keys = append(keys, routeBinding{host, "/" + strings.Trim(prefix, "/")})
		}
	}
	return keys
}

// CheckRouteConflicts mirrors ServiceMap.CheckAvailability and Normalize*
// from basecamp/kamal-proxy v0.10.2, commit 3b4ad579bcf5db316a86d27bd896c1e30874f192
// (internal/server/service_map.go). Only equal literal host + equal normalized
// path-prefix conflict. Catch-all/specific and wildcard/specific coexist;
// request host port stripping is NOT reservation normalization. Yoho deploys
// root-path routes, but existing manually managed path routes remain legitimate.
// Desired hosts must come from DesiredRoute; existing hosts from List are
// already decoded, with the lossy catch-all display handled explicitly below.
// Same-route updates still need a separate ownership check by the caller.
func CheckRouteConflicts(desired, existing []Route) error {
	reserved := map[routeBinding]string{}
	for _, rt := range desired {
		for _, key := range routeBindings(rt) {
			if old, ok := reserved[key]; ok && old != rt.Service {
				return &RouteConflictError{Desired: rt.Service, Existing: old, Host: key.host, Path: key.path}
			}
			reserved[key] = rt.Service
		}
	}
	for _, rt := range existing {
		for _, key := range routeBindings(rt) {
			// Service.Describe (same pinned commit, service.go) replaces
			// empty hosts with "*" in list output. Keep the displayed literal
			// binding AND its possible catch-all binding: losing either could
			// permit a late collision. This ambiguity affects only live List
			// results, never the known raw hosts of desired Releases.
			keys := []routeBinding{key}
			if key.host == "*" {
				keys = append(keys, routeBinding{"", key.path})
			}
			for _, possible := range keys {
				if want, ok := reserved[possible]; ok && want != rt.Service {
					return &RouteConflictError{Desired: want, Existing: rt.Service, Host: possible.host, Path: possible.path, DisplayAmbiguous: key.host == "*"}
				}
			}
		}
	}
	return nil
}
