// Package roles resolves opt-in multi-Server Compose placement.
package roles

import (
	"fmt"
	"maps"
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
	for _, c := range d.Connections {
		p := d.ServiceServers(c.Service)
		if len(p) != 1 {
			return nil, fmt.Errorf("connection target %s must be singleton", c.Service)
		}
		for _, svc := range c.Consumers {
			for _, h := range d.ServiceServers(svc) {
				if h != p[0] && !edges[p[0]][h] {
					edges[p[0]][h] = true
					degree[h]++
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
		if ext.Proxy != nil && len(hosts) > 1 && d.Proxy.Tunnel != nil && d.Proxy.Tunnel.TokenSecret != "" {
			proxy := *ext.Proxy
			proxy.TLS = false
			proxy.TLSStaging = false
			ext.Proxy = &proxy
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
			providers := dest.ServiceServers(c.Service)
			if len(providers) != 1 {
				return nil, fmt.Errorf("connection %s is not singleton", connName)
			}
			addr := addresses[providers[0]]
			if addr == "" {
				return nil, fmt.Errorf("connection %s Server %s has no effective private address", connName, providers[0])
			}
			if name == c.Service {
				// Replace same-target TCP mappings with the protected private binding.
				svc.Ports = slices.DeleteFunc(svc.Ports, func(p types.ServicePortConfig) bool {
					return p.Target == uint32(c.Port) && (p.Protocol == "" || p.Protocol == "tcp")
				})
				svc.Ports = append(svc.Ports, types.ServicePortConfig{HostIP: addr, Target: uint32(c.Port), Published: strconv.Itoa(c.Port), Protocol: "tcp"})
			}
			if providers[0] == server || !slices.Contains(c.Consumers, name) {
				continue
			}
			if svc.Environment == nil {
				svc.Environment = types.MappingWithEquals{}
			}
			port := strconv.Itoa(c.Port)
			host := addr
			svc.Environment[config.ConnectionEnvName(connName)+"_HOST"] = &host
			svc.Environment[config.ConnectionEnvName(connName)+"_PORT"] = &port
			if svc.ExtraHosts == nil {
				svc.ExtraHosts = types.HostsList{}
			}
			svc.ExtraHosts[c.Service] = []string{addr}
			svc.ExtraHosts[c.Service+"."+d.App+".yoho.internal"] = []string{addr}
		}
		p.Services[name] = svc
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

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}
