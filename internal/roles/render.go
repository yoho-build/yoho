// Package roles resolves opt-in multi-Server Compose placement.
package roles

import (
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strconv"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/plan"
)

// Order returns provider-before-consumer Server order. Cyclic Server placement
// cannot be rolled out safely as a sequence of complete Compose projects.
func Order(d config.Destination) ([]string, error) {
	edges := map[string]map[string]bool{}
	degree := map[string]int{}
	for _, h := range d.Servers {
		degree[h] = 0
		edges[h] = map[string]bool{}
	}
	instances := false
	for _, c := range d.Connections {
		providers, err := Providers(d, c)
		if err != nil {
			return nil, err
		}
		instances = instances || c.Instances
		for _, p := range providers {
			for _, svc := range c.Consumers {
				for _, h := range d.ServiceServers(svc) {
					if edges[p] == nil || edges[h] == nil {
						return nil, fmt.Errorf("connection placement references Server absent from Destination; retain all previous Servers before rollback")
					}
					if h != p && !edges[p][h] {
						edges[p][h] = true
						degree[h]++
					}
				}
			}
		}
	}
	var out []string
	for len(degree) > 0 {
		var ready []string
		for h, n := range degree {
			if n == 0 {
				ready = append(ready, h)
			}
		}
		slices.Sort(ready)
		if len(ready) == 0 {
			if instances {
				return nil, fmt.Errorf("Roles connections form a cycle across Servers; colocate dependencies or remove the cycle. Hint: instance Connections order Servers provider-first; a consumer on every provider Server forms a cycle. Colocate or split the Role")
			}
			return nil, fmt.Errorf("Roles connections form a cycle across Servers; colocate dependencies or remove the cycle")
		}
		for _, h := range ready {
			out = append(out, h)
			delete(degree, h)
			for dest := range edges[h] {
				degree[dest]--
			}
		}
	}
	return out, nil
}

