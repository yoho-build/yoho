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

	"github.com/yoho-build/yoho/internal/config"
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

// containerIDs excludes compose run one-offs from every readiness/candidate
// enumeration, including failed-start rediscovery. Inspection fails closed.
func (c composeCLI) containerIDs(service string, all bool) string {
	args := []string{"ps"}
	if all {
		args = append(args, "--all")
	}
	args = append(args, "-q", service)
	return "set -eu\ncids=$(" + c.cmd(args...) + ")\n" + `for cid in $cids; do
  oneoff=$(docker inspect -f '{{index .Config.Labels "com.docker.compose.oneoff"}}' "$cid")
  if [ "$oneoff" != True ] && [ "$oneoff" != true ]; then printf '%s\n' "$cid"; fi
done`
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
	registry  *config.Registry // names Yoho-built images (pinImages)
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
	if err := validateReadinessPlans(plans); err != nil {
		return err
	}
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
	// Compose also starts transitive prerequisites. Gate those before running
	// a release command with --no-deps, using the same policy as cutover.
	for i := 0; i < len(deps); i++ {
		for _, sp := range plans {
			if sp.Name == deps[i] {
				for _, dep := range sp.DependsOn {
					if !slices.Contains(deps, dep) {
						deps = append(deps, dep)
					}
				}
			}
		}
	}
	for _, sp := range plans {
		if slices.Contains(deps, sp.Name) {
			if err := r.waitService(ctx, sp); err != nil {
				return fmt.Errorf("start dependencies: %w", err)
			}
		}
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

// validateReadinessPlans rejects impossible completion contracts before any
// Service startup or release command. A scaled-zero Service is otherwise valid.
func validateReadinessPlans(plans []servicePlan) error {
	if _, err := dependencyOrder(plans); err != nil {
		return err
	}
	byName := map[string]servicePlan{}
	for _, sp := range plans {
		byName[sp.Name] = sp
	}
	for _, sp := range plans {
		if sp.Replicas == 0 {
			continue
		}
		for _, dep := range sp.CompletedDependencies {
			job, ok := byName[dep]
			if !ok {
				return fmt.Errorf("Service %s: completion dependency %s missing from saved plan", sp.Name, dep)
			}
			if job.Replicas == 0 {
				return fmt.Errorf("Service %s requires successful completion of scaled-zero Service %s", sp.Name, dep)
			}
		}
	}
	return nil
}

// dependencyOrder validates the saved plan before starting anything. Prerequisites
// become ready before dependants, irrespective of whether either uses the Proxy.
func dependencyOrder(plans []servicePlan) ([]servicePlan, error) {
	byName := make(map[string]servicePlan, len(plans))
	for _, sp := range plans {
		byName[sp.Name] = sp
	}
	state := map[string]int{}
	var ordered []servicePlan
	var visit func(string) error
	visit = func(name string) error {
		if state[name] == 2 {
			return nil
		}
		if state[name] == 1 {
			return fmt.Errorf("Service dependency cycle at %s", name)
		}
		sp, ok := byName[name]
		if !ok {
			return fmt.Errorf("Service dependency %s missing from saved plan", name)
		}
		state[name] = 1
		for _, dep := range sp.DependsOn {
			if err := visit(dep); err != nil {
				return err
			}
		}
		state[name] = 2
		ordered = append(ordered, sp)
		return nil
	}
	for _, sp := range plans {
		if err := visit(sp.Name); err != nil {
			return nil, err
		}
	}
	return ordered, nil
}

// cutover prepares Services in dependency order, preserving stop-first plain
// replacements. Proxy candidates can satisfy downstream startup dependencies,
// but no route switches until every Service has passed readiness.
func (r *runner) cutover(ctx context.Context, plans []servicePlan) (err error) {
	if err := validateReadinessPlans(plans); err != nil {
		return err
	}
	ordered, err := dependencyOrder(plans)
	if err != nil {
		return err
	}
	var staged []*stagedProxy
	defer func() {
		if err == nil {
			return
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		for i := len(staged) - 1; i >= 0; i-- {
			if !staged[i].switchAttempted {
				r.removeCandidate(cleanup, staged[i])
			}
		}
	}()
	for _, sp := range ordered {
		if sp.proxied() {
			candidate, prepErr := r.prepareProxied(ctx, sp)
			if candidate != nil {
				staged = append(staged, candidate)
			}
			if prepErr != nil {
				return prepErr
			}
		} else {
			r.logf("starting %s", sp.Name)
			if err := r.h.Run(ctx, remote.Cmd{Script: r.cc.cmd("up", "-d", "--no-deps", sp.Name)}); err != nil {
				return fmt.Errorf("compose up %s: %w", sp.Name, err)
			}
			if err := r.waitService(ctx, sp); err != nil {
				return err
			}
		}
	}
	for _, candidate := range staged {
		// From this point only proxySwitchFailed may decide whether removal is safe.
		candidate.switchAttempted = true
		if err := r.switchProxied(ctx, candidate); err != nil {
			return err
		}
	}
	return r.removeOrphans(ctx, plans)
}

type stagedProxy struct {
	plan                servicePlan
	old, fresh, targets []string
	switchAttempted     bool
}

// removeCandidate only receives candidates never offered to a Proxy, or ones
// proxySwitchFailed has established are safe to remove. Cleanup is bounded even
// after cancellation; callers provide an uncancelled, bounded cleanup context
// and must never remove an uncertain switched candidate.
func (r *runner) removeCandidate(ctx context.Context, stage *stagedProxy) {
	if len(stage.fresh) == 0 {
		return
	}
	r.logf("removing new %s container(s), previous version keeps serving", stage.plan.Name)
	if err := r.h.Run(ctx, remote.Cmd{Script: "docker rm -f " + remote.QuoteArgs(stage.fresh...) + " >/dev/null"}); err != nil {
		r.logf("warning: could not remove new containers: %v", err)
	}
}

// prepareProxied adds only the required new containers next to the old ones,
// waits for readiness, and resolves targets without changing Proxy traffic.
func (r *runner) prepareProxied(ctx context.Context, sp servicePlan) (*stagedProxy, error) {
	// Remove stopped Service replicas only, never compose run one-offs.
	// Non-forced docker rm also refuses to delete a replica that starts again
	// between inspect and removal.
	snapshot := "set -eu\nids=$(" + r.cc.containerIDs(sp.Name, true) + ")\n" + `for id in $ids; do
  status=$(docker inspect -f '{{.State.Status}}' "$id")
  case "$status" in exited|dead|created) docker rm "$id" >/dev/null ;; esac
done
` + r.cc.containerIDs(sp.Name, false)
	out, err := r.h.Output(ctx, remote.Cmd{Script: snapshot})
	if err != nil {
		return nil, fmt.Errorf("list containers of %s: %w", sp.Name, err)
	}
	old := lines(out)
	stage := &stagedProxy{plan: sp, old: old}

	scale := len(old) + sp.Replicas
	r.logf("starting new %s container(s) next to %d running", sp.Name, len(old))
	out, err = r.h.Output(ctx, remote.Cmd{Script: "set -e\n" +
		r.cc.cmd("up", "-d", "--no-deps", "--no-recreate", "--scale", sp.Name+"="+strconv.Itoa(scale), sp.Name) + " >&2\n" +
		r.cc.containerIDs(sp.Name, true)})
	if err != nil {
		// Compose may have created only part of the requested replica set
		// before failing or cancellation. The set -e script never reaches ps
		// then, so discover candidates without relying on its partial output.
		discovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		ids, discoverErr := r.h.Output(discovery, remote.Cmd{Script: r.cc.containerIDs(sp.Name, true)})
		if discoverErr != nil {
			r.logf("warning: could not discover new %s containers after startup failed; no unverified containers will be removed: %v", sp.Name, discoverErr)
		} else {
			for _, id := range lines(ids) {
				if !slices.Contains(stage.old, id) {
					stage.fresh = append(stage.fresh, id)
				}
			}
		}
		return stage, fmt.Errorf("start new %s containers: %w", sp.Name, err)
	}
	var fresh []string
	for _, id := range lines(out) {
		if !slices.Contains(old, id) {
			fresh = append(fresh, id)
		}
	}
	stage.fresh = fresh
	if len(fresh) != sp.Replicas {
		return stage, fmt.Errorf("service %s: expected %d new container(s), compose started %d", sp.Name, sp.Replicas, len(fresh))
	}

	r.logf("waiting for %s to become healthy", sp.Name)
	if err := r.h.Run(ctx, remote.Cmd{Script: waitHealthyScript(fresh, sp.Proxy.DeployTimeout)}); err != nil {
		return stage, fmt.Errorf("service %s did not become healthy: %w", sp.Name, err)
	}

	out, err = r.h.Output(ctx, remote.Cmd{Script: "docker inspect -f '{{.Name}}' " + remote.QuoteArgs(fresh...)})
	if err != nil {
		return stage, fmt.Errorf("inspect new %s containers: %w", sp.Name, err)
	}
	var targets []string
	for _, n := range lines(out) {
		targets = append(targets, strings.TrimPrefix(n, "/")+":"+strconv.Itoa(sp.Proxy.Port))
	}
	stage.targets = targets
	return stage, nil
}

func (r *runner) switchProxied(ctx context.Context, stage *stagedProxy) error {
	sp, old, targets := stage.plan, stage.old, stage.targets
	route := RouteName(r.app, r.dest, sp.Name)
	r.logf("switching proxy route %s to %s", route, strings.Join(targets, ","))
	err := proxy.Deploy(ctx, r.h, route, strings.Join(targets, ","), proxy.DeployOptions{
		Hosts:         sp.Proxy.Hosts,
		HealthPath:    sp.Proxy.HealthPath,
		TLS:           sp.Proxy.TLS,
		DeployTimeout: time.Duration(sp.Proxy.DeployTimeout) * time.Second,
		DrainTimeout:  time.Duration(sp.Proxy.DrainTimeout) * time.Second,
	})
	if err != nil {
		return r.proxySwitchFailed(ctx, sp.Name, route, targets, err, func(err error) error {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			r.removeCandidate(cleanup, stage)
			return err
		})
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

// waitService never scales up: Compose has already converged stop-first.
// ps --all includes exited jobs and crashed Services, not just running ones.
func (r *runner) waitService(ctx context.Context, sp servicePlan) error {
	if sp.Replicas == 0 {
		return nil
	}
	completed := sp.CompletedSuccessfully != nil && *sp.CompletedSuccessfully
	r.logf("waiting for Service %s readiness", sp.Name)
	script := "set -eu\nids=$(" + r.cc.containerIDs(sp.Name, true) + ")\nset -- $ids\n" +
		"if [ \"$#\" -ne " + strconv.Itoa(sp.Replicas) + " ]; then echo " +
		remote.Quote("Service "+sp.Name+": expected "+strconv.Itoa(sp.Replicas)+" container(s)") + " >&2; exit 1; fi\n" +
		waitReadyBody(defaultDeployTimeout, completed, true)
	if err := r.h.Run(ctx, remote.Cmd{Script: script, Stdout: r.out}); err != nil {
		return fmt.Errorf("Service %s did not become ready: %w", sp.Name, err)
	}
	return nil
}

// waitHealthyScript shares state handling with plain Services and release
// prerequisites. Proxy deploy_timeout remains the explicit Proxy deadline.
func waitHealthyScript(ids []string, timeoutSec int) string {
	return "set -eu\nset -- " + remote.QuoteArgs(ids...) + "\n" + waitReadyBody(timeoutSec, false, false)
}

// waitReadyBody expects container IDs in positional parameters. Inspect the
// effective container healthcheck, including inherited image settings. Plain
// Services have a finite 30s minimum budget, extended for startup grace plus
// retries+1 complete probes (interval + timeout). No new configuration knob.
func waitReadyBody(timeoutSec int, completed, healthBudget bool) string {
	if timeoutSec <= 0 {
		timeoutSec = defaultDeployTimeout
	}
	budget := ""
	if healthBudget {
		budget = `
# Inspect formatting on older CLIs can also render a missing map key as zero;
# the engine version distinguishes that from an unset supported field.
engine=$(docker version --format '{{.Server.Version}}' 2>/dev/null) || engine=""
major=${engine%%.*}
startup_supported=0
case "$major" in
  ""|*[!0-9]*) ;;
  *) [ "$major" -lt 25 ] || startup_supported=1 ;;
esac
for id do
  enabled=$(docker inspect -f '{{if .State.Health}}yes{{else}}no{{end}}' "$id")
  [ "$enabled" = yes ] || continue
  hc=$(docker inspect -f '{{if .Config.Healthcheck}}{{if .Config.Healthcheck.StartPeriod}}{{json .Config.Healthcheck.StartPeriod}}{{else}}0{{end}} {{if .Config.Healthcheck.Interval}}{{json .Config.Healthcheck.Interval}}{{else}}0{{end}} {{if .Config.Healthcheck.Timeout}}{{json .Config.Healthcheck.Timeout}}{{else}}0{{end}} {{if .Config.Healthcheck.Retries}}{{json .Config.Healthcheck.Retries}}{{else}}0{{end}}{{else}}0 0 0 0{{end}}' "$id")
  read -r grace interval probe retries <<EOF
$hc
EOF
  # Engine defaults for unset timing/retries; round nanoseconds upward.
  grace=$(( (grace + 999999999) / 1000000000 ))
  interval=$(( (interval + 999999999) / 1000000000 ))
  probe=$(( (probe + 999999999) / 1000000000 ))
  [ "$interval" -gt 0 ] || interval=30
  [ "$probe" -gt 0 ] || probe=30
  [ "$retries" -gt 0 ] || retries=3
  # Docker before 25 has no StartInterval field. Other inspection errors
  # still fail through the required full healthcheck/state inspections.
  if startup=$(docker inspect -f '{{if .Config.Healthcheck.StartInterval}}{{json .Config.Healthcheck.StartInterval}}{{else}}0{{end}}' "$id" 2>/dev/null); then
    # Engine 25+ defaults an unset startup probe interval to 5 seconds.
    if [ "$startup_supported" -eq 1 ]; then
      [ "$startup" -gt 0 ] || startup=5000000000
    fi
  else
    startup=0
  fi
  startup=$(( (startup + 999999999) / 1000000000 ))
  [ "$startup" -le "$interval" ] || interval=$startup
  candidate=$(( grace + (retries + 1) * (interval + probe) ))
  [ "$candidate" -le "$budget" ] || budget=$candidate
done
`
	}
	ready := `
      "running healthy"|"running none") ;;
      *" unhealthy"|exited\ *|dead\ *|restarting\ *) echo "container $id: $s" >&2; exit 1 ;;
      *) pending=1 ;;
`
	if completed {
		ready = `
      "exited "*)
        if [ "$code" -ne 0 ]; then echo "job $id exited with status $code" >&2; exit 1; fi ;;
      dead\ *|restarting\ *) echo "job $id: $s" >&2; exit 1 ;;
      *) pending=1 ;;
`
	}
	noHealthLog := ""
	if !completed {
		noHealthLog = `for id do
  health=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$id")
  if [ "$health" = none ]; then echo "container $id: no enabled Docker healthcheck; readiness only guarantees the process is running"; fi
done
`
	}
	return "budget=" + strconv.Itoa(timeoutSec) + budget + `
[ "$#" -gt 0 ] || { echo "no containers to wait for" >&2; exit 1; }
baseline=" "
for id do
  restarts=$(docker inspect -f '{{.RestartCount}}' "$id")
  baseline="$baseline$id:$restarts "
done
` + noHealthLog + `deadline=$(( $(date +%s) + budget ))
while :; do
  pending=0
  for id do
    state=$(docker inspect -f '{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}} {{.State.ExitCode}} {{.RestartCount}}' "$id")
    read -r process health code restarts <<EOF
$state
EOF
    case "$baseline" in
      *" $id:$restarts "*) ;;
      *) echo "container $id restarted during startup" >&2; exit 1 ;;
    esac
    s="$process $health"
    case "$s" in` + ready + `    esac
  done
  [ "$pending" -ne 0 ] || break
  if [ "$(date +%s)" -ge "$deadline" ]; then echo "timeout waiting for Service containers" >&2; exit 1; fi
  sleep 1
done`
}

// removeOrphans stops and removes containers of this compose project whose
// Service is no longer in the compiled file (like --remove-orphans, but
// without asking compose to converge the proxied Services).
func (r *runner) removeOrphans(ctx context.Context, plans []servicePlan) error {
	out, err := r.h.Output(ctx, remote.Cmd{Script: "docker ps -a --filter " + remote.Quote("label=com.docker.compose.project="+r.cc.project) + ` --format '{{.ID}} {{.Label "com.docker.compose.service"}} {{.Label "com.docker.compose.oneoff"}}'`})
	if err != nil {
		return fmt.Errorf("list project containers: %w", err)
	}
	var ids, names []string
	for _, l := range lines(out) {
		fields := strings.Fields(l)
		if len(fields) < 2 || (len(fields) > 2 && (fields[2] == "True" || fields[2] == "true")) {
			continue
		}
		id, svc := fields[0], fields[1]
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
