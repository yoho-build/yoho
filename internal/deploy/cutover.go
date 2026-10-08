package deploy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/yoho-build/yoho/internal/proxy"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/secrets"
)

// composeCLI builds `docker compose` invocations for one compiled file.
type composeCLI struct {
	project string
	dir     string // project directory (the App Destination dir)
	file    string
}

func (c composeCLI) cmd(args ...string) string {
	return "docker compose " + remote.QuoteArgs(append([]string{"-p", c.project, "--project-directory", c.dir, "-f", c.file}, args...)...)
}

// RouteName is the Proxy route (kamal-proxy service) of a Service.
func RouteName(app, destination, service string) string {
	return app + "-" + destination + "-" + service
}

// runner executes cutover steps on one Server. All remote work goes through
// h, so tests assert the exact command sequence with a fake Host.
type runner struct {
	h         remote.Host
	out       io.Writer
	server    string
	app, dest string
	cc        composeCLI
	// secretVals are the resolved and generated secret values of this
	// deploy, masked in remote output that joins an error.
	secretVals []string
}

// redact masks secret values in s.
func (r *runner) redact(s string) string {
	if len(r.secretVals) == 0 || s == "" {
		return s
	}
	var b bytes.Buffer
	w := secrets.NewRedactor(&b, r.secretVals...)
	_, _ = w.Write([]byte(s))
	_ = w.Flush()
	return b.String()
}

func (r *runner) logf(format string, a ...any) {
	fmt.Fprintf(r.out, "["+r.server+"] "+format+"\n", a...)
}

func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// startDependencies starts (without recreating) what release_command
// Services depend on, so migrations work on the first deploy too.
func (r *runner) startDependencies(ctx context.Context, plans []servicePlan) error {
	var deps []string
	hasRelease := false
	for _, sp := range plans {
		if len(sp.ReleaseCommand) == 0 {
			continue
		}
		hasRelease = true
		for _, dep := range sp.DependsOn {
			if !slices.Contains(deps, dep) {
				deps = append(deps, dep)
			}
		}
	}
	// Migrations need the database even when depends_on is not declared:
	// Stateful Services are started (never recreated) before release commands.
	if hasRelease {
		for _, sp := range plans {
			if sp.Stateful && !slices.Contains(deps, sp.Name) {
				deps = append(deps, sp.Name)
			}
		}
	}
	if len(deps) == 0 {
		return nil
	}
	slices.Sort(deps)
	r.logf("starting dependencies %s", strings.Join(deps, ", "))
	if err := r.h.Run(ctx, remote.Cmd{Script: r.cc.cmd(append([]string{"up", "-d", "--no-recreate"}, deps...)...)}); err != nil {
		return fmt.Errorf("start dependencies: %w", err)
	}
	return nil
}

func tailLines(s string, n int) string {
	ls := lines(s)
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	return strings.Join(ls, "\n")
}

// releaseCommands runs each x-yoho.release_command in a one-off container
// from the new image. The old version keeps serving if one fails.
func (r *runner) releaseCommands(ctx context.Context, plans []servicePlan) error {
	for _, sp := range plans {
		if len(sp.ReleaseCommand) == 0 {
			continue
		}
		r.logf("release command for %s: %s", sp.Name, strings.Join(sp.ReleaseCommand, " "))
		args := append([]string{"run", "--rm", "--no-deps", "-T", sp.Name}, sp.ReleaseCommand...)
		// Output is kept and shown only on failure, so migrations don't flood the deploy log.
		var buf bytes.Buffer
		if err := r.h.Run(ctx, remote.Cmd{Script: r.cc.cmd(args...) + " 2>&1", Stdout: &buf}); err != nil {
			// The output joins the error, which the CLI prints outside the
			// progress redactor; the command can see every mounted secret.
			return fmt.Errorf("release command for %s failed, aborting (the previous version keeps running): %w\n%s", sp.Name, err, r.redact(tailLines(buf.String(), 20)))
		}
	}
	return nil
}

