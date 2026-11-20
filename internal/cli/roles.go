package cli

import (
	"context"
	"errors"
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
	hosts = slices.Clone(hosts)
	slices.SortFunc(hosts, func(a, b plan.NamedHost) int {
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})
	var releases []func(context.Context) error
	unlock := func(ctx context.Context) error {
		var errs []error
		for i := len(releases) - 1; i >= 0; i-- {
			errs = append(errs, releases[i](ctx))
		}
		return errors.Join(errs...)
	}
	for _, h := range hosts {
		f, err := a.deployLock(ctx, h, ver)
		if err != nil {
			return nil, errors.Join(err, unlock(context.WithoutCancel(ctx)))
		}
		releases = append(releases, f)
	}
	return unlock, nil
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
	names := []string{}
	for n := range a.cfg.Servers {
		names = append(names, n)
	}
	state, err := mesh.Allocate(mesh.Defaults(a.cfg.Network).Subnet, names, copies)
	if err != nil {
		return err
	}
	for i, h := range hosts {
		hosts[i].Server.PrivateAddress = h.Server.EffectivePrivateAddress(state.Addresses[h.Name])
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
