package deploy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
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
	secretVals         []string
	phase              string
	facts              []ServiceFailure
	diagnosed          bool
	omitDiagnosticText bool // rollback cannot resolve all retained secret values
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
	r.logTo(r.out, format, a...)
}

// logTo writes one redacted step line to w. Concurrent readiness waits use
// their own buffers so lines do not interleave.
func (r *runner) logTo(w io.Writer, format string, a ...any) {
	fmt.Fprintln(w, r.redact(fmt.Sprintf("["+r.server+"] "+format, a...)))
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
	r.phase = "release-prerequisites-validation"
	if err := validateReadinessPlans(plans); err != nil {
		return r.failure(ctx, err, "", nil)
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
	startup := slices.Clone(deps)
	// Compose also starts transitive prerequisites. Record their attempted
	// startup even if Compose fails before the readiness gates can run.
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
	r.logf("starting dependencies %s", strings.Join(startup, ", "))
	r.phase = "release-prerequisites-start"
	for _, name := range deps {
		r.fact(name).Convergence = "prerequisite-start-attempted"
	}
	if err := r.h.Run(ctx, remote.Cmd{Script: r.cc.cmd(append([]string{"up", "-d", "--no-recreate"}, startup...)...)}); err != nil {
		r.logf("deploy failed in phase %s", r.phase)
		r.diagnoseBatch(ctx, deps)
		r.diagnosed = true
		return r.failure(ctx, fmt.Errorf("start dependencies: %w", err), "", nil)
	}
	// Compose has started every prerequisite. Gate them by dependency level,
	// so a prerequisite's deadline starts only after its own dependencies are
	// ready; the Services of one level are waited on concurrently.
	levels, err := dependencyLevels(plans)
	if err != nil {
		return r.failure(ctx, err, "", nil)
	}
	for _, level := range levels {
		var batch []servicePlan
		for _, sp := range level {
			if slices.Contains(deps, sp.Name) {
				batch = append(batch, sp)
			}
		}
		if len(batch) == 0 {
			continue
		}
		r.phase = "release-prerequisites-readiness"
		for _, sp := range batch {
			r.fact(sp.Name).Convergence = "prerequisite-readiness-attempted"
		}
		if err := r.waitServices(ctx, batch, "prerequisite-ready"); err != nil {
			return fmt.Errorf("start dependencies: %w", err)
		}
	}
	return nil
}

// releaseCommands runs each x-yoho.release_command in a one-off container
// from the new image, before Proxy switching. Completed migrations and any
// prerequisite startup are not automatically undone on failure.
func (r *runner) releaseCommands(ctx context.Context, plans []servicePlan) error {
	for _, sp := range plans {
		if len(sp.ReleaseCommand) == 0 {
			continue
		}
		r.phase = "release-command"
		r.logf("release command for %s", sp.Name)
		args := append([]string{"run", "--rm", "--no-deps", "-T", sp.Name}, sp.ReleaseCommand...)
		sink := &cappedBuffer{limit: 4096}
		capture := &logCapture{sink: sink, remaining: 1 << 20, drain: true}
		capture.redact = secrets.NewRedactor(discardOverflow{sink}, r.secretVals...)
		if err := r.h.Run(ctx, remote.Cmd{Script: r.cc.cmd(args...) + " 2>&1", Stdout: capture, Stderr: io.Discard}); err != nil {
			return r.failure(ctx, fmt.Errorf("release command for %s failed: %w\n%s", sp.Name, err, sink.String()), "", nil)
		}
		if !capture.cut {
			_ = capture.redact.Flush()
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

// dependencyLevels groups the saved plan by depends_on depth: level 0 has no
// dependencies, and every other Service sits one level above its deepest
// dependency. Services of one level never depend on each other. Within a
// level, Services keep dependencyOrder's order, which stays the validator.
func dependencyLevels(plans []servicePlan) ([][]servicePlan, error) {
	ordered, err := dependencyOrder(plans)
	if err != nil {
		return nil, err
	}
	depth := make(map[string]int, len(ordered))
	var levels [][]servicePlan
	for _, sp := range ordered {
		// dependencyOrder places every dependency before its dependants.
		d := 0
		for _, dep := range sp.DependsOn {
			d = max(d, depth[dep]+1)
		}
		depth[sp.Name] = d
		for len(levels) <= d {
			levels = append(levels, nil)
		}
		levels[d] = append(levels[d], sp)
	}
	return levels, nil
}

// cutover prepares Services level by level in dependency order, preserving
// stop-first plain replacements. The plain Services of one level start with
// one `compose up` and are gated concurrently; proxied Services of the level
// then stage their candidates one at a time. Proxy candidates can satisfy
// downstream startup dependencies, but no route switches until every Service
// has passed readiness.
func (r *runner) cutover(ctx context.Context, plans []servicePlan) (err error) {
	r.phase = "service-plan-validation"
	if err := validateReadinessPlans(plans); err != nil {
		return r.failure(ctx, err, "", nil)
	}
	levels, err := dependencyLevels(plans)
	if err != nil {
		return err
	}
	var staged []*stagedProxy
	var failedService string
	for _, sp := range plans {
		f := r.fact(sp.Name)
		if sp.proxied() {
			f.ProxyRoute = "not-attempted"
		}
	}
	defer func() {
		if err == nil {
			return
		}
		if !r.diagnosed {
			var fresh []string
			for _, candidate := range staged {
				if candidate.plan.Name == failedService {
					fresh = candidate.fresh
					break
				}
			}
			err = r.failure(ctx, err, failedService, fresh)
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		for i := len(staged) - 1; i >= 0; i-- {
			if !staged[i].switchAttempted {
				r.removeCandidate(cleanup, staged[i])
			}
		}
	}()
	for i, level := range levels {
		start := time.Now()
		var batch []servicePlan
		for _, sp := range level {
			if !sp.proxied() {
				batch = append(batch, sp)
			}
		}
		if len(batch) > 0 {
			if err := r.startBatch(ctx, batch, i, len(levels)); err != nil {
				failedService = ""
				if len(batch) == 1 {
					failedService = batch[0].Name
				}
				return err
			}
		}
		for _, sp := range level {
			if !sp.proxied() {
				continue
			}
			failedService = sp.Name
			r.phase = "service-prepare"
			r.fact(sp.Name).Convergence = "attempted"
			candidate, prepErr := r.prepareProxied(ctx, sp)
			if candidate != nil {
				staged = append(staged, candidate)
			}
			if prepErr != nil {
				var ids []string
				if candidate != nil {
					ids = candidate.fresh
				}
				return r.failure(ctx, prepErr, sp.Name, ids)
			}
			r.fact(sp.Name).Convergence = "complete"
		}
		if len(levels) > 1 || len(level) > 1 {
			r.logf("level %d/%d ready in %s", i+1, len(levels), time.Since(start).Round(100*time.Millisecond))
		}
	}
	for _, candidate := range staged {
		failedService = candidate.plan.Name
		r.phase = "proxy-switch"
		r.fact(failedService).ProxyRoute = "uncertain"
		// From this point only proxySwitchFailed may decide whether removal is safe.
		candidate.switchAttempted = true
		if err := r.switchProxied(ctx, candidate); err != nil {
			return err
		}
		r.fact(failedService).ProxyRoute = "switched"
	}
	r.phase = "orphan-cleanup"
	failedService = ""
	return r.removeOrphans(ctx, plans)
}

// startBatch converges the plain Services of one dependency level stop-first
// with one `compose up`, then waits for their readiness concurrently. A
// single-Service level runs the same commands as a sequential start.
func (r *runner) startBatch(ctx context.Context, batch []servicePlan, level, levels int) error {
	r.phase = "service-prepare"
	names := make([]string, len(batch))
	for i, sp := range batch {
		names[i] = sp.Name
		r.fact(sp.Name).Convergence = "attempted"
	}
	if len(batch) == 1 {
		r.logf("starting %s", names[0])
	} else {
		r.logf("starting level %d/%d: %s", level+1, levels, strings.Join(names, ", "))
	}
	if err := r.h.Run(ctx, remote.Cmd{Script: r.cc.cmd(append([]string{"up", "-d", "--no-deps"}, names...)...)}); err != nil {
		err = fmt.Errorf("compose up %s: %w", strings.Join(names, " "), err)
		if len(batch) == 1 {
			return err // cutover diagnoses the one Service
		}
		// Compose does not say which Service failed: sample them all.
		r.logf("deploy failed in phase %s", r.phase)
		r.diagnoseBatch(ctx, names)
		r.diagnosed = true
		return r.failure(ctx, err, "", nil)
	}
	r.phase = "plain-readiness"
	return r.waitServices(ctx, batch, "complete")
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
	r.logf("removing unswitched new %s container(s); other Services may already have changed", stage.plan.Name)
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

	r.phase = "proxy-readiness"
	r.logf("waiting for %s to become healthy", sp.Name)
	if err := r.h.Run(ctx, remote.Cmd{Script: waitHealthyScript(fresh, sp.Proxy.DeployTimeout)}); err != nil {
		return stage, fmt.Errorf("service %s did not become healthy: %w", sp.Name, err)
	}

	r.phase = "proxy-targets"
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
	opts, err := proxy.ServiceDeployOptions(*sp.Proxy)
	if err != nil {
		return fmt.Errorf("service %s: %w", sp.Name, err)
	}
	err = proxy.Deploy(ctx, r.h, route, strings.Join(targets, ","), opts)
	if err != nil {
		return r.proxySwitchFailed(ctx, sp.Name, route, targets, err, func(err error) error {
			err = r.failure(ctx, err, sp.Name, stage.fresh)
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
	found := false
	for _, rt := range routes {
		if rt.Service != route {
			continue
		}
		found = true
		for _, t := range strings.Split(rt.Target, ",") {
			switched = switched || slices.Contains(targets, strings.TrimSpace(t))
		}
	}
	if lerr == nil && !switched && ctx.Err() == nil {
		r.fact(svc).ProxyRoute = "retained"
		if !found {
			r.fact(svc).ProxyRoute = "absent"
		}
		return fail(err)
	}
	r.fact(svc).ProxyRoute = "uncertain"
	r.logf("warning: proxy route %s may already target the new %s container(s); old and new containers are left running", route, svc)
	return fmt.Errorf("%w (the new %s containers were kept because the proxy route may use them; run the deploy again to finish)", err, svc)
}

// waitService never scales up: Compose has already converged stop-first.
func (r *runner) waitService(ctx context.Context, sp servicePlan) error {
	if err := r.readiness(ctx, sp, r.out); err != nil {
		return r.failure(ctx, err, sp.Name, nil)
	}
	return nil
}

// waitServices gates Services that do not depend on each other concurrently
// and sets each ready one's convergence fact to ready. Each wait writes to its
// own buffer, flushed in Service order, so output does not interleave. The
// first failure cancels the other waits and is diagnosed for its Service;
// only this goroutine touches facts and diagnostics.
func (r *runner) waitServices(ctx context.Context, batch []servicePlan, ready string) error {
	if len(batch) == 1 {
		if err := r.waitService(ctx, batch[0]); err != nil {
			return err
		}
		r.fact(batch[0].Name).Convergence = ready
		return nil
	}
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	outs := make([]*cappedBuffer, len(batch))
	errs := make([]error, len(batch))
	failed := -1
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i, sp := range batch {
		outs[i] = &cappedBuffer{limit: 65536}
		wg.Go(func() {
			errs[i] = r.readiness(waitCtx, sp, discardOverflow{outs[i]})
			if errs[i] != nil {
				mu.Lock()
				if failed < 0 {
					failed = i
					cancel()
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	for i, sp := range batch {
		_, _ = r.out.Write(outs[i].Bytes())
		if errs[i] == nil {
			r.fact(sp.Name).Convergence = ready
		}
	}
	if failed >= 0 {
		return r.failure(ctx, errs[failed], batch[failed].Name, nil)
	}
	return nil
}

// readiness runs one Service's readiness gate, logging to out. ps --all
// includes exited jobs and crashed Services, not just running ones.
func (r *runner) readiness(ctx context.Context, sp servicePlan, out io.Writer) error {
	if sp.Replicas == 0 {
		return nil
	}
	completed := sp.CompletedSuccessfully != nil && *sp.CompletedSuccessfully
	r.logTo(out, "waiting for Service %s readiness", sp.Name)
	script := "set -eu\nids=$(" + r.cc.containerIDs(sp.Name, true) + ")\nset -- $ids\n" +
		"if [ \"$#\" -ne " + strconv.Itoa(sp.Replicas) + " ]; then echo " +
		remote.Quote("Service "+sp.Name+": expected "+strconv.Itoa(sp.Replicas)+" container(s)") + " >&2; exit 1; fi\n" +
		waitReadyBody(defaultDeployTimeout, completed, true)
	if err := r.h.Run(ctx, remote.Cmd{Script: script, Stdout: out}); err != nil {
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