// cutover switches every Service to the compiled file: non-proxied Services
// (including Stateful ones) via `up -d`, which recreates changed containers
// stop-first; proxied Services via scale-up + Proxy switch + remove old.
// Containers of Services no longer in the file are removed last.
func (r *runner) cutover(ctx context.Context, plans []servicePlan) error {
	var plain []string
	for _, sp := range plans {
		if !sp.proxied() {
			plain = append(plain, sp.Name)
		}
	}
	if len(plain) > 0 {
		r.logf("starting %s", strings.Join(plain, ", "))
		if err := r.h.Run(ctx, remote.Cmd{Script: r.cc.cmd(append([]string{"up", "-d", "--no-deps"}, plain...)...)}); err != nil {
			return fmt.Errorf("compose up: %w", err)
		}
	}
	for _, sp := range plans {
		if sp.proxied() {
			if err := r.cutoverProxied(ctx, sp); err != nil {
				return err
			}
		}
	}
	return r.removeOrphans(ctx, plans)
}

// cutoverProxied replaces a proxied Service without downtime:
//
//  1. remove stopped leftovers, record running (old) container ids;
//  2. `up -d --no-deps --no-recreate --scale svc=old+replicas` adds
//     containers from the new config and leaves the old ones untouched;
//  3. wait for the new containers' docker healthcheck (if any);
//  4. `kamal-proxy deploy` to the new containers (health-gated, drains old);
//  5. stop and remove the old containers directly with docker.
//
// Old containers are removed with `docker rm` rather than scaling back down,
// because which container compose drops on scale-down is not ours to pick.
// The next deploy starts from whatever is running. On failure the new
// containers are removed and the Proxy keeps routing to the old ones.
func (r *runner) cutoverProxied(ctx context.Context, sp servicePlan) error {
	out, err := r.h.Output(ctx, remote.Cmd{Script: r.cc.cmd("rm", "-f", sp.Name) + " >/dev/null 2>&1 || true\n" + r.cc.cmd("ps", "-q", sp.Name)})
	if err != nil {
		return fmt.Errorf("list containers of %s: %w", sp.Name, err)
	}
	old := lines(out)

	scale := len(old) + sp.Replicas
	r.logf("starting new %s container(s) next to %d running", sp.Name, len(old))
	out, err = r.h.Output(ctx, remote.Cmd{Script: "set -e\n" +
		r.cc.cmd("up", "-d", "--no-deps", "--no-recreate", "--scale", sp.Name+"="+strconv.Itoa(scale), sp.Name) + " >&2\n" +
		r.cc.cmd("ps", "-q", sp.Name)})
	if err != nil {
		return fmt.Errorf("start new %s containers: %w", sp.Name, err)
	}
	var fresh []string
	for _, id := range lines(out) {
		if !slices.Contains(old, id) {
			fresh = append(fresh, id)
		}
	}
	fail := func(err error) error {
		if len(fresh) > 0 {
			r.logf("removing new %s container(s), previous version keeps serving", sp.Name)
			if rmErr := r.h.Run(context.WithoutCancel(ctx), remote.Cmd{Script: "docker rm -f " + remote.QuoteArgs(fresh...) + " >/dev/null"}); rmErr != nil {
				r.logf("warning: could not remove new containers: %v", rmErr)
			}
		}
		return err
	}
	if len(fresh) != sp.Replicas {
		return fail(fmt.Errorf("service %s: expected %d new container(s), compose started %d", sp.Name, sp.Replicas, len(fresh)))
	}

	r.logf("waiting for %s to become healthy", sp.Name)
	if err := r.h.Run(ctx, remote.Cmd{Script: waitHealthyScript(fresh, sp.Proxy.DeployTimeout)}); err != nil {
		return fail(fmt.Errorf("service %s did not become healthy: %w", sp.Name, err))
	}

	out, err = r.h.Output(ctx, remote.Cmd{Script: "docker inspect -f '{{.Name}}' " + remote.QuoteArgs(fresh...)})
	if err != nil {
		return fail(fmt.Errorf("inspect new %s containers: %w", sp.Name, err))
	}
	var targets []string
	for _, n := range lines(out) {
		targets = append(targets, strings.TrimPrefix(n, "/")+":"+strconv.Itoa(sp.Proxy.Port))
	}
	route := RouteName(r.app, r.dest, sp.Name)
	r.logf("switching proxy route %s to %s", route, strings.Join(targets, ","))
	err = proxy.Deploy(ctx, r.h, route, strings.Join(targets, ","), proxy.DeployOptions{
		Hosts:         sp.Proxy.Hosts,
		HealthPath:    sp.Proxy.HealthPath,
		TLS:           sp.Proxy.TLS,
		DeployTimeout: time.Duration(sp.Proxy.DeployTimeout) * time.Second,
		DrainTimeout:  time.Duration(sp.Proxy.DrainTimeout) * time.Second,
	})
	if err != nil {
		return r.proxySwitchFailed(ctx, sp.Name, route, targets, err, fail)
	}
	r.logf("switched proxy route %s", route)

	if len(old) > 0 {
		// kamal-proxy has drained them; docker stop honors stop_grace_period.
		r.logf("removing old %s container(s)", sp.Name)
		// The route already targets the new containers: the deploy is
		// committed. A failure here leaves stray old containers, not a
		// failed Release, so it is only a warning.
		if err := r.h.Run(context.WithoutCancel(ctx), remote.Cmd{Script: "set -e\ndocker stop " + remote.QuoteArgs(old...) + " >/dev/null\ndocker rm " + remote.QuoteArgs(old...) + " >/dev/null"}); err != nil {
			r.logf("warning: traffic is switched, but removing the old %s container(s) failed (remove them with `docker rm -f %s`): %v", sp.Name, strings.Join(old, " "), err)
		}
	}
	return nil
}

