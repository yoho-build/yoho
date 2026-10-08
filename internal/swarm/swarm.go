// Package swarm is the Swarm Runtime (ADR 0007): it deploys an App as a
// `docker stack deploy` stack from the manager (the Destination's first
// Server). Swarm's start-first rolling update owns the cutover; the Proxy
// routes to the stable service VIP (or to tasks with x-yoho.strict_drain).
//
// Differences from the compose runtime:
//   - secrets are Swarm secrets named <stack>_<NAME>_<hash8> (immutable,
//     content-addressed), mounted at /run/secrets/<NAME> with <NAME>_FILE;
//   - Stateful Services update stop-first and are pinned with a
//     node.hostname placement constraint to the first Server;
//   - release_command runs as a one-off replicated-job service in a separate
//     stack (<stack>-release) that joins the main stack's networks, volumes
//     and secrets, so it sees exactly what the Service will;
//   - the Proxy (kamal-proxy) runs on every node. The attachable overlay
//     network `yoho` is cluster-wide; each node's `yoho-proxy` container
//     joins it and routes to the service VIP, so DNS can point at any node.
package swarm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/proxy"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/secrets"
)

// Runtime implements plan.Runtime with Docker Swarm.
type Runtime struct{}

var _ plan.Runtime = Runtime{}

const defaultRetain = 5

// Replaceable in tests.
var (
	now             = func() time.Time { return time.Now().UTC() }
	pollInterval    = 2 * time.Second
	convergeTimeout = 15 * time.Minute
	releaseTimeout  = 30 * time.Minute
)

// stackLabel is set by docker stack deploy on every object of a stack.
const stackLabel = "com.docker.stack.namespace"

// ReleaseStack is the stack running release_command jobs of stack.
func ReleaseStack(stack string) string { return stack + "-release" }

type runner struct {
	h         remote.Host
	out       io.Writer
	server    string
	app, dest string
	stack     string
	dir       string // App Destination dir on the manager
	// secretVals are the secret values of this run; command output and task
	// errors embedded in returned errors are redacted with them (d.Out is
	// already redacted, errors are not).
	secretVals []string
}

func (r *runner) setSecrets(svcSecrets map[string]map[string]string) {
	for _, m := range svcSecrets {
		for _, v := range m {
			r.secretVals = append(r.secretVals, v)
		}
	}
}

// redact masks secret values in remote output before it joins an error.
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

func newRunner(d *plan.Deploy) *runner {
	out := d.Out
	if out == nil {
		out = io.Discard
	}
	return &runner{
		h: d.Servers[0].Host, out: out, server: d.Servers[0].Name, app: d.App, dest: d.Destination,
		stack: release.ProjectName(d.App, d.Destination), dir: release.AppDir(d.App, d.Destination),
	}
}

func (r *runner) logf(format string, a ...any) {
	fmt.Fprintf(r.out, "["+r.server+"] "+format+"\n", a...)
}

func hook(ctx context.Context, d *plan.Deploy, name string, extra map[string]string) error {
	if d.Hook == nil {
		return nil
	}
	var hs []string
	for _, s := range d.Servers {
		_, h, _, err := remote.ParseTarget(s.Server.SSH)
		if err != nil || h == "" {
			h = s.Name
		}
		hs = append(hs, h)
	}
	env := map[string]string{
		"YOHO_APP":             d.App,
		"YOHO_DESTINATION":     d.Destination,
		"YOHO_VERSION":         d.Version,
		"YOHO_SERVICE_VERSION": d.App + "@" + d.Version,
		"YOHO_PERFORMER":       d.Performer,
		"YOHO_HOSTS":           strings.Join(hs, ","),
	}
	for k, v := range extra {
		env[k] = v
	}
	return d.Hook(ctx, name, env)
}

// checkSwarm verifies the first Server is a Swarm manager and the others are
// Swarm nodes; it returns the manager's node hostname.
func checkSwarm(ctx context.Context, d *plan.Deploy) (string, error) {
	mgr := d.Servers[0]
	out, err := mgr.Host.Output(ctx, remote.Cmd{Script: "docker info -f '{{.Swarm.LocalNodeState}}|{{.Swarm.ControlAvailable}}|{{.Name}}'"})
	if err != nil {
		return "", fmt.Errorf("docker info on %s: %w", mgr.Name, err)
	}
	f := strings.Split(strings.TrimSpace(out), "|")
	if len(f) != 3 || f[0] != "active" || f[1] != "true" {
		return "", fmt.Errorf("server %s is not a Swarm manager (state %q); run `yoho swarm init` first", mgr.Name, strings.TrimSpace(out))
	}
	for _, s := range d.Servers[1:] {
		st, err := s.Host.Output(ctx, remote.Cmd{Script: "docker info -f '{{.Swarm.LocalNodeState}}'"})
		if err != nil {
			return "", fmt.Errorf("docker info on %s: %w", s.Name, err)
		}
		if strings.TrimSpace(st) != "active" {
			return "", fmt.Errorf("server %s has not joined the Swarm (state %q); run `yoho swarm join`", s.Name, strings.TrimSpace(st))
		}
	}
	return f[2], nil
}

