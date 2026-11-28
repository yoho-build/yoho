package config

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// ServiceServers returns a stable placement; without Roles it preserves the
// Destination's original Server order.
func (d Destination) ServiceServers(service string) []string {
	if d.Roles == nil {
		return slices.Clone(d.Servers)
	}
	for _, r := range d.Roles {
		if slices.Contains(r.Services, service) {
			out := slices.Clone(r.Servers)
			slices.Sort(out)
			return out
		}
	}
	return nil
}

// ConnectionEnvName is the environment prefix reserved for a connection.
func ConnectionEnvName(name string) string {
	return "YOHO_CONNECTION_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}

func validateRoles(c *Config, name string, d Destination) []error {
	var errs []error
	add := func(f string, args ...any) {
		errs = append(errs, fmt.Errorf("destinations.%s: %s", name, fmt.Sprintf(f, args...)))
	}
	if d.Roles == nil {
		if len(d.Connections) > 0 {
			add("connections requires roles")
		}
		return errs
	}
	if d.Runtime == "swarm" || d.Access == "scoped" || d.Migrate != nil {
		add("roles requires operator access and Compose runtime without migrate")
	}
	if len(d.Roles) == 0 {
		add("roles must not be empty")
	}
	used := map[string]bool{}
	for _, server := range d.Servers {
		if used[server] {
			add("Destination repeats Server %s", server)
		}
		used[server] = true
	}
	seen := map[string]string{}
	for _, n := range sortedKeys(d.Roles) {
		r := d.Roles[n]
		if !appPattern.MatchString(n) {
			add("invalid Role name %q", n)
		}
		if len(r.Services) == 0 || len(r.Servers) == 0 {
			add("Role %s requires services and servers", n)
		}
		hosts := map[string]bool{}
		for _, h := range r.Servers {
			if !slices.Contains(d.Servers, h) {
				add("Role %s Server %s is not in Destination servers", n, h)
			}
			if hosts[h] {
				add("Role %s repeats Server %s", n, h)
			}
			hosts[h] = true
		}
		for _, svc := range r.Services {
			if prev, ok := seen[svc]; ok {
				add("Service %s belongs to Roles %s and %s; require exactly one Role", svc, prev, n)
			}
			seen[svc] = n
		}
	}
	envs := map[string]string{}
	for _, n := range sortedKeys(d.Connections) {
		conn := d.Connections[n]
		if !appPattern.MatchString(n) {
			add("invalid connection name %q", n)
		}
		env := ConnectionEnvName(n)
		if prev, ok := envs[env]; ok {
			add("connections %s and %s have colliding environment names", prev, n)
		}
		envs[env] = n
		if conn.Port < 1 || conn.Port > 65535 {
			add("connection %s port must be 1..65535", n)
		}
		providers := d.ServiceServers(conn.Service)
		switch {
		case conn.Instances && len(providers) == 0:
			add("connection %s target Service %s must resolve to at least one Server", n, conn.Service)
		case !conn.Instances && len(providers) == 0:
			add("connection %s target Service %s must resolve to exactly one Server", n, conn.Service)
		case !conn.Instances && len(providers) > 1:
			add("connection %s target Service %s must resolve to exactly one Server, or set instances: true to address each Server as `<server>.m.%s`", n, conn.Service, conn.Service)
		}
		if conn.Instances {
			for _, h := range providers {
				if !appPattern.MatchString(h) {
					add("connection %s instance name needs Server name %q to match %s (it becomes %s.m.%s)", n, h, appPattern, h, conn.Service)
				}
			}
			if slices.Contains(conn.Consumers, conn.Service) {
				add("connection %s provider %s cannot consume its own instance Connection; split the consumer into another Service", n, conn.Service)
			}
		}
		if len(conn.Consumers) == 0 {
			add("connection %s requires consumers", n)
		}
		hosts := slices.Clone(providers)
		for _, svc := range conn.Consumers {
			hs := d.ServiceServers(svc)
			if len(hs) == 0 {
				add("connection %s unknown consumer %s", n, svc)
			}
			hosts = append(hosts, hs...)
		}
		for _, h := range hosts {
			if c.Network.Mesh == "wireguard" {
				continue
			}
			addr, err := netip.ParseAddr(c.Servers[h].PrivateAddress)
			if err != nil || addr.IsUnspecified() || addr.IsMulticast() || addr.IsLoopback() {
				add("connection %s Server %s requires a private_address (unicast IP)", n, h)
			}
		}
	}
	return errs
}

// InstanceName is the name consumers of an instance Connection use for the
// provider's copy on one Server.
func InstanceName(server, service string) string { return server + ".m." + service }

// ServerEnv returns the x-yoho.tags environment for one Server: each of the
// Server's tags in its tags order, then the Server's own name. Later wins.
func ServerEnv(ext ServiceExt, name string, s Server) map[string]string {
	if len(ext.Tags) == 0 {
		return nil
	}
	out := map[string]string{}
	for _, sel := range append(slices.Clone(s.Tags), name) {
		for k, v := range ext.Tags[sel].Env {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// TagSelectors lists every valid x-yoho.tags key: Server names and tags.
func TagSelectors(servers map[string]Server) []string {
	var out []string
	for name, s := range servers {
		out = append(out, name)
		out = append(out, s.Tags...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func validateServerTags(c *Config, name string) []error {
	var errs []error
	seen := map[string]bool{}
	for _, tag := range c.Servers[name].Tags {
		switch {
		case !appPattern.MatchString(tag):
			errs = append(errs, fmt.Errorf("servers.%s.tags: tag %q must match %s", name, tag, appPattern))
		case seen[tag]:
			errs = append(errs, fmt.Errorf("servers.%s.tags: tag %s is repeated", name, tag))
		default:
			if _, ok := c.Servers[tag]; ok {
				errs = append(errs, fmt.Errorf("servers.%s.tags: tag %s is also a Server name; the name already selects that Server, so use a tag that names a group", name, tag))
			}
		}
		seen[tag] = true
	}
	return errs
}
