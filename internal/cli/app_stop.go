package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/ui"
)

func appStopCmd(g *globals) *cobra.Command {
	var reason string
	c := &cobra.Command{
		Use:   "stop SERVICE",
		Short: "Stop a Service and keep it stopped through deploy, apply and rollback",
		Long: `Stop and remove the containers of SERVICE on its Servers and record it as
stopped on each Server. deploy, apply and rollback then leave it down, and
plan shows it as stopped, until ` + "`yoho app start SERVICE`" + `. Volumes are
never touched.

With Roles it acts on every Server hosting SERVICE; --server picks one.
Services with x-yoho.proxy cannot be stopped this way: set deploy.replicas: 0
and deploy. Not available on runtime swarm or with scoped access.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			chosen, _ := cmd.Flags().GetString("server")
			return a.stopService(cmd.Context(), args[0], chosen, reason)
		},
	}
	addServerFlag(c)
	c.Flags().StringVar(&reason, "reason", "", "Why the Service is stopped (shown by plan and app ps)")
	addLockWaitFlag(c)
	return c
}

func appStartCmd(g *globals) *cobra.Command {
	c := &cobra.Command{
		Use:   "start SERVICE",
		Short: "Start a Service stopped by `yoho app stop`",
		Long: `Remove the stopped record of SERVICE, start it from the current Release's
compiled compose (without its dependencies) and wait until it is ready.
Rerunning it on a running Service changes nothing.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			chosen, _ := cmd.Flags().GetString("server")
			return a.startService(cmd.Context(), args[0], chosen)
		},
	}
	addServerFlag(c)
	addLockWaitFlag(c)
	return c
}

// stopTargets connects the Servers `app stop`/`app start` act on and
// refuses, before any change, what they do not support.
func (a *app) stopTargets(ctx context.Context, verb, service, chosen string) ([]plan.NamedHost, func(), error) {
	if a.scoped {
		return nil, nil, &hintError{"app " + verb + " is not available with scoped access", "ask an Operator"}
	}
	if a.dest.Runtime == "swarm" {
		return nil, nil, &hintError{"app " + verb + " is not available on runtime swarm", "set deploy.replicas: 0 for the Service and deploy"}
	}
	var names []string
	if a.dest.Roles != nil {
		hs := a.dest.ServiceServers(service)
		if len(hs) == 0 && verb == "start" {
			// A Service removed from the config may have orphaned stopped
			// entries on any Server; start clears them.
			hs = slices.Clone(a.dest.Servers)
			slices.Sort(hs)
		}
		if len(hs) == 0 {
			return nil, nil, &hintError{fmt.Sprintf("no placement for Service %s", service), "place it in a Role of this Destination"}
		}
		names = hs
		if chosen != "" {
			if !slices.Contains(hs, chosen) {
				return nil, nil, &hintError{fmt.Sprintf("Service %s is not placed on Server %s", service, chosen), "Servers: " + strings.Join(hs, ", ")}
			}
			names = []string{chosen}
		}
	} else {
		names = []string{a.dest.Servers[0]}
		if chosen != "" && chosen != names[0] {
			return nil, nil, &hintError{fmt.Sprintf("Destination %s runs on Server %s, not %s", a.destName, names[0], chosen), "drop --server"}
		}
	}
	var hosts []plan.NamedHost
	done := func() {
		for _, h := range hosts {
			_ = h.Host.Close()
		}
	}
	for _, name := range names {
		srv := a.cfg.Servers[name]
		h, err := connectServer(name, srv.SSH, srv.Sudo, a.sshOptions())
		if err != nil {
			done()
			return nil, nil, err
		}
		hosts = append(hosts, plan.NamedHost{Name: name, Server: srv, Host: h})
	}
	var keep []plan.NamedHost
	for _, h := range hosts {
		_, svcs, err := deploy.CurrentServices(ctx, h.Host, a.cfg.App, a.destName)
		if errors.Is(err, deploy.ErrNoCurrentRelease) {
			done()
			return nil, nil, &hintError{fmt.Sprintf("%s has no current Release on %s", a.cfg.App, h.Name), "run `yoho deploy` first"}
		}
		if err != nil {
			done()
			return nil, nil, err
		}
		var names []string
		found := false
		for _, s := range svcs {
			names = append(names, s.Name)
			if s.Name != service {
				continue
			}
			found = true
			if s.Proxied {
				done()
				return nil, nil, &hintError{fmt.Sprintf("Service %s is proxied; app %s supports Services without x-yoho.proxy", service, verb), "to take it down, set deploy.replicas: 0 and deploy"}
			}
		}
		if !found {
			if verb == "start" {
				// `app start` clears an orphaned stopped entry.
				s, err := deploy.ReadStopped(ctx, h.Host, a.cfg.App, a.destName)
				if err != nil {
					done()
					return nil, nil, err
				}
				if _, ok := s[service]; ok {
					a.orphans[h.Name] = true
					keep = append(keep, h)
					continue
				}
				if a.dest.Roles != nil && len(a.dest.ServiceServers(service)) == 0 {
					continue // not placed here and no entry: nothing to do on this Server
				}
			}
			done()
			return nil, nil, &hintError{fmt.Sprintf("Service %s is not in the current Release on %s", service, h.Name), "Services: " + strings.Join(names, ", ")}
		}
		keep = append(keep, h)
	}
	if len(keep) == 0 {
		done()
		return nil, nil, &hintError{fmt.Sprintf("no placement for Service %s", service), "place it in a Role of this Destination"}
	}
	return keep, done, nil
}

