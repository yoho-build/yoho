package cli

import (
	"context"
	"fmt"
	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/deploy"
	"slices"

	"github.com/yoho-build/yoho/internal/mesh"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/proxy"
)

func privateAddresses(hosts []plan.NamedHost) map[string]string {
	out := map[string]string{}
	for _, h := range hosts {
		out[h.Name] = h.Server.EffectivePrivateAddress("")
	}
	return out
}

func (a *app) deployLocks(ctx context.Context, hosts []plan.NamedHost, ver string) (func(context.Context) error, error) {
	if a.dest.Roles == nil {
		return a.deployLock(ctx, hosts[0], ver)
	}
	return a.operationLocks(ctx, hosts, deploy.LockInfo{Version: ver, Command: "deploy"}, true)
}

func (a *app) rolesAddresses(ctx context.Context, hosts []plan.NamedHost) error {
	if a.dest.Roles == nil || a.cfg.Network.Mesh != "wireguard" {
		return nil
	}
	all, done, err := a.meshHosts(ctx)
	if err != nil {
		return err
	}
	defer done()
	copies, err := mesh.Read(ctx, all)
	if err != nil {
		return err
	}
	allocations := map[string]string{}
	for _, copy := range copies {
		for name, addr := range copy.Addresses {
			if prev := allocations[name]; prev != "" && prev != addr {
				return fmt.Errorf("conflicting mesh allocation for %s; run yoho mesh up", name)
			}
			allocations[name] = addr
		}
	}
	for i, h := range hosts {
		addr := allocations[h.Name]
		if addr == "" {
			return fmt.Errorf("Server %s has no mesh allocation; run yoho mesh up", h.Name)
		}
		hosts[i].Server.PrivateAddress = addr
	}

	return nil
}

// backupOwner refuses a job spanning independent volume owners: one Backup
// must describe a consistent set of data on one Server.
func (a *app) backupOwner(hosts []plan.NamedHost, j *backupJob) (plan.NamedHost, error) {
	if a.dest.Roles == nil {
		return hosts[0], nil
	}
	owner := ""
	for svc := range j.services {
		hs := a.dest.ServiceServers(svc)
		if len(hs) != 1 {
			return plan.NamedHost{}, fmt.Errorf("Backup Service %s must have exactly one Server", svc)
		}
		if owner != "" && owner != hs[0] {
			return plan.NamedHost{}, fmt.Errorf("Backup job spans Servers; configure one job per owning Server")
		}
		owner = hs[0]
	}
	for _, h := range hosts {
		if h.Name == owner {
			return h, nil
		}
	}
	return plan.NamedHost{}, fmt.Errorf("no owning Server for Backup job")
}

func (a *app) serverHasProxy(name string, ext map[string]config.ServiceExt) bool {
	if a.dest.Roles == nil {
		return true
	}
	for svc, e := range ext {
		if e.Proxy != nil && slices.Contains(a.dest.ServiceServers(svc), name) {
			return true
		}
	}
	return false
}

// rolesJobsApp selects jobs for one volume owner. The copy uses the existing
// single-Server scheduling code, including that Server's sudo mode.
func (a *app) rolesJobsApp(ctx context.Context, hosts []plan.NamedHost, h plan.NamedHost) (*app, error) {
	b := *a
	cfg := *a.cfg
	b.cfg = &cfg
	b.dest.Servers = []string{h.Name}
	b.dest.Roles = nil
	b.dest.Connections = nil
	cfg.Backups = a.cfg.Backups
	cfg.Backups.Jobs = map[string]config.BackupJob{}
	for n, jc := range a.cfg.Backups.Jobs {
		if jc.Destination != a.destName {
			continue
		}
		j, e := a.newBackupJob(n, jc)
		if e != nil {
			return nil, e
		}
		if e = a.resolveServices(ctx, j); e != nil {
			return nil, e
		}
		owner, e := a.backupOwner(hosts, j)
		if e != nil {
			return nil, e
		}
		if owner.Name == h.Name {
			cfg.Backups.Jobs[n] = jc
		}
	}
	return &b, nil
}

// rolesStatus reports ingress health per placed Service and Server.
func (a *app) rolesStatus(ctx context.Context) error {
	r, err := a.compose(ctx)
	if err != nil {
		return err
	}
	hosts, done, err := a.connect(ctx)
	if err != nil {
		return err
	}
	defer done()
	for _, h := range hosts {
		if !a.serverHasProxy(h.Name, r.Ext) {
			continue
		}
		status, e := proxy.Status(ctx, h.Host, a.proxyConfig())
		if e != nil {
			return e
		}
		routes, e := proxy.List(ctx, h.Host)
		if e != nil {
			return e
		}
		for _, svc := range sortedKeys(r.Ext) {
			if r.Ext[svc].Proxy == nil || !slices.Contains(a.dest.ServiceServers(svc), h.Name) {
				continue
			}
			present := false
			for _, route := range routes {
				if route.Service == deploy.RouteName(a.cfg.App, a.destName, svc) && route.Target != "" {
					present = true
				}
			}
			healthy := status.Running && present
			a.ui.Event("service_status", fmt.Sprintf("%s/%s: Proxy healthy=%t", h.Name, svc, healthy), map[string]any{"server": h.Name, "service": svc, "proxy_running": status.Running, "route_present": present, "proxy_healthy": healthy})
		}
	}
	return nil
}

func (a *app) operationLocks(ctx context.Context, hosts []plan.NamedHost, info deploy.LockInfo, steps bool) (func(context.Context) error, error) {
	if a.dest.Roles == nil && len(hosts) > 1 {
		hosts = hosts[:1]
	}
	return deploy.AcquireLocks(ctx, hosts, a.cfg.App, a.destName, info, func(ctx context.Context, h plan.NamedHost, info deploy.LockInfo) (func(context.Context) error, error) {
		if !steps {
			return a.acquireLock(ctx, h.Host, info)
		}
		step := a.ui.Step(h.Name, "Acquire lock")
		u, e := a.acquireLock(ctx, h.Host, info)
		if e != nil {
			step.Fail(e, hintFor(e, "check that the Server is reachable"))
			return nil, &silentError{e}
		}
		step.Done()
		return u, nil
	})
}