// Deploy implements plan.Runtime.
func (Runtime) Deploy(ctx context.Context, d *plan.Deploy) (_ *release.Release, err error) {
	if err := validate(d); err != nil {
		return nil, err
	}
	start := now()
	mgr := d.Servers[0]
	h := mgr.Host
	r := newRunner(d)
	relDir := release.Dir(d.App, d.Destination, d.Version)
	stackFile := path.Join(relDir, "compose.yaml")

	unlock, err := deploy.AcquireLock(ctx, h, d.App, d.Destination, deploy.LockInfo{Performer: d.Performer, Version: d.Version, Command: "deploy", Time: start})
	if err != nil {
		return nil, err
	}
	defer func() {
		if uerr := unlock(ctx); uerr != nil && err == nil {
			err = uerr
		}
	}()

	pinHost, err := checkSwarm(ctx, d)
	if err != nil {
		return nil, err
	}

	r.logf("preparing release %s", d.Version)
	if err := h.Run(ctx, remote.Cmd{Script: "set -eu\numask 077\nmkdir -p " + remote.QuoteArgs(r.dir, path.Join(r.dir, "releases"), path.Join(r.dir, "secrets"), path.Join(r.dir, "generated"), relDir)}); err != nil {
		return nil, fmt.Errorf("prepare %s: %w", r.dir, err)
	}
	key, err := deploy.EnsureHMACKey(ctx, h, r.dir)
	if err != nil {
		return nil, err
	}
	kinds, err := deploy.GeneratedDecls(d)
	if err != nil {
		return nil, err
	}
	generated, err := deploy.EnsureGenerated(ctx, h, r.dir, kinds)
	if err != nil {
		return nil, err
	}
	svcSecrets, err := deploy.ServiceSecrets(d, generated, r.logf)
	if err != nil {
		return nil, err
	}
	r.setSecrets(svcSecrets)
	generation := start.Format("20060102T150405Z") + "-" + d.Version
	genDir := release.SecretsDir(d.App, d.Destination, generation)

	c, err := compile(d, compileInput{PinHost: pinHost, GenerationDir: genDir, SvcSecrets: svcSecrets, HMACKey: key, DockerVersion: deploy.DockerServerVersion(ctx, h)})
	if err != nil {
		return nil, err
	}
	for _, w := range c.Warnings {
		r.logf("warning: %s", w)
	}
	if err := r.writeEnvFiles(ctx, d, genDir, svcSecrets); err != nil {
		return nil, err
	}
	if err := r.createSecrets(ctx, d, svcSecrets, key); err != nil {
		return nil, err
	}
	// Redeploying the Version `current` points at reuses the Release
	// directory. Keep the deployed record to restore if this attempt fails,
	// so plan and rollback keep comparing against what actually runs. Swarm
	// rolls back only the Services whose update failed; once the stack was
	// touched, restoreStack puts the others back on the recorded spec too.
	prevFiles := deploy.SnapshotRelease(ctx, h, relDir)
	committed, stackTouched := false, false
	if err := h.WriteFile(ctx, stackFile, c.YAML, 0o600, false); err != nil {
		deploy.RestoreRelease(ctx, h, relDir, prevFiles, r.logf)
		return nil, fmt.Errorf("write stack file: %w", err)
	}
	if err := deploy.WriteJSON(ctx, h, path.Join(relDir, "plan.json"), c.Plan); err != nil {
		deploy.RestoreRelease(ctx, h, relDir, prevFiles, r.logf)
		return nil, err
	}

	rel := &release.Release{
		App: d.App, Destination: d.Destination, Server: mgr.Name, Version: d.Version,
		Runtime: "swarm", Role: "all", DeployedAt: start, Performer: d.Performer,
		Images: map[string]string{}, SecretsGeneration: generation,
		Secrets:       deploy.SecretAudit(d, svcSecrets, generated, key),
		ComposeSHA256: deploy.SHA256Hex(c.YAML), Status: "failed",
	}
	for _, sp := range c.Plan.Services {
		if sp.Image != "" {
			rel.Images[sp.Name] = sp.Image
		}
	}
	defer func() {
		switch {
		case err == nil || committed:
		case prevFiles != nil:
			restored := deploy.RestoreRelease(ctx, h, relDir, prevFiles, r.logf)
			if stackTouched {
				r.restoreStack(context.WithoutCancel(ctx), d, relDir, restored, c.Plan)
			}
		default:
			_ = deploy.WriteJSON(context.WithoutCancel(ctx), h, path.Join(relDir, "release.json"), rel)
		}
	}()

	if err := hook(ctx, d, "pre-deploy", nil); err != nil {
		return nil, err
	}
	if c.Plan.needsProxy() {
		if err := r.bootProxy(ctx, d); err != nil {
			return nil, err
		}
	}
	auth := registryAuth(d, c.Plan)
	stackTouched = true // release_command may deploy dependencies first
	if err := r.releaseCommands(ctx, c, relDir, auth); err != nil {
		return nil, err
	}
	r.logf("deploying stack %s", r.stack)
	if err := r.deployStack(ctx, stackFile, r.stack, true, auth); err != nil {
		return nil, err
	}
	if err := r.routes(ctx, d, c.Plan); err != nil {
		return nil, err
	}

	rel.Status = "deployed"
	if err := r.finish(ctx, d, rel); err != nil {
		return nil, err
	}
	committed = true
	secs := int(now().Sub(start).Round(time.Second) / time.Second)
	if err := hook(ctx, d, "post-deploy", map[string]string{"YOHO_RUNTIME": strconv.Itoa(secs)}); err != nil {
		return rel, err
	}
	r.logf("deployed %s in %ds", d.Version, secs)
	return rel, nil
}

// partialMarker in a Release directory says the live stack may not match
// the Release's record: a failed redeploy of that Version updated some
// Services and putting them back failed. Plan then treats every Service as
// changed; a successful deploy or rollback of the Version removes it.
const partialMarker = "partial"

