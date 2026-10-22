package composefile

import (
	"regexp"
	"sort"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
)

var hostPortRe = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9_.-]*):\d+(?:[/?].*)?$`)

// endpointKeys are the environment key fragments that mark a bare value as a
// hostname (POSTGRES_DB=web is a database name, not a host).
var endpointKeys = []string{"HOST", "ADDR", "URL", "URI", "ENDPOINT", "SERVER", "DSN", "BROKER"}

func endpointKey(key string) bool {
	key = strings.ToUpper(key)
	for _, k := range endpointKeys {
		if strings.Contains(key, k) {
			return true
		}
	}
	return false
}

// hostTokens returns the lower-cased hostnames an environment value refers to:
// URL hosts, host:port pairs (comma or space separated lists included) and,
// for an endpoint-like key, a value that is a bare name. Names inside paths
// or longer words never match.
func hostTokens(key, v string) []string {
	v = strings.TrimSpace(v)
	var out []string
	for _, piece := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
		if i := strings.Index(piece, "://"); i >= 0 {
			auth := piece[i+3:]
			if j := strings.IndexAny(auth, "/?#"); j >= 0 {
				auth = auth[:j]
			}
			// The last '@' ends the userinfo, which may hold '@' in the password.
			auth = auth[strings.LastIndex(auth, "@")+1:]
			if j := strings.Index(auth, ":"); j >= 0 {
				auth = auth[:j]
			}
			if auth != "" {
				out = append(out, strings.ToLower(auth))
			}
			continue
		}
		if i := strings.LastIndex(piece, "="); i >= 0 {
			piece = piece[i+1:]
		}
		piece = piece[strings.LastIndex(piece, "@")+1:]
		if m := hostPortRe.FindStringSubmatch(piece); m != nil {
			out = append(out, strings.ToLower(m[1]))
		}
	}
	if endpointKey(key) && v != "" && !strings.ContainsAny(v, "/:@=, \t") {
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
			for _, tok := range hostTokens(key, *val) {
				for _, other := range p.ServiceNames() {
					if other == name || reported[other] || deps[other] || p.Services[other].NetworkMode != "" {
						continue
					}
					nets, ok := serviceHosts(p.Services[other])[tok]
					if !ok || !sharesNetwork(mine, nets) {
						continue
					}
					reported[other] = true
					// depends_on would close a cycle when other already needs name.
					out = append(out, dependsFinding(name, key, other, p.Services[other], swarm || dependsClosure(p, other)[name]))
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

func dependsFinding(svc, key, other string, o types.ServiceConfig, retry bool) Finding {
	f := Finding{Level: LevelWarning, Service: svc}
	if retry {
		f.Message = "environment " + key + " refers to Service " + other + "; depends_on cannot order them (Swarm ignores it, or it would close a cycle), so make the app retry until it is reachable"
		return f
	}
	cond := "service_started"
	if o.HealthCheck != nil && !o.HealthCheck.Disable {
		cond = "service_healthy"
	}
	f.Message = "environment " + key + " refers to Service " + other + ", which is not in depends_on; add depends_on: {" + other + ": {condition: " + cond + "}}"
	return f
}
