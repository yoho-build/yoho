package swarm

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// removeWait bounds how long RemoveStack waits for task containers to go.
// A variable so tests do not wait.
var (
	removeWait = 2 * time.Minute
	removePoll = 2 * time.Second
)

// StackRmScript removes the App Destination's stack and its release-command
// stack on the manager, by exact stack name. A stack that does not exist is
// skipped.
func StackRmScript(app, dest string) string {
	stack := release.ProjectName(app, dest)
	return "set -eu\nstacks=$(docker stack ls --format '{{.Name}}')\n" +
		"for s in " + remote.QuoteArgs(stack, ReleaseStack(stack)) + "; do\n" +
		"  if printf '%s\\n' \"$stacks\" | grep -qxF -- \"$s\"; then docker stack rm \"$s\" >/dev/null; fi\n" +
		"done\n"
}

// RemoveStack is the swarm part of `yoho remove`: stack rm on the manager
// (the first Server), wait until no task container of the stacks is left
// on any Server, then remove the Swarm secrets Yoho created for the App
// Destination (labelled yoho.app/yoho.destination). Networks, volumes and
// the App directory are removed by the caller with the deploy helpers.
func RemoveStack(ctx context.Context, hosts []plan.NamedHost, app, dest string, logf func(string, ...any)) error {
	if len(hosts) == 0 {
		return fmt.Errorf("no Servers")
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	mgr := hosts[0]
	if err := mgr.Host.Run(ctx, remote.Cmd{Script: StackRmScript(app, dest)}); err != nil {
		return fmt.Errorf("docker stack rm on %s: %w", mgr.Name, err)
	}
	o := deploy.RemoveOptions{App: app, Destination: dest, Swarm: true}
	check := "docker ps -aq --filter " + remote.Quote("label="+stackLabel+"="+release.ProjectName(app, dest)) +
		"; docker ps -aq --filter " + remote.Quote("label="+stackLabel+"="+ReleaseStack(release.ProjectName(app, dest)))
	deadline := time.Now().Add(removeWait)
	for _, h := range hosts {
		for {
			out, err := h.Host.Output(ctx, remote.Cmd{Script: check})
			if err != nil {
				return fmt.Errorf("list task containers on %s: %w", h.Name, err)
			}
			if strings.TrimSpace(out) == "" {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("task containers of %s still on %s after %s; rerun yoho remove", release.ProjectName(app, dest), h.Name, removeWait)
			}
			logf("waiting for task containers to stop on %s", h.Name)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(removePoll):
			}
		}
	}
	secrets := "for s in $(docker secret ls -q " + remote.QuoteArgs("--filter", "label="+deploy.LabelApp+"="+o.App, "--filter", "label="+deploy.LabelDestination+"="+o.Destination) + "); do docker secret rm \"$s\" >/dev/null; done"
	if err := mgr.Host.Run(ctx, remote.Cmd{Script: secrets}); err != nil {
		return fmt.Errorf("remove Swarm secrets on %s: %w", mgr.Name, err)
	}
	return nil
}