// restoreStack re-deploys the restored record of relDir after a failed
// same-Version redeploy touched the stack, so Services whose update already
// converged do not keep a spec the record does not describe. Routes follow
// the record and routes only the attempt added are removed. When the record
// was not fully restored or the re-deploy fails, partialMarker is left
// instead of claiming a state the stack is not in.
func (r *runner) restoreStack(ctx context.Context, d *plan.Deploy, relDir string, restored bool, attempted stackPlan) {
	marker := path.Join(relDir, partialMarker)
	err := errors.New("the previous record could not be restored")
	if restored {
		err = func() error {
			p, err := readPlan(ctx, r.h, relDir)
			if err != nil {
				return err
			}
			r.logf("restoring the stack to the record of Release %s", path.Base(relDir))
			if err := r.deployStack(ctx, path.Join(relDir, "compose.yaml"), r.stack, true, registryAuth(d, *p)); err != nil {
				return err
			}
			if err := r.routes(ctx, d, *p); err != nil {
				return err
			}
			return r.removeRoutes(ctx, d, attempted, *p)
		}()
	}
	if err == nil {
		if rerr := r.h.Run(ctx, remote.Cmd{Script: "rm -f " + remote.Quote(marker)}); rerr != nil {
			r.logf("warning: could not clear %s: %v", marker, rerr)
		}
		return
	}
	r.logf("warning: the stack may not match the record of Release %s (%v); the next plan treats every Service as changed", path.Base(relDir), err)
	if werr := r.h.WriteFile(ctx, marker, []byte("a failed redeploy left the stack partly updated\n"), 0o600, false); werr != nil {
		r.logf("warning: could not write %s: %v", marker, werr)
	}
}

// writeEnvFiles writes <generation>/<service>.env (0600) for Services with
// x-yoho.secrets_as_env. docker stack deploy reads them on the manager and
// puts the values into the service spec (visible to `docker service
// inspect`), like env_file does with compose.
func (r *runner) writeEnvFiles(ctx context.Context, d *plan.Deploy, genDir string, svcSecrets map[string]map[string]string) error {
	for _, svc := range sortedKeys(svcSecrets) {
		if !d.Ext[svc].SecretsAsEnv {
			continue
		}
		if err := r.h.WriteFile(ctx, path.Join(genDir, svc+".env"), deploy.EnvFile(svcSecrets[svc]), 0o600, false); err != nil {
			return fmt.Errorf("write secrets for %s: %w", svc, err)
		}
	}
	return nil
}

// createSecrets creates the Swarm secrets the stack references (values on
// stdin). Existing ones are kept: names are content-addressed.
func (r *runner) createSecrets(ctx context.Context, d *plan.Deploy, svcSecrets map[string]map[string]string, key []byte) error {
	done := map[string]bool{}
	for _, svc := range sortedKeys(svcSecrets) {
		if d.Ext[svc].SecretsAsEnv {
			continue
		}
		m := svcSecrets[svc]
		for _, name := range sortedKeys(m) {
			sn := SecretName(r.stack, name, m[name], key)
			if done[sn] {
				continue
			}
			done[sn] = true
			q := remote.Quote(sn)
			script := "set -eu\nif ! docker secret inspect " + q + " >/dev/null 2>&1; then\n  docker secret create " +
				remote.QuoteArgs("--label", deploy.LabelApp+"="+d.App, "--label", deploy.LabelDestination+"="+d.Destination, sn, "-") +
				" >/dev/null\nfi"
			if err := r.h.Run(ctx, remote.Cmd{Script: script, Stdin: strings.NewReader(m[name])}); err != nil {
				return fmt.Errorf("create swarm secret %s: %w", sn, err)
			}
		}
	}
	return nil
}

// bootProxy ensures the attachable overlay network `yoho` (cluster-wide, so
// both kamal-proxy containers and Swarm tasks can join it) and boots the
// Proxy on every Server. DNS or a Cloudflare Tunnel can point at any node.
func (r *runner) bootProxy(ctx context.Context, d *plan.Deploy) error {
	script := `set -eu
s=$(docker network inspect -f '{{.Driver}}|{{.Scope}}|{{.Attachable}}' ` + proxy.Network + ` 2>/dev/null || true)
case "$s" in
  "") docker network create -d overlay --attachable ` + proxy.Network + ` >/dev/null ;;
  "overlay|swarm|true") ;;
  *) echo "$s"; exit 3 ;;
esac`
	if out, err := r.h.Output(ctx, remote.Cmd{Script: script}); err != nil {
		var ee *remote.ExitError
		if errors.As(err, &ee) && ee.Code == 3 {
			return fmt.Errorf("docker network %s exists but is not an attachable overlay (driver|scope|attachable = %s), probably from the compose runtime; "+
				"switching a Server to swarm needs downtime: remove the compose Apps' proxied containers and %s, run `docker network rm %s`, then deploy again (ADR 0007)",
				proxy.Network, strings.TrimSpace(out), proxy.ContainerName, proxy.Network)
		}
		return fmt.Errorf("ensure overlay network %s: %w", proxy.Network, err)
	}
	for _, s := range d.Servers {
		if err := r.removeStaleBridge(ctx, s); err != nil {
			return err
		}
		// Never let the proxy create `yoho` here: on a worker that would be a
		// local bridge instead of the manager's overlay.
		if err := proxy.BootWith(ctx, s.Host, d.Proxy, r.out, proxy.BootOptions{SkipNetwork: true}); err != nil {
			return fmt.Errorf("boot proxy on %s: %w", s.Name, err)
		}
	}
	return nil
}

