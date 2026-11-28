package deploy

import (
	"context"
	"fmt"
	"path"
	"slices"

	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/proxy"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/roles"
)

// Preflight is read-only and runs for every Server before any Roles mutation.
func (Compose) Preflight(ctx context.Context, d *plan.Deploy) error {
	if e := validate(d); e != nil {
		return e
	}
	h := d.Servers[0]
	logging, e := DockerLogDriver(ctx, h.Host)
	if e != nil {
		return e
	}
	data, plans, e := compile(d, h.Name, planGenDir, nil, planKey, "", logging)
	if e != nil {
		return e
	}
	if e = checkOwnership(ctx, h.Host, d.App, d.Destination, plans); e != nil {
		return e
	}
	subnet, exists, e := currentSubnet(ctx, h.Host, d.App, d.Destination)
	if e != nil {
		return e
	}
	return checkSubnetChange(release.ProjectName(d.App, d.Destination), data, subnet, exists)
}

// prepareRoleCommands writes a private preparation Compose file, never the
// current Release. It delivers secrets without cutting over any replica.
func prepareRoleCommands(ctx context.Context, d *plan.Deploy) (*runner, []servicePlan, error) {
	h := d.Servers[0]
	dir := release.AppDir(d.App, d.Destination)
	key, e := ensureHMACKey(ctx, h.Host, dir)
	if e != nil {
		return nil, nil, e
	}
	kinds, e := generatedDecls(d)
	if e != nil {
		return nil, nil, e
	}
	generated, e := ensureGenerated(ctx, h.Host, dir, kinds)
	if e != nil {
		return nil, nil, e
	}
	values, e := serviceSecrets(d, generated, func(string, ...any) {})
	if e != nil {
		return nil, nil, e
	}
	gen := release.SecretsDir(d.App, d.Destination, "prepare-"+now().Format("20060102T150405.000000000")+"-"+d.Version)
	asEnv := map[string]bool{}
	for svc, ext := range d.Ext {
		asEnv[svc] = ext.SecretsAsEnv
	}
	if e = writeSecrets(ctx, h.Host, gen, values, asEnv); e != nil {
		return nil, nil, e
	}
	if _, e = shipAppFiles(ctx, h.Host, d, func(string, ...any) {}); e != nil {
		return nil, nil, e
	}
	logging, e := DockerLogDriver(ctx, h.Host)
	if e != nil {
		return nil, nil, e
	}
	data, plans, e := compile(d, h.Name, gen, values, key, DockerServerVersion(ctx, h.Host), logging)
	if e != nil {
		return nil, nil, e
	}
	prep := path.Join(release.Dir(d.App, d.Destination, d.Version), "roles-prepare")
	if e = writeReleaseFiles(ctx, h.Host, prep, data, plans); e != nil {
		return nil, nil, e
	}
	r := newRunner(d, path.Join(prep, "compose.yaml"))
	if plans, e = enforceStopped(ctx, r, plans); e != nil {
		return nil, nil, e
	}
	for _, m := range values {
		for _, v := range m {
			r.secretVals = append(r.secretVals, v)
		}
	}
	// Release containers and prerequisites can join the external yoho network
	// before Compose.Deploy gets a chance to create it on a fresh Server.
	if needsProxy(plans) {
		if e := proxy.BootWith(ctx, h.Host, d.Proxy, r.out, proxy.BootOptions{OnReboot: ProxyRebootHook(d, d.Version, h)}); e != nil {
			return nil, nil, e
		}
		if e := ensureScopedNetwork(ctx, h.Host, d); e != nil {
			return nil, nil, e
		}
	}
	return r, plans, nil
}

// rolesReleasePhase recursively readies dependencies, then runs a Service's
// command on its designated Server before any replica is ever cut over.
func rolesReleasePhase(ctx context.Context, d *plan.Deploy, parts map[string]*plan.Deploy) error {
	if d.SkipReleaseCommands || d.ServiceMove || len(d.Restart) > 0 {
		return nil
	}
	needed := false
	for _, ext := range d.Ext {
		needed = needed || len(ext.ReleaseCommand) > 0
	}
	if !needed {
		return nil
	}
	runners := map[string]*runner{}
	plans := map[string][]servicePlan{}
	for _, server := range sortedKeys(parts) {
		r, ps, e := prepareRoleCommands(ctx, parts[server])
		if e != nil {
			return e
		}
		runners[server] = r
		plans[server] = ps
	}
	running, ready, started := map[string]bool{}, map[string]bool{}, map[string]bool{}
	var ensure func(string, bool) error
	ensure = func(svc string, start bool) error {
		if running[svc] {
			return fmt.Errorf("release prerequisite cycle at %s", svc)
		}
		if ready[svc] && (!start || started[svc]) {
			return nil
		}
		running[svc] = true
		dependencies := map[string]bool{}
		for dep := range d.Project.Services[svc].DependsOn {
			dependencies[dep] = true
		}
		for _, c := range d.Roles.Connections {
			if slices.Contains(c.Consumers, svc) {
				dependencies[c.Service] = true
			}
		}
		for _, dep := range sortedKeys(dependencies) {
			if e := ensure(dep, true); e != nil {
				return e
			}
		}
		hosts := d.Roles.ServiceServers(svc)
		if len(hosts) == 0 {
			return fmt.Errorf("no placement for release Service %s", svc)
		}
		for _, c := range d.Roles.Connections {
			if !slices.Contains(c.Consumers, svc) {
				continue
			}
			for _, provider := range d.Roles.ServiceServers(c.Service) {
				for _, consumer := range hosts {
					if provider != consumer {
						if e := roles.Ready(ctx, parts[consumer].Servers[0].Host, d.PrivateAddresses[provider], c.Port); e != nil {
							return e
						}
					}
				}
			}
		}
		designated := hosts[0]
		var sp servicePlan
		for _, candidate := range plans[designated] {
			if candidate.Name == svc {
				sp = candidate
				break
			}
		}
		// Render keeps the command only on the designated Server.
		if !ready[svc] {
			if e := runners[designated].releaseCommands(ctx, []servicePlan{sp}); e != nil {
				return e
			}
		}
		if start {
			for _, server := range hosts {
				r := runners[server]
				if e := r.h.Run(ctx, remote.Cmd{Script: r.cc.cmd("up", "-d", "--no-recreate", "--no-deps", svc)}); e != nil {
					return e
				}
				for _, candidate := range plans[server] {
					if candidate.Name == svc {
						candidate.Check = nil
						if e := r.waitServices(ctx, []servicePlan{candidate}, "prerequisite-ready"); e != nil {
							return e
						}
					}
				}
			}
		}
		running[svc] = false
		ready[svc] = true
		started[svc] = started[svc] || start
		return nil
	}
	for _, svc := range sortedKeys(d.Ext) {
		if len(d.Ext[svc].ReleaseCommand) > 0 {
			if e := ensure(svc, false); e != nil {
				return e
			}
		}
	}
	return nil
}

func (Roles) Preflight(ctx context.Context, d *plan.Deploy) error {
	parts, order, e := roleParts(d)
	if e != nil {
		return e
	}
	if e = checkRolesOwnership(ctx, d); e != nil {
		return e
	}
	for _, server := range order {
		if e = (Compose{}).Preflight(ctx, parts[server]); e != nil {
			return e
		}
	}
	return nil
}