// proxySwitchFailed handles a failed `kamal-proxy deploy`. When kamal-proxy
// itself refused (target unhealthy), the route still points at the old
// containers and the new ones are removed. But a cancelled deploy (Ctrl-C,
// lost SSH) only stops waiting: kamal-proxy may have switched, or may still
// switch, to the new containers. Removing them would leave the route pointing
// at deleted containers, so they are kept unless the route is known to
// target only the old ones after kamal-proxy finished.
func (r *runner) proxySwitchFailed(ctx context.Context, svc, route string, targets []string, err error, fail func(error) error) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	routes, lerr := proxy.List(cctx, r.h)
	switched := false
	for _, rt := range routes {
		if rt.Service != route {
			continue
		}
		for _, t := range strings.Split(rt.Target, ",") {
			switched = switched || slices.Contains(targets, strings.TrimSpace(t))
		}
	}
	if lerr == nil && !switched && ctx.Err() == nil {
		return fail(err)
	}
	r.logf("warning: proxy route %s may already target the new %s container(s); old and new containers are left running", route, svc)
	return fmt.Errorf("%w (the new %s containers were kept because the proxy route may use them; run the deploy again to finish)", err, svc)
}

// waitHealthyScript waits until each container runs and its healthcheck (if
// any) reports healthy; fails on exit, unhealthy, or timeout.
func waitHealthyScript(ids []string, timeoutSec int) string {
	if timeoutSec <= 0 {
		timeoutSec = defaultDeployTimeout
	}
	return `set -eu
deadline=$(( $(date +%s) + ` + strconv.Itoa(timeoutSec) + ` ))
for id in ` + remote.QuoteArgs(ids...) + `; do
  while :; do
    s=$(docker inspect -f '{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$id")
    case "$s" in
      "running healthy"|"running none") break ;;
      *" unhealthy"|exited*|dead*) echo "container $id: $s" >&2; exit 1 ;;
    esac
    if [ "$(date +%s)" -ge "$deadline" ]; then echo "timeout waiting for $id ($s)" >&2; exit 1; fi
    sleep 1
  done
done`
}

// removeOrphans stops and removes containers of this compose project whose
// Service is no longer in the compiled file (like --remove-orphans, but
// without asking compose to converge the proxied Services).
func (r *runner) removeOrphans(ctx context.Context, plans []servicePlan) error {
	out, err := r.h.Output(ctx, remote.Cmd{Script: "docker ps -a --filter " + remote.Quote("label=com.docker.compose.project="+r.cc.project) + ` --format '{{.ID}} {{.Label "com.docker.compose.service"}}'`})
	if err != nil {
		return fmt.Errorf("list project containers: %w", err)
	}
	var ids, names []string
	for _, l := range lines(out) {
		id, svc, _ := strings.Cut(l, " ")
		if slices.ContainsFunc(plans, func(sp servicePlan) bool { return sp.Name == svc }) {
			continue
		}
		ids = append(ids, id)
		if !slices.Contains(names, svc) {
			names = append(names, svc)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	r.logf("removing containers of removed services: %s", strings.Join(names, ", "))
	if err := r.h.Run(ctx, remote.Cmd{Script: "set -e\ndocker stop " + remote.QuoteArgs(ids...) + " >/dev/null\ndocker rm " + remote.QuoteArgs(ids...) + " >/dev/null"}); err != nil {
		return fmt.Errorf("remove orphan containers: %w", err)
	}
	return nil
}