// removeStaleBridge removes a node-local, non-overlay `yoho` network (left by
// the compose runtime, or created by an older Yoho on a worker), which would
// shadow the cluster overlay and reject Swarm tasks. Only the Proxy may be
// attached; it is removed too and recreated by the proxy boot (its routes
// live in the state volume). Other containers need the manual migration.
func (r *runner) removeStaleBridge(ctx context.Context, s plan.NamedHost) error {
	script := `set -eu
s=$(docker network inspect -f '{{.Driver}}|{{.Scope}}' ` + proxy.Network + ` 2>/dev/null || true)
case "$s" in
  *"|local") ;;
  *) exit 0 ;;
esac
names=$(docker network inspect -f '{{range .Containers}}{{.Name}} {{end}}' ` + proxy.Network + `)
others=""
proxy_attached=""
for n in $names; do
  if [ "$n" = ` + proxy.ContainerName + ` ]; then proxy_attached=1; else others="$others $n"; fi
done
if [ -n "$others" ]; then echo "$s:$others"; exit 3; fi
if [ -n "$proxy_attached" ]; then docker rm -f ` + proxy.ContainerName + ` >/dev/null; fi
docker network rm ` + proxy.Network + ` >/dev/null
echo "$s"`
	out, err := s.Host.Output(ctx, remote.Cmd{Script: script})
	if err != nil {
		var ee *remote.ExitError
		if errors.As(err, &ee) && ee.Code == 3 {
			return fmt.Errorf("%s has a node-local docker network %s (%s) that Swarm tasks cannot use, probably from the compose runtime; "+
				"switching a Server to swarm needs downtime: remove the compose Apps' proxied containers and %s, run `docker network rm %s` on it, then deploy again (ADR 0007)",
				s.Name, proxy.Network, strings.TrimSpace(out), proxy.ContainerName, proxy.Network)
		}
		return fmt.Errorf("check docker network %s on %s: %w", proxy.Network, s.Name, err)
	}
	if out = strings.TrimSpace(out); out != "" {
		fmt.Fprintf(r.out, "[%s] warning: removed node-local docker network %s (%s) so the Swarm overlay is used; the proxy is recreated (brief proxy downtime)\n", s.Name, proxy.Network, out)
	}
	return nil
}

// registryAuth reports whether `docker stack deploy` should pass
// --with-registry-auth: the Destination configures a registry, or an image
// name's first path component looks like a registry host. Yoho ships images
// to each node and does not run docker login; when nodes must pull, the
// manager is expected to be logged in already.
func registryAuth(d *plan.Deploy, p stackPlan) bool {
	if d.Registry != nil {
		return true
	}
	for _, sp := range p.Services {
		first, _, ok := strings.Cut(sp.Image, "/")
		if ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
			return true
		}
	}
	return false
}

func stackDeployCmd(dir, file, stack string, prune, auth, wait bool) string {
	args := []string{"--compose-file", file, "--resolve-image", "never"}
	if prune {
		args = append(args, "--prune")
	}
	if auth {
		args = append(args, "--with-registry-auth")
	}
	args = append(args, "--detach="+strconv.FormatBool(!wait), stack)
	return "cd " + remote.Quote(dir) + " && docker stack deploy " + remote.QuoteArgs(args...)
}

// deployStack runs docker stack deploy (waiting for convergence) and then
// verifies every Service it touched converged without a rollback.
func (r *runner) deployStack(ctx context.Context, file, stack string, prune, auth bool) error {
	before, err := r.serviceStatus(ctx, stack)
	if err != nil {
		return err
	}
	dctx, cancel := context.WithTimeout(ctx, convergeTimeout)
	defer cancel()
	var buf bytes.Buffer
	if err := r.h.Run(dctx, remote.Cmd{Script: stackDeployCmd(r.dir, file, stack, prune, auth, true) + " 2>&1", Stdout: &buf}); err != nil {
		return fmt.Errorf("docker stack deploy %s: %w\n%s%s", stack, err, r.redact(tailLines(buf.String(), 15)), r.diagnose(context.WithoutCancel(ctx), stack, nil))
	}
	return r.waitConverged(ctx, stack, before)
}

// svcStatus is one Service's state as seen by `docker service inspect/ls`.
type svcStatus struct {
	UpdateStarted string // UpdateStatus.StartedAt, identifies an update
	UpdateState   string
	UpdateMessage string
	Replicas      string // e.g. "2/2", "0/1 (1/1 completed)"
}

func (r *runner) serviceStatus(ctx context.Context, stack string) (map[string]svcStatus, error) {
	f := remote.Quote("label=" + stackLabel + "=" + stack)
	script := "ids=$(docker service ls -q --filter " + f + ")\n" +
		`[ -z "$ids" ] || docker service inspect --format 'S|{{.Spec.Name}}|{{with .UpdateStatus}}{{.StartedAt}}|{{.State}}|{{.Message}}{{end}}' $ids` + "\n" +
		"docker service ls --filter " + f + " --format 'R|{{.Name}}|{{.Replicas}}'"
	out, err := r.h.Output(ctx, remote.Cmd{Script: script})
	if err != nil {
		return nil, fmt.Errorf("inspect services of %s: %w", stack, err)
	}
	return parseStatus(out), nil
}

func parseStatus(out string) map[string]svcStatus {
	m := map[string]svcStatus{}
	for _, l := range lines(out) {
		f := strings.Split(l, "|")
		switch {
		case f[0] == "S" && len(f) >= 2:
			s := m[f[1]]
			if len(f) >= 5 {
				s.UpdateStarted, s.UpdateState, s.UpdateMessage = f[2], f[3], strings.Join(f[4:], "|")
			}
			m[f[1]] = s
		case f[0] == "R" && len(f) == 3:
			s := m[f[1]]
			s.Replicas = f[2]
			m[f[1]] = s
		}
	}
	return m
}

// replicasReady parses `docker service ls` replicas: "running/desired",
// optionally followed by "(done/total completed)" for jobs. A job is ready
// only when every task has completed ("0/1 (0/1 completed)" is still
// running, or failed).
func replicasReady(s string) bool {
	head, rest, _ := strings.Cut(s, " ")
	if strings.Contains(rest, "completed") {
		inner := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(rest), "("), ")"))
		inner = strings.TrimSpace(strings.TrimSuffix(inner, "completed"))
		done, total, ok := strings.Cut(inner, "/")
		return ok && done == total && total != "" && total != "0"
	}
	run, want, ok := strings.Cut(head, "/")
	return ok && run == want
}