// Render returns a copy containing only this Server's Services. Inputs are
// never modified, including nested maps used by subsequent Server renders.
func Render(d *plan.Deploy, dest config.Destination, server string, addresses map[string]string) (*plan.Deploy, error) {
	if dest.Roles == nil {
		return d, nil
	}
	out := *d
	out.StrictSecretRefs = true
	if out.Proxy.Tunnel != nil {
		zero := 0
		out.Proxy.HTTPPort = &zero
		out.Proxy.HTTPSPort = &zero
	}
	out.SecretRefs = maps.Clone(d.SecretRefs)
	p, err := d.Project.WithServicesTransform(func(_ string, s types.ServiceConfig) (types.ServiceConfig, error) { return s, nil })
	if err != nil {
		return nil, err
	}
	services := p.Services
	out.Project = p
	p.Services = types.Services{}
	out.Ext = map[string]config.ServiceExt{}
	out.ServiceSecrets = map[string]map[string]string{}
	out.Built = nil
	out.Servers = nil
	for _, h := range d.Servers {
		if h.Name == server {
			out.Servers = []plan.NamedHost{h}
		}
	}
	if len(out.Servers) != 1 {
		return nil, fmt.Errorf("missing Server %s", server)
	}
	for name, svc := range services {
		hosts := dest.ServiceServers(name)
		if !slices.Contains(hosts, server) {
			continue
		}
		ext := d.Ext[name]
		if ext.Proxy != nil {
			pr := *ext.Proxy
			if pr.SSL != nil {
				pr.TLS = *pr.SSL
			}
			ext.Proxy = &pr
		}

		if len(hosts) > 0 && hosts[0] != server {
			ext.ReleaseCommand = nil
		}
		out.Ext[name] = ext
		out.ServiceSecrets[name] = maps.Clone(d.ServiceSecrets[name])
		if slices.Contains(d.Built, name) {
			out.Built = append(out.Built, name)
		}
		for dep := range svc.DependsOn {
			if !slices.Contains(dest.ServiceServers(dep), server) {
				delete(svc.DependsOn, dep)
			}
		}
		for _, connName := range sortedKeys(dest.Connections) {
			c := dest.Connections[connName]
			providers, e := Providers(dest, c)
			if e != nil {
				return nil, e
			}
			addrOf := func(h string) (netip.Addr, error) {
				if addresses[h] == "" {
					return netip.Addr{}, fmt.Errorf("connection %s Server %s has no effective private address", connName, h)
				}
				ip, e := netip.ParseAddr(addresses[h])
				if e != nil {
					return netip.Addr{}, fmt.Errorf("connection %s requires a private IP address", connName)
				}
				return ip, nil
			}
			if name == c.Service {
				// Each provider Server publishes on its own address.
				ip, e := addrOf(server)
				if e != nil {
					return nil, e
				}
				addr := ip.String()
				for _, p := range svc.Ports {
					if p.Target != uint32(c.Port) || p.Protocol != "" && p.Protocol != "tcp" {
						continue
					}
					bound, e := netip.ParseAddr(p.HostIP)
					if e == nil && bound.IsLoopback() {
						continue
					}
					if e != nil || bound != ip || p.Published != strconv.Itoa(c.Port) {
						return nil, fmt.Errorf("connection %s may only publish %s:%d or an explicit loopback port", connName, addr, c.Port)
					}
				}
				// Replace same-target TCP mappings with the protected private binding.
				svc.Ports = slices.DeleteFunc(svc.Ports, func(p types.ServicePortConfig) bool {
					return p.Target == uint32(c.Port) && (p.Protocol == "" || p.Protocol == "tcp") && sameIP(p.HostIP, addr) && p.Published == strconv.Itoa(c.Port)
				})
				svc.Ports = append(svc.Ports, types.ServicePortConfig{HostIP: addr, Target: uint32(c.Port), Published: strconv.Itoa(c.Port), Protocol: "tcp"})
			}
			if !slices.Contains(c.Consumers, name) {
				continue
			}
			if svc.Environment == nil {
				svc.Environment = types.MappingWithEquals{}
			}
			port := strconv.Itoa(c.Port)
			svc.Environment[config.ConnectionEnvName(connName)+"_PORT"] = &port
			if svc.ExtraHosts == nil {
				svc.ExtraHosts = types.HostsList{}
			}
			// The bare and qualified Service names exist only for one provider Server.
			if len(providers) == 1 {
				ip, e := addrOf(providers[0])
				if e != nil {
					return nil, e
				}
				host := ip.String()
				if providers[0] == server {
					host = c.Service
				} else {
					svc.ExtraHosts[c.Service] = []string{host}
					svc.ExtraHosts[c.Service+"."+d.App+".yoho.internal"] = []string{host}
				}
				svc.Environment[config.ConnectionEnvName(connName)+"_HOST"] = &host
			}
			if !c.Instances {
				continue
			}
			// A same-Server instance is a network alias on the provider instead:
			// mangle PREROUTING would drop container traffic to the Server's
			// own private address (its source is a container address).
			for _, h := range providers {
				if h == server {
					continue
				}
				ip, e := addrOf(h)
				if e != nil {
					return nil, e
				}
				for _, alias := range InstanceNames(d.App, h, c.Service) {
					svc.ExtraHosts[alias] = []string{ip.String()}
				}
			}
		}
		p.Services[name] = svc
	}
	// Preserve the qualified endpoint when a provider and consumer colocate.
	for _, connName := range sortedKeys(dest.Connections) {
		c := dest.Connections[connName]
		provider, ok := p.Services[c.Service]
		if !ok {
			continue
		}
		local := false
		for _, consumer := range c.Consumers {
			_, exists := p.Services[consumer]
			local = local || exists
		}
		if !local {
			continue
		}
		var aliases []string
		if len(dest.ServiceServers(c.Service)) == 1 {
			aliases = append(aliases, c.Service+"."+d.App+".yoho.internal")
		}
		if c.Instances {
			aliases = append(aliases, InstanceNames(d.App, server, c.Service)...)
		}
		if len(aliases) == 0 {
			continue
		}
		if provider.Networks == nil {
			provider.Networks = map[string]*types.ServiceNetworkConfig{"default": {}}
		}
		for n, nc := range provider.Networks {
			if nc == nil {
				nc = &types.ServiceNetworkConfig{}
			}
			for _, alias := range aliases {
				if !slices.Contains(nc.Aliases, alias) {
					nc.Aliases = append(nc.Aliases, alias)
				}
			}
			provider.Networks[n] = nc
		}
		p.Services[c.Service] = provider
	}
	// Unused top-level resources must not create duplicate Stateful volumes
	// or ship another Server's Compose secret/config files.
	volumes, secrets, configs, networks := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, svc := range p.Services {
		for _, v := range svc.Volumes {
			if v.Type == types.VolumeTypeVolume {
				volumes[v.Source] = true
			}
		}
		for _, v := range svc.Secrets {
			secrets[v.Source] = true
		}
		for _, v := range svc.Configs {
			configs[v.Source] = true
		}
		if len(svc.Networks) == 0 && svc.NetworkMode == "" {
			networks["default"] = true
		}
		for n := range svc.Networks {
			networks[n] = true
		}
	}
	for n := range p.Volumes {
		if !volumes[n] {
			delete(p.Volumes, n)
		}
	}
	for n := range p.Secrets {
		if !secrets[n] {
			delete(p.Secrets, n)
		}
	}
	for n := range p.Configs {
		if !configs[n] {
			delete(p.Configs, n)
		}
	}
	for n := range p.Networks {
		if !networks[n] {
			delete(p.Networks, n)
		}
	}
	slices.Sort(out.Built)
	return &out, nil
}

// Providers returns the Servers hosting a Connection's provider: exactly one,
// or one or more for an instance Connection.
func Providers(d config.Destination, c config.Connection) ([]string, error) {
	p := d.ServiceServers(c.Service)
	if len(p) == 0 || len(p) > 1 && !c.Instances {
		return nil, fmt.Errorf("connection target %s must be singleton unless the connection sets instances: true", c.Service)
	}
	return p, nil
}

// InstanceNames are the two names of a provider's copy on one Server.
func InstanceNames(app, server, service string) []string {
	n := config.InstanceName(server, service)
	return []string{n, n + "." + app + ".yoho.internal"}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

func sameIP(a, b string) bool {
	x, e := netip.ParseAddr(a)
	if e != nil {
		return false
	}
	y, e := netip.ParseAddr(b)
	return e == nil && x == y
}
