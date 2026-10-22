package composefile

import (
	"regexp"
	"sort"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
)

var (
	// scheme://[user[:pw]@]host
	urlHostRe = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://(?:[^/@\s]*@)?([^/:?#\s,]+)`)
	// host:port after start, whitespace, ',', '@' or '='; a leading '/' is a path.
	hostPortRe = regexp.MustCompile(`(?:^|[\s,@=])([A-Za-z0-9][A-Za-z0-9_.-]*):\d+(?:$|[\s,/?])`)
)

// hostTokens returns the lower-cased hostnames an environment value refers to:
// URL hosts, host:port pairs and a value that is a bare name. Names inside
// paths or longer words never match.
func hostTokens(v string) []string {
	v = strings.TrimSpace(v)
	var out []string
	for _, m := range urlHostRe.FindAllStringSubmatch(v, -1) {
		out = append(out, strings.ToLower(m[1]))
	}
	for _, m := range hostPortRe.FindAllStringSubmatch(v, -1) {
		out = append(out, strings.ToLower(m[1]))
	}
	if v != "" && !strings.ContainsAny(v, "/:@= \t,") {
		out = append(out, strings.ToLower(v))
	}
	return out
}

// serviceHosts maps each lower-cased hostname a Service answers to (name,
// container_name, network aliases) to the networks it is reachable on.
func serviceHosts(s types.ServiceConfig) map[string]map[string]bool {
	nets := serviceNetworks(s)
	hosts := map[string]map[string]bool{}
	add := func(h string, on map[string]bool) {
		h = strings.ToLower(h)
		if h == "" {
			return
		}
		if hosts[h] == nil {
			hosts[h] = map[string]bool{}
		}
		for n := range on {
			hosts[h][n] = true
		}
	}
	add(s.Name, nets)
	add(s.ContainerName, nets)
	for n, cfg := range s.Networks {
		if cfg == nil {
			continue
		}
		for _, a := range cfg.Aliases {
			add(a, map[string]bool{n: true})
		}
	}
	return hosts
}

func serviceNetworks(s types.ServiceConfig) map[string]bool {
	nets := map[string]bool{}
	for n := range s.Networks {
		nets[n] = true
	}
	if len(nets) == 0 {
		nets["default"] = true
	}
	return nets
}

// dependsClosure returns every Service name s depends on, directly or not.
func dependsClosure(p *types.Project, s string) map[string]bool {
	seen := map[string]bool{}
	var walk func(string)
	walk = func(n string) {
		for dep := range p.Services[n].DependsOn {
			if !seen[dep] {
				seen[dep] = true
				walk(dep)
			}
		}
	}
	walk(s)
	return seen
}

// undeclaredDependencies reports Services whose environment names another
// Service's hostname without a depends_on path to it. Without one, compose
// may start the Service before its dependency. Only environment is scanned
// (env_file values are already merged in); secret values never reach Check.
func undeclaredDependencies(p *types.Project, swarm bool) []Finding {
	var out []Finding
	for _, name := range p.ServiceNames() {
		svc := p.Services[name]
		if svc.NetworkMode != "" {
			continue
		}
		deps := dependsClosure(p, name)
		mine := serviceNetworks(svc)
		reported := map[string]bool{}
		keys := make([]string, 0, len(svc.Environment))
		for k := range svc.Environment {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, key := range keys {
			val := svc.Environment[key]
			if val == nil {
				continue
			}
			for _, tok := range hostTokens(*val) {
				for _, other := range p.ServiceNames() {
					if other == name || reported[other] || deps[other] {
						continue
					}
					nets, ok := serviceHosts(p.Services[other])[tok]
					if !ok || !sharesNetwork(mine, nets) {
						continue
					}
					reported[other] = true
					out = append(out, dependsFinding(name, key, other, p.Services[other], swarm))
				}
			}
		}
	}
	return out
}

func sharesNetwork(a, b map[string]bool) bool {
	for n := range a {
		if b[n] {
			return true
		}
	}
	return false
}

func dependsFinding(svc, key, other string, o types.ServiceConfig, swarm bool) Finding {
	f := Finding{Level: LevelWarning, Service: svc}
	if swarm {
		f.Message = "environment " + key + " refers to Service " + other + "; depends_on is ignored by stack deploy and Swarm has no start order, so make the app retry until it is reachable"
		return f
	}
	cond := "service_started"
	if o.HealthCheck != nil && !o.HealthCheck.Disable {
		cond = "service_healthy"
	}
	f.Message = "environment " + key + " refers to Service " + other + ", which is not in depends_on; add depends_on: {" + other + ": {condition: " + cond + "}}"
	return f
}