// converged classifies Services against the snapshot taken before the
// deploy: only Services this deploy created or updated are checked, so a
// stale rollback status from an earlier failed deploy is not blamed on it.
func converged(before, after map[string]svcStatus) (pending, failed []string) {
	for _, name := range sortedKeys(after) {
		a := after[name]
		b, existed := before[name]
		updated := a.UpdateStarted != "" && (!existed || a.UpdateStarted != b.UpdateStarted)
		if existed && !updated {
			continue // spec unchanged: Swarm did not touch its tasks
		}
		switch a.UpdateState {
		case "rollback_started", "rollback_paused", "rollback_completed", "paused":
			if updated {
				failed = append(failed, name)
				continue
			}
		case "updating":
			pending = append(pending, name)
			continue
		}
		if !replicasReady(a.Replicas) {
			pending = append(pending, name)
		}
	}
	return pending, failed
}

func (r *runner) waitConverged(ctx context.Context, stack string, before map[string]svcStatus) error {
	deadline := now().Add(convergeTimeout)
	for {
		after, err := r.serviceStatus(ctx, stack)
		if err != nil {
			return err
		}
		pending, failed := converged(before, after)
		if len(failed) > 0 {
			var msgs []string
			for _, n := range failed {
				msgs = append(msgs, r.redact(n+": "+after[n].UpdateState+" "+after[n].UpdateMessage))
			}
			return fmt.Errorf("update failed and Swarm rolled back (the previous version keeps serving):\n%s%s", strings.Join(msgs, "\n"), r.diagnose(ctx, stack, failed))
		}
		if len(pending) == 0 {
			return nil
		}
		if now().After(deadline) {
			return fmt.Errorf("services did not converge within %s: %s%s", convergeTimeout, strings.Join(pending, ", "), r.diagnose(ctx, stack, pending))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// diagnose returns recent task errors (`docker service ps --no-trunc`) for
// services (all of the stack when empty). Best effort.
func (r *runner) diagnose(ctx context.Context, stack string, services []string) string {
	sel := remote.QuoteArgs(services...)
	if len(services) == 0 {
		sel = "$(docker service ls -q --filter " + remote.Quote("label="+stackLabel+"="+stack) + ")"
	}
	out, err := r.h.Output(ctx, remote.Cmd{Script: "for s in " + sel + "; do docker service ps --no-trunc --format '{{.Name}} on {{.Node}}: {{.CurrentState}} {{.Error}}' \"$s\" 2>/dev/null | head -n 5; done; true"})
	if err != nil || strings.TrimSpace(out) == "" {
		return ""
	}
	return "\ntasks (docker service ps --no-trunc):\n" + r.redact(out)
}

// routes points each proxied Service's Proxy route at its stable Swarm name
// (VIP, or tasks.<name> with strict_drain). kamal-proxy deploy is
// idempotent and health-gated; the rolling update already did the cutover.
//
// VIP mode may still route a few requests to a stopping task (moby#38841),
// so Apps should drain on SIGTERM. strict_drain uses endpoint_mode dnsrr:
// kamal-proxy resolves tasks.<service> per new connection, so a removed task
// stops receiving new connections at once; keep-alive connections to it
// last until the task closes them, and DNS results may be cached briefly.
func (r *runner) routes(ctx context.Context, d *plan.Deploy, p stackPlan) error {
	for _, sp := range p.Services {
		if !sp.proxied() {
			continue
		}
		target := p.Stack + "_" + sp.Name
		if sp.StrictDrain {
			target = "tasks." + target
		}
		target += ":" + strconv.Itoa(sp.Proxy.Port)
		route := deploy.RouteName(d.App, d.Destination, sp.Name)
		r.logf("proxy route %s -> %s on %d servers", route, target, len(d.Servers))
		opts := proxy.DeployOptions{
			Hosts: sp.Proxy.Hosts, HealthPath: sp.Proxy.HealthPath, TLS: sp.Proxy.TLS,
			DeployTimeout: time.Duration(sp.Proxy.DeployTimeout) * time.Second,
			DrainTimeout:  time.Duration(sp.Proxy.DrainTimeout) * time.Second,
		}
		for _, s := range d.Servers {
			if err := proxy.Deploy(ctx, s.Host, route, target, opts); err != nil {
				return fmt.Errorf("proxy route %s on %s: %w", route, s.Name, err)
			}
		}
	}
	return r.removeStaleRoutes(ctx, d, p)
}

// removeStaleRoutes drops Proxy routes that the previous Release had and
// this plan does not, on every node. kamal-proxy keeps a route until it is
// removed; stack prune does not touch the Proxy.
func (r *runner) removeStaleRoutes(ctx context.Context, d *plan.Deploy, p stackPlan) error {
	prev, err := currentPlan(ctx, r.h, d.App, d.Destination)
	if err != nil || prev == nil {
		return err
	}
	return r.removeRoutes(ctx, d, *prev, p)
}

// removeRoutes drops the routes of prev's x-yoho.proxy Services that p does
// not route.
func (r *runner) removeRoutes(ctx context.Context, d *plan.Deploy, prev, p stackPlan) error {
	keep := map[string]bool{}
	for _, sp := range p.Services {
		if sp.proxied() {
			keep[sp.Name] = true
		}
	}
	for _, sp := range prev.Services {
		if sp.Proxy == nil || keep[sp.Name] {
			continue
		}
		route := deploy.RouteName(d.App, d.Destination, sp.Name)
		r.logf("remove proxy route %s", route)
		for _, s := range d.Servers {
			if err := proxy.Remove(ctx, s.Host, route); err != nil {
				return fmt.Errorf("remove proxy route %s on %s: %w", route, s.Name, err)
			}
		}
	}
	return nil
}

// currentPlan reads the current Release's plan.json. A missing current
// Release is (nil, nil).
func currentPlan(ctx context.Context, h remote.Host, app, dest string) (*stackPlan, error) {
	dir := release.AppDir(app, dest)
	out, err := h.Output(ctx, remote.Cmd{Script: "readlink " + remote.Quote(path.Join(dir, "current")) + " 2>/dev/null || true"})
	if err != nil {
		return nil, err
	}
	ver := path.Base(strings.TrimSpace(out))
	if ver == "" || ver == "." {
		return nil, nil
	}
	p, err := readPlan(ctx, h, release.Dir(app, dest, ver))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return p, nil
}

// releaseCommands runs each x-yoho.release_command before the stack update.
// Dependencies (depends_on of those Services, and Stateful Services) that do
// not exist yet are deployed first so migrations work on the first deploy.
func (r *runner) releaseCommands(ctx context.Context, c *compiled, relDir string, auth bool) error {
	var jobs []servicePlan
	var deps []string
	for _, sp := range c.Plan.Services {
		if len(sp.ReleaseCommand) > 0 {
			jobs = append(jobs, sp)
			deps = append(deps, sp.DependsOn...)
		}
	}
	if len(jobs) == 0 {
		return nil
	}
	for _, sp := range c.Plan.Services {
		if sp.Stateful {
			deps = append(deps, sp.Name)
		}
	}
	existing, err := r.serviceStatus(ctx, r.stack)
	if err != nil {
		return err
	}
	var missing []string
	for _, dep := range deps {
		isJob := slices.ContainsFunc(jobs, func(sp servicePlan) bool { return sp.Name == dep })
		if _, ok := existing[r.stack+"_"+dep]; !ok && !isJob && !slices.Contains(missing, dep) {
			missing = append(missing, dep)
		}
	}
	slices.Sort(missing)
	if len(missing) > 0 {
		r.logf("starting dependencies %s", strings.Join(missing, ", "))
		b, err := subset(c.Doc, missing)
		if err != nil {
			return err
		}
		f := path.Join(relDir, "dependencies.yaml")
		if err := r.h.WriteFile(ctx, f, b, 0o600, false); err != nil {
			return fmt.Errorf("write dependencies stack file: %w", err)
		}
		if err := r.deployStack(ctx, f, r.stack, false, auth); err != nil {
			return fmt.Errorf("start dependencies: %w", err)
		}
	}
	for _, sp := range jobs {
		if err := r.runJob(ctx, c, relDir, sp, auth); err != nil {
			return err
		}
	}
	return nil
}

// ensureStackNetworks creates the main stack's own networks a release job
// joins before the stack exists, labelled so docker stack deploy adopts
// them as its own.
func (r *runner) ensureStackNetworks(ctx context.Context, doc map[string]any, svc string) error {
	s, _ := mapOf(doc, "services")[svc].(map[string]any)
	nets := mapOf(s, "networks")
	if len(nets) == 0 {
		nets = map[string]any{"default": nil}
	}
	var b strings.Builder
	b.WriteString("set -eu\n")
	for _, n := range sortedKeys(nets) {
		def, _ := mapOf(doc, "networks")[n].(map[string]any)
		if def != nil && def["external"] == true {
			continue
		}
		args := []string{"docker", "network", "create", "--label", stackLabel + "=" + r.stack}
		driver, _ := def["driver"].(string)
		if driver == "" {
			driver = "overlay"
		}
		args = append(args, "--driver", driver)
		if def["attachable"] == true {
			args = append(args, "--attachable")
		}
		if def["internal"] == true {
			args = append(args, "--internal")
		}
		for _, k := range sortedKeys(mapOf(def, "labels")) {
			args = append(args, "--label", fmt.Sprintf("%s=%v", k, mapOf(def, "labels")[k]))
		}
		name := realName(doc, "networks", n, r.stack)
		args = append(args, name)
		b.WriteString("docker network inspect " + remote.Quote(name) + " >/dev/null 2>&1 || " + remote.QuoteArgs(args...) + " >/dev/null\n")
	}
	if err := r.h.Run(ctx, remote.Cmd{Script: b.String()}); err != nil {
		return fmt.Errorf("create networks for release command: %w", err)
	}
	return nil
}

// runJob runs one release_command as a replicated-job in the release stack,
// waits for its single task to finish, and removes the stack again. Output
// is shown only on failure.
func (r *runner) runJob(ctx context.Context, c *compiled, relDir string, sp servicePlan, auth bool) error {
	r.logf("release command for %s: %s", sp.Name, strings.Join(sp.ReleaseCommand, " "))
	b, err := releaseJobStack(c.Doc, r.stack, sp.Name, sp.ReleaseCommand)
	if err != nil {
		return err
	}
	f := path.Join(relDir, "release-"+sp.Name+".yaml")
	if err := r.h.WriteFile(ctx, f, b, 0o600, false); err != nil {
		return fmt.Errorf("write release command stack file: %w", err)
	}
	if err := r.ensureStackNetworks(ctx, c.Doc, sp.Name); err != nil {
		return err
	}
	rs := ReleaseStack(r.stack)
	svc := rs + "_" + sp.Name
	rm := "docker stack rm " + remote.Quote(rs) + " >/dev/null 2>&1 || true"
	if err := r.h.Run(ctx, remote.Cmd{Script: rm}); err != nil {
		return err
	}
	defer func() { _ = r.h.Run(context.WithoutCancel(ctx), remote.Cmd{Script: rm}) }()

	fail := func(err error) error {
		logs, _ := r.h.Output(context.WithoutCancel(ctx), remote.Cmd{Script: "docker service logs --raw " + remote.Quote(svc) + " 2>&1 | tail -n 30"})
		return fmt.Errorf("release command for %s failed, aborting (the previous version keeps running): %w\n%s", sp.Name, err, r.redact(logs))
	}
	if err := r.h.Run(ctx, remote.Cmd{Script: stackDeployCmd(r.dir, f, rs, false, auth, false) + " >/dev/null"}); err != nil {
		return fail(err)
	}
	deadline := now().Add(releaseTimeout)
	script := "t=$(docker service ps -q " + remote.Quote(svc) + " | head -n 1)\n" +
		`[ -z "$t" ] || docker inspect -f '{{.Status.State}}|{{with .Status.ContainerStatus}}{{.ExitCode}}{{end}}|{{.Status.Err}}' "$t"`
	for {
		out, err := r.h.Output(ctx, remote.Cmd{Script: script})
		if err != nil {
			return fail(err)
		}
		state, rest, _ := strings.Cut(strings.TrimSpace(out), "|")
		switch state {
		case "complete":
			return nil
		case "failed", "rejected", "shutdown", "orphaned", "remove":
			return fail(fmt.Errorf("task %s (exit code|error: %s)", state, rest))
		}
		if now().After(deadline) {
			return fail(fmt.Errorf("timed out after %s (task state %q)", releaseTimeout, state))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// finish records the Release, points `current` at it and prunes.
func (r *runner) finish(ctx context.Context, d *plan.Deploy, rel *release.Release) error {
	relDir := release.Dir(d.App, d.Destination, rel.Version)
	if err := deploy.WriteJSON(ctx, r.h, path.Join(relDir, "release.json"), rel); err != nil {
		return err
	}
	// The whole stack now runs this record (see partialMarker).
	if err := r.h.Run(ctx, remote.Cmd{Script: "rm -f " + remote.Quote(path.Join(relDir, partialMarker))}); err != nil {
		return fmt.Errorf("clear %s: %w", partialMarker, err)
	}
	if err := r.h.Run(ctx, remote.Cmd{Script: "ln -sfn " + remote.Quote("releases/"+rel.Version) + " " + remote.Quote(path.Join(r.dir, "current"))}); err != nil {
		return fmt.Errorf("update current release: %w", err)
	}
	if err := r.prune(ctx, d, rel.Version); err != nil {
		r.logf("warning: pruning old releases failed: %v", err)
	}
	// Same image cleanup as compose. Swarm's `stack deploy --prune` removes
	// services, not images. Release records live only on the manager, so the
	// retained-image keep-set is computed there once and passed to every
	// Server. Each Server also keeps images its own containers use.
	r.pruneImages(ctx, d)
	return nil
}

// pruneImages removes this App's unused images on every Server. Errors are
// warnings, logged by PruneImages (and here when the keep-set cannot be read).
func (r *runner) pruneImages(ctx context.Context, d *plan.Deploy) {
	keep, err := deploy.RetainedImageRefs(ctx, r.h, d.App)
	if err != nil {
		r.logf("warning: %v", err)
		return
	}
	opts := deploy.PruneOptions{Keep: keep}
	for _, s := range d.Servers {
		deploy.PruneImages(ctx, s.Host, d.App, func(format string, a ...any) {
			fmt.Fprintf(r.out, "["+s.Name+"] "+format+"\n", a...)
		}, opts)
	}
}

// prune keeps the newest RetainReleases successful Releases (always current,
// plus the newest failed record), their
// secrets generations, and the Swarm secrets they reference; it removes the
// rest. Swarm refuses to remove a secret still used by a service, which is
// reported but not an error.
func (r *runner) prune(ctx context.Context, d *plan.Deploy, current string) error {
	retain := d.RetainReleases
	if retain <= 0 {
		retain = defaultRetain
	}
	rels, err := deploy.ListReleases(ctx, r.h, d.App, d.Destination)
	if err != nil {
		return err
	}
	keepGen := map[string]bool{}
	keepSecret := map[string]bool{}
	secretsKnown := true
	var drop []string
	retained := deploy.RetainedReleases(rels, retain, current)
	for _, rel := range rels {
		if retained[rel.Version] {
			keepGen[rel.SecretsGeneration] = true
			p, err := readPlan(ctx, r.h, release.Dir(d.App, d.Destination, rel.Version))
			if err != nil {
				secretsKnown = false
				continue
			}
			for _, s := range p.Secrets {
				keepSecret[s] = true
			}
			continue
		}
		drop = append(drop, path.Join(r.dir, "releases", rel.Version))
	}
	gens, err := r.h.Output(ctx, remote.Cmd{Script: "ls -1 " + remote.Quote(path.Join(r.dir, "secrets")) + " 2>/dev/null || true"})
	if err != nil {
		return err
	}
	for _, g := range lines(gens) {
		if !keepGen[g] {
			drop = append(drop, path.Join(r.dir, "secrets", g))
		}
	}
	if len(drop) > 0 {
		r.logf("pruning %d old release/secrets director(ies)", len(drop))
		if err := r.h.Run(ctx, remote.Cmd{Script: "rm -rf " + remote.QuoteArgs(drop...)}); err != nil {
			return err
		}
	}
	if !secretsKnown {
		r.logf("warning: a retained Release has no readable plan.json; keeping all Swarm secrets")
		return nil
	}
	out, err := r.h.Output(ctx, remote.Cmd{Script: "docker secret ls " + remote.QuoteArgs("--filter", "label="+deploy.LabelApp+"="+d.App, "--filter", "label="+deploy.LabelDestination+"="+d.Destination) + " --format '{{.Name}}'"})
	if err != nil {
		return fmt.Errorf("list swarm secrets: %w", err)
	}
	unused := unusedSecrets(lines(out), keepSecret)
	if len(unused) == 0 {
		return nil
	}
	r.logf("removing %d unused Swarm secret(s)", len(unused))
	inUse, err := r.h.Output(ctx, remote.Cmd{Script: "for s in " + remote.QuoteArgs(unused...) + "; do docker secret rm \"$s\" >/dev/null 2>&1 || echo \"$s\"; done"})
	if err != nil {
		return err
	}
	if l := lines(inUse); len(l) > 0 {
		r.logf("kept %d Swarm secret(s) still in use: %s", len(l), strings.Join(l, ", "))
	}
	return nil
}

// unusedSecrets returns the existing secret names not referenced by any
// retained Release.
func unusedSecrets(existing []string, keep map[string]bool) []string {
	var out []string
	for _, s := range existing {
		if !keep[s] {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return out
}

func readPlan(ctx context.Context, h remote.Host, relDir string) (*stackPlan, error) {
	b, err := h.ReadFile(ctx, path.Join(relDir, "plan.json"), false)
	if err != nil {
		return nil, err
	}
	var p stackPlan
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("parse %s/plan.json: %w", relDir, err)
	}
	return &p, nil
}

// Rollback implements plan.Runtime: it redeploys a retained Release's stack
// file, which still references that Release's Swarm secrets. No build, no
// release_command; volumes and migrations are not reverted.
func (Runtime) Rollback(ctx context.Context, d *plan.Deploy, version string) (_ *release.Release, err error) {
	if len(d.Servers) == 0 || slices.ContainsFunc(d.Servers, func(s plan.NamedHost) bool { return s.Host == nil }) {
		return nil, errors.New("swarm rollback needs open connections to the Destination's Servers")
	}
	if !deploy.ValidVersion(version) {
		return nil, fmt.Errorf("invalid version %q", version)
	}
	start := now()
	h := d.Servers[0].Host
	r := newRunner(d)
	r.setSecrets(d.ServiceSecrets)
	relDir := release.Dir(d.App, d.Destination, version)
	stackFile := path.Join(relDir, "compose.yaml")

	unlock, err := deploy.AcquireLock(ctx, h, d.App, d.Destination, deploy.LockInfo{Performer: d.Performer, Version: version, Command: "rollback", Time: start})
	if err != nil {
		return nil, err
	}
	defer func() {
		if uerr := unlock(ctx); uerr != nil && err == nil {
			err = uerr
		}
	}()
	if _, err := checkSwarm(ctx, d); err != nil {
		return nil, err
	}

	rel, err := deploy.ReadRelease(ctx, h, path.Join(relDir, "release.json"))
	if err != nil {
		return nil, fmt.Errorf("release %s not found on %s: %w", version, d.Servers[0].Name, err)
	}
	if rel.Runtime != "swarm" {
		return nil, fmt.Errorf("release %s was deployed with the %s runtime; switching runtimes is a migration (ADR 0007)", version, rel.Runtime)
	}
	stackYAML, err := h.ReadFile(ctx, stackFile, false)
	if err != nil {
		return nil, fmt.Errorf("release %s has no stack file: %w", version, err)
	}
	if rel.ComposeSHA256 != "" && deploy.SHA256Hex(stackYAML) != rel.ComposeSHA256 {
		return nil, fmt.Errorf("release %s: stack file does not match its recorded sha256", version)
	}
	p, err := readPlan(ctx, h, relDir)
	if err != nil {
		return nil, fmt.Errorf("release %s: %w", version, err)
	}
	if len(p.Secrets) > 0 {
		if err := h.Run(ctx, remote.Cmd{Script: "docker secret inspect " + remote.QuoteArgs(p.Secrets...) + " >/dev/null"}); err != nil {
			return nil, fmt.Errorf("release %s: its Swarm secrets are gone (pruned?): %w", version, err)
		}
	}
	if p.EnvFiles && rel.SecretsGeneration != "" {
		if err := h.Run(ctx, remote.Cmd{Script: "test -d " + remote.Quote(release.SecretsDir(d.App, d.Destination, rel.SecretsGeneration))}); err != nil {
			return nil, fmt.Errorf("release %s: secrets generation %s is gone", version, rel.SecretsGeneration)
		}
	}
	prev, _ := h.Output(ctx, remote.Cmd{Script: "readlink " + remote.Quote(path.Join(r.dir, "current")) + " 2>/dev/null || true"})
	prevVersion := path.Base(strings.TrimSpace(prev))

	r.logf("rolling back to %s (volumes, data and migrations are not reverted)", version)
	if p.needsProxy() {
		if err := r.bootProxy(ctx, d); err != nil {
			return nil, err
		}
	}
	if err := r.deployStack(ctx, stackFile, r.stack, true, registryAuth(d, *p)); err != nil {
		return nil, err
	}
	if err := r.routes(ctx, d, *p); err != nil {
		return nil, err
	}
	if prevVersion != "" && prevVersion != "." && prevVersion != version {
		prevPath := path.Join(release.Dir(d.App, d.Destination, prevVersion), "release.json")
		if pr, err := deploy.ReadRelease(ctx, h, prevPath); err == nil {
			pr.Status = "rolled_back"
			_ = deploy.WriteJSON(ctx, h, prevPath, pr)
		}
	}
	rel.Status = "deployed"
	rel.DeployedAt = start
	rel.Performer = d.Performer
	if err := r.finish(ctx, d, rel); err != nil {
		return nil, err
	}
	r.logf("rolled back to %s", version)
	return rel, nil
}

// Releases implements plan.Runtime: Releases on the manager, newest first.
func (Runtime) Releases(ctx context.Context, d *plan.Deploy) ([]release.Release, error) {
	if len(d.Servers) == 0 || d.Servers[0].Host == nil {
		return nil, errors.New("no Server")
	}
	return deploy.ListReleases(ctx, d.Servers[0].Host, d.App, d.Destination)
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

func tailLines(s string, n int) string {
	ls := lines(s)
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	return strings.Join(ls, "\n")
}