func (a *app) stopService(ctx context.Context, service, chosen, reason string) (err error) {
	hosts, done, err := a.stopTargets(ctx, "stop", service, chosen)
	if err != nil {
		return a.fail(&validationError{err})
	}
	defer done()
	defer func() {
		if err != nil {
			a.ui.Finished(classifyMutation(err, !preMutation(ctx)), "")
		}
	}()
	unlock, err := a.operationLocks(ctx, hosts, deploy.LockInfo{Command: "app stop"}, true)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, unlock(context.WithoutCancel(ctx))) }()
	if err = beginOperation(ctx); err != nil {
		return err
	}
	info := deploy.StoppedEntry{By: performer(), At: time.Now().UTC(), Reason: reason}
	changed := 0
	for _, h := range hosts {
		step := a.ui.Step(h.Name, "Stop %s", service)
		res, e := deploy.StopService(ctx, h.Host, a.cfg.App, a.destName, service, info)
		if e != nil {
			step.Fail(e, "check that Docker is running on the Server; rerun `yoho app stop "+service+"`")
			return &silentError{e}
		}
		detail := "already stopped"
		if res.Changed {
			changed++
			detail = fmt.Sprintf("removed %s", ui.Count(len(res.Containers), "container", ""))
		}
		step.Done(detail)
		a.ui.Event("service_stop", fmt.Sprintf("%s/%s: stopped (%s)", h.Name, service, detail), map[string]any{
			"service": service, "server": h.Name, "changed": res.Changed, "containers": res.Containers, "exit_codes": res.ExitCodes,
		})
		for _, id := range res.Containers {
			if code := res.ExitCodes[id]; code != 0 {
				a.ui.Warn("%s: container %s exited %d after docker stop (not a clean shutdown)", h.Name, id, code)
			}
		}
	}
	summary := "Stopped " + service + "; `yoho app start " + service + "` runs it again"
	if changed == 0 {
		summary = service + " was already stopped"
	}
	a.ui.Finished(nil, summary)
	return nil
}

func (a *app) startService(ctx context.Context, service, chosen string) (err error) {
	a.orphans = map[string]bool{}
	hosts, done, err := a.stopTargets(ctx, "start", service, chosen)
	if err != nil {
		return a.fail(&validationError{err})
	}
	defer done()
	defer func() {
		if err != nil {
			a.ui.Finished(classifyMutation(err, !preMutation(ctx)), "")
		}
	}()
	unlock, err := a.operationLocks(ctx, hosts, deploy.LockInfo{Command: "app start"}, true)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, unlock(context.WithoutCancel(ctx))) }()
	if err = beginOperation(ctx); err != nil {
		return err
	}
	changed := 0
	for _, h := range hosts {
		step := a.ui.Step(h.Name, "Start %s", service)
		d := &plan.Deploy{App: a.cfg.App, Destination: a.destName, Servers: []plan.NamedHost{h}, Performer: performer(), Out: step.Output()}
		c, e := deploy.StartService(ctx, d, service)
		if e != nil {
			hint := "check that Docker is running on the Server"
			var se *deploy.StartError
			if errors.As(e, &se) {
				hint = se.Hint()
			}
			step.Fail(e, hint)
			a.ui.Event("service_start", fmt.Sprintf("%s/%s: not ready", h.Name, service), map[string]any{"service": service, "server": h.Name, "changed": c, "ready": false})
			return &silentError{e}
		}
		detail := "already running"
		switch {
		case c && a.orphans[h.Name]:
			changed++
			detail = "cleared its stopped entry (not in the current Release)"
		case c:
			changed++
			detail = "started"
		case a.orphans[h.Name]:
			detail = "no stopped entry"
		}
		step.Done(detail)
		a.ui.Event("service_start", fmt.Sprintf("%s/%s: %s", h.Name, service, detail), map[string]any{"service": service, "server": h.Name, "changed": c, "ready": true})
	}
	summary := "Started " + service
	if changed == 0 {
		summary = service + " was not stopped; it is running"
	}
	a.ui.Finished(nil, summary)
	return nil
}

// stoppedLines are `app ps` lines for the Services stopped on h. inRelease
// reports whether a Service is in the current Release (nil: unknown).
func stoppedLines(s deploy.Stopped, inRelease func(string) bool) []string {
	var out []string
	for _, name := range sortedKeys(s) {
		if inRelease != nil && !inRelease(name) {
			out = append(out, fmt.Sprintf("%s: stopped by `yoho app stop` (%s) (not in the current Release); `yoho app start %s` clears the entry", name, s[name].Describe(), name))
			continue
		}
		out = append(out, fmt.Sprintf("%s: stopped by `yoho app stop` (%s); `yoho app start %s` runs it", name, s[name].Describe(), name))
	}
	return out
}
