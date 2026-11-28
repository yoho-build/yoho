package composefile

import (
	"fmt"
	"slices"
	"strconv"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/roles"
)

// CheckRoles validates placement against the resolved Compose Services.
func CheckRoles(r *Result, d config.Destination, proxy config.ProxyConfig, app ...string) []Finding {
	appName := r.Project.Name
	if len(app) > 0 {
		appName = app[0]
	}
	if d.Roles == nil {
		return nil
	}
	var out []Finding
	add := func(svc, f string, args ...any) {
		out = append(out, Finding{Level: LevelError, Service: svc, Message: fmt.Sprintf(f, args...)})
	}
	for _, name := range r.Project.ServiceNames() {
		svc := r.Project.Services[name]
		ext := r.Ext[name]
		hosts := d.ServiceServers(name)
		if len(hosts) == 0 {
			add(name, "must belong to exactly one Role")
		}
		if len(hosts) > 1 && (ext.Stateful || stateDescription(svc, r.Project.WorkingDir) != "") {
			add(name, "Stateful Service must resolve to exactly one Server (ADR 0005)")
		}
		if !ext.Stateful && stateDescription(svc, r.Project.WorkingDir) != "" {
			add(name, "persistent data requires x-yoho.stateful: true with Roles")
		}
		if len(hosts) > 1 && ext.Proxy != nil {
			if proxy.Tunnel != nil && proxy.Tunnel.TokenSecret == "" {
				add(name, "multi-Server ingress requires a named Cloudflare Tunnel; Quick Tunnels are unsupported")
			}
			if (proxy.Tunnel == nil || proxy.Tunnel.TokenSecret == "") && (ext.Proxy.SSL == nil || *ext.Proxy.SSL) {
				add(name, "multi-Server Proxy TLS requires a named Cloudflare Tunnel or explicit x-yoho.proxy.ssl: false")
			}
		}
		for dep, condition := range svc.DependsOn {
			for _, h := range hosts {
				if slices.Contains(d.ServiceServers(dep), h) {
					continue
				}
				connected := false
				for _, c := range d.Connections {
					connected = connected || c.Service == dep && slices.Contains(c.Consumers, name)
				}
				if !connected || condition.Condition == "service_completed_successfully" {
					add(name, "cross-Server depends_on %s requires a connection (or colocate the Services); completion jobs must be colocated", dep)
				}
			}
		}

	}
	for _, role := range d.Roles {
		for _, svc := range role.Services {
			if _, ok := r.Project.Services[svc]; !ok {
				add(svc, "Role references unknown Service")
			}
		}
	}
	type serverPort struct {
		server string
		port   int
	}
	ports := map[serverPort]string{}
	for _, name := range sortedMapKeys(d.Connections) {
		conn := d.Connections[name]
		svc, ok := r.Project.Services[conn.Service]
		if !ok {
			add(conn.Service, "connection %s references unknown provider", name)
			continue
		}
		if svc.NetworkMode != "" {
			add(conn.Service, "connection %s target must use Compose networking", name)
		}
		providers := d.ServiceServers(conn.Service)
		for _, h := range providers {
			key := serverPort{h, conn.Port}
			if prev, ok := ports[key]; ok && prev != name {
				add(conn.Service, "connections %s and %s publish the same port on one Server", prev, name)
			}
			ports[key] = name
		}
		if conn.Instances {
			if r.Ext[conn.Service].Proxy != nil {
				add(conn.Service, "instance connection %s provider cannot be proxied; consumers address each Server's container directly", name)
			}
			if svc.GetScale() != 1 {
				add(conn.Service, "instance connection %s provider must run exactly one container per Server (replicas: 1)", name)
			}
		}
		for _, p := range svc.Ports {
			if p.Published == strconv.Itoa(conn.Port) && (p.Target != uint32(conn.Port) || p.Protocol != "" && p.Protocol != "tcp") {
				add(conn.Service, "connection %s conflicts with existing publication on port %d", name, conn.Port)
			}
			if p.Target == uint32(conn.Port) || p.Published == strconv.Itoa(conn.Port) {
				if p.HostIP == "" || p.HostIP == "0.0.0.0" || p.HostIP == "::" {
					add(conn.Service, "connection %s port %d must not bind a wildcard address", name, conn.Port)
				}
			}
		}
		for _, consumer := range conn.Consumers {
			cs, ok := r.Project.Services[consumer]
			if !ok {
				add(consumer, "connection %s references unknown consumer", name)
				continue
			}
			if cs.NetworkMode != "" {
				add(consumer, "connection %s consumer must use Compose networking", name)
			}
			for _, h := range d.ServiceServers(consumer) {
				var names []string
				if len(providers) == 1 && providers[0] != h {
					names = append(names, conn.Service, conn.Service+"."+appName+".yoho.internal")
				}
				if conn.Instances {
					for _, p := range providers {
						names = append(names, roles.InstanceNames(appName, p, conn.Service)...)
					}
				}
				for _, alias := range names {
					if _, ok := cs.ExtraHosts[alias]; ok {
						add(consumer, "connection %s name collision in extra_hosts: %s", name, alias)
					}
				}
				for _, local := range r.Project.ServiceNames() {
					if !slices.Contains(d.ServiceServers(local), h) {
						continue
					}
					for _, net := range r.Project.Services[local].Networks {
						if net != nil && slices.ContainsFunc(names, func(n string) bool { return slices.Contains(net.Aliases, n) }) {
							add(consumer, "connection %s name collides with local Service %s network alias", name, local)
						}
					}
				}
			}
			for _, suffix := range []string{"_HOST", "_PORT"} {
				if _, ok := cs.Environment[config.ConnectionEnvName(name)+suffix]; ok {
					add(consumer, "connection %s environment name collision", name)
				}
			}
		}
	}
	if _, e := roles.Order(d); e != nil {
		add("", "%s", e)
	}
	return out
}
