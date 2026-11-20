package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/yoho-build/yoho/internal/dash"
	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/remote"
)

func addServerFlag(c *cobra.Command) {
	c.Flags().String("server", "", "Roles Server hosting the requested Service")
}
func (a *app) serviceServer(service, chosen string) (string, error) {
	hosts := a.dest.ServiceServers(service)
	if len(hosts) == 0 {
		return "", fmt.Errorf("no placement for Service %s", service)
	}
	if chosen != "" {
		if !slices.Contains(hosts, chosen) {
			return "", fmt.Errorf("Service %s is not placed on Server %s", service, chosen)
		}
		return chosen, nil
	}
	if len(hosts) > 1 {
		a.ui.Info("Service %s runs on %s; using %s (select another with --server)", service, strings.Join(hosts, ", "), hosts[0])
	}
	return hosts[0], nil
}
func (a *app) serviceApp(cmd *cobra.Command, service string) (*app, error) {
	if a.dest.Roles == nil {
		return a, nil
	}
	chosen, _ := cmd.Flags().GetString("server")
	server, e := a.serviceServer(service, chosen)
	if e != nil {
		return nil, e
	}
	b := *a
	b.dest.Servers = []string{server}
	return &b, nil
}
func (a *app) rolesLogs(cmd *cobra.Command, services []string, opts dash.LogsOptions) error {
	chosen, _ := cmd.Flags().GetString("server")
	groups := map[string][]string{}
	for _, svc := range services {
		h, e := a.serviceServer(svc, chosen)
		if e != nil {
			return e
		}
		groups[h] = append(groups[h], svc)
	}
	if opts.Follow && len(groups) > 1 {
		return fmt.Errorf("following logs across Servers requires --server and Services hosted there")
	}
	for _, name := range sortedKeys(groups) {
		b := *a
		b.dest.Servers = []string{name}
		script := appLogsScript(a.project(), groups[name], false, opts)
		if opts.Follow {
			if opts.Grep != "" {
				script = appFollowGrepScript(a.project(), groups[name], false, opts)
			} else {
				script = appFollowScript(a.project(), groups[name], false, opts)
			}
			if e := b.follow(cmd, cmd.ErrOrStderr(), script); e != nil {
				return a.logsError(e, opts)
			}
		} else if e := b.logsRun(cmd, cmd.ErrOrStderr(), script); e != nil {
			return a.logsError(e, opts)
		}
	}
	return nil
}

func (a *app) rolesManualLock(ctx context.Context, verb, message string, force bool) error {
	hosts, done, e := a.connectForLocks()
	if e != nil {
		return e
	}
	defer done()
	if verb == "acquire" {
		u, e := a.operationLocks(ctx, hosts, deploy.LockInfo{Performer: performer(), Command: deploy.ManualLockCommand, Message: message, Time: time.Now().UTC()}, true)
		if e != nil {
			return e
		}
		if e = beginOperation(ctx); e != nil {
			_ = u(ctx)
			return e
		}
		a.ui.Event("lock", "Locked all Roles Servers", map[string]any{"action": verb, "locked": true})
		return nil
	}
	// Refuse the entire release before dropping any maintenance lock.
	if verb == "release" && !force {
		for _, h := range hosts {
			info, held, e := deploy.ReadLock(ctx, h.Host, a.cfg.App, a.destName)
			if e != nil {
				return e
			}
			if held && info.Command != deploy.ManualLockCommand {
				return fmt.Errorf("Server %s lock is held by %s; wait or use --force for a killed run", h.Name, describeHolder(info))
			}
		}
	}
	for _, h := range hosts {
		var info deploy.LockInfo
		var held bool
		if verb == "status" {
			info, held, e = deploy.ReadLock(ctx, h.Host, a.cfg.App, a.destName)
		} else {
			if e = beginOperation(ctx); e != nil {
				return e
			}
			info, held, e = deploy.ReleaseLock(ctx, h.Host, a.cfg.App, a.destName, force)
		}
		if e != nil {
			return e
		}
		fields := withAction(lockFields(info, held), verb)
		fields["server"] = h.Name
		if verb == "release" {
			fields["released"] = held
			fields["locked"] = false
		}
		a.ui.Event("lock", fmt.Sprintf("%s: locked=%t", h.Name, held && verb != "release"), fields)
	}
	return nil
}

// authority is where operator audit/operation records are stored. Each record
// is one Destination-wide journal, even when a Backup's data lives elsewhere.
func (a *app) authority(ctx context.Context, h plan.NamedHost) (remote.Host, func(), error) {
	dest, ok := a.cfg.Destinations[a.destName]
	if !ok || dest.Roles == nil || len(dest.Servers) == 0 || dest.Servers[0] == h.Name {
		return h.Host, func() {}, nil
	}
	name := dest.Servers[0]
	srv := a.cfg.Servers[name]
	conn, e := connectServer(name, srv.SSH, srv.Sudo, a.sshOptions())
	if e != nil {
		return nil, nil, e
	}
	return conn, func() { _ = conn.Close() }, nil
}

// Maintenance locks and operation journals remain inspectable when Docker or
// the mesh is down. Their SSH connections do not probe either runtime.
func (a *app) connectForLocks() ([]plan.NamedHost, func(), error) {
	var hosts []plan.NamedHost
	done := func() {
		for _, h := range hosts {
			_ = h.Host.Close()
		}
	}
	for _, name := range a.dest.Servers {
		server := a.cfg.Servers[name]
		h, e := connectServer(name, server.SSH, server.Sudo, a.sshOptions())
		if e != nil {
			done()
			return nil, nil, e
		}
		hosts = append(hosts, plan.NamedHost{Name: name, Server: server, Host: h})
	}
	return hosts, done, nil
}
