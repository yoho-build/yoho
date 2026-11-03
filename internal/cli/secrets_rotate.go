package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yoho-build/yoho/internal/composefile"
	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/version"
)

func secretsRotateCmd(g *globals) *cobra.Command {
	var dryRun bool
	var generated []string
	c := &cobra.Command{
		Use:   "rotate [service...] | rotate --generated KEY...",
		Short: "Re-resolve secrets and zero-downtime restart Services whose values changed",
		Long: `Re-resolve secrets from the configured provider (1Password, Bitwarden, or
.yoho/secrets) without rebuilding images or changing code. Services whose
values changed are restarted through the Proxy (zero downtime). Stateful
Services follow their normal stop-first rules. pre-deploy and post-deploy
Hooks run with YOHO_COMMAND="secrets rotate". The current Release is
recorded again with the new secrets generation, so its old values are not
kept: to undo, restore them in the provider and rotate again. (Rolling
back to an earlier Version still runs that Version's secrets.)

The local project must match the deployed one (same git commit and
uncommitted changes, and no other config change), so rotate changes
secrets and nothing else.

Pass Service names to require that only those changed. --dry-run lists
what would restart and changes nothing. Secret values are never printed.

--generated KEY (repeatable) replaces a Server-generated secret
(x-yoho.generate) with a new value from the same generator. The new value
is applied to the database first (Postgres: ALTER ROLE; MySQL/MariaDB:
ALTER USER; or the x-yoho.rotate command), then swapped in, and every
Service that uses it restarts. If the database step fails, the old value
stays in use and a re-run applies the same pending value.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			keys, err := checkGeneratedFlags(generated, args, a.dest.Runtime)
			if err != nil {
				return a.fail(err)
			}
			return a.rotateSecrets(cmd.Context(), args, keys, dryRun)
		},
	}
	c.Flags().BoolVar(&dryRun, "dry-run", false, "List Services that would restart; change nothing")
	c.Flags().StringArrayVar(&generated, "generated", nil, "Rotate this x-yoho.generate secret (repeatable); applies it to the database first")
	return c
}

// checkGeneratedFlags validates --generated keys and returns them sorted
// and unique.
func checkGeneratedFlags(keys, services []string, runtime string) ([]string, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	if len(services) > 0 {
		return nil, &hintError{"--generated cannot be combined with Service names", "rotate restarts every Service that uses the key; drop the Service names"}
	}
	if runtime == "swarm" {
		return nil, &hintError{"--generated supports the compose runtime only", "rotate the value by hand on Swarm; see docs/site/secrets.md"}
	}
	var out []string
	for _, k := range keys {
		if !secretKeyRe.MatchString(k) {
			return nil, &hintError{fmt.Sprintf("--generated %q is not a secret key", k), "use a key from x-yoho.generate, e.g. --generated POSTGRES_PASSWORD"}
		}
		if !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out, nil
}

var secretKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (a *app) rotateSecrets(ctx context.Context, services, generated []string, dryRun bool) (err error) {
	u := a.ui
	start := time.Now()
	defer func() {
		if err != nil {
			u.Finished(err, "")
		}
	}()
	if dryRun {
		u.Title("Rotating secrets for %s on %s (dry run)", a.cfg.App, a.destName)
	} else {
		u.Title("Rotating secrets for %s on %s", a.cfg.App, a.destName)
	}

	step := u.Step("", "Load compose")
	r, err := a.compose(ctx)
	if err != nil {
		step.Fail(err, "run `yoho config check`")
		return &silentError{err}
	}
	keys, _ := a.secretKeys(r)
	findings := composefile.Check(r, a.dest.Runtime, keys)
	for _, f := range findings {
		u.Finding(f.Level, f.Service, f.Message)
	}
	if composefile.HasErrors(findings) {
		err = fmt.Errorf("compose check failed")
		step.Fail(err, "fix the errors above, then `yoho config check`")
		return &silentError{err}
	}
	names := sortedKeys(r.Project.Services)
	if err = checkServices(names, services); err != nil {
		step.Fail(err, "use a Service name from the compose files")
		return &silentError{err}
	}
	step.Done(fmt.Sprintf("%d services", len(r.Project.Services)))

	step = u.Step("", "Resolve secrets")
	store, err := a.loadSecrets(ctx, step)
	if err != nil {
		step.Fail(err, secretsHint(err, "check .yoho/secrets and that op / bw / bws are signed in"))
		return &silentError{err}
	}
	step.Done(fmt.Sprintf("%d keys", len(store.Keys())))
	s := &session{a: a, r: r, store: store, red: store.Redactor(os.Stdout)}
	defer s.close()

	if !dryRun {
		if err = a.hookFunc("current", "secrets rotate", nil)(ctx, "pre-connect", nil); err != nil {
			return err
		}
	}
	hosts, closeHosts, err := a.connect(ctx)
	if err != nil {
		return err
	}
	s.hosts, s.closeHosts = hosts, closeHosts

	if !dryRun {
		if err = a.ensureAppDirs(ctx, hosts); err != nil {
			return err
		}
	}

	step = u.Step(hosts[0].Name, "Read current Release")
	cur, err := a.currentDeployedRelease(ctx, hosts)
	if err != nil {
		step.Fail(err, "run `yoho deploy` first")
		return &silentError{err}
	}
	if cur == nil {
		err = fmt.Errorf("no deployed Release for %s/%s", a.cfg.App, a.destName)
		step.Fail(err, "run `yoho deploy` first")
		return &silentError{err}
	}
	if err = a.checkRotateVersion(cur.Version); err != nil {
		step.Fail(err, "check out the deployed Version so rotate uses its compose files, or run `yoho deploy`")
		return &silentError{err}
	}
	step.Done(shortVersion(cur.Version))
	s.ver = cur.Version

	d, err := a.newDeploy(s, cur.Images, true)
	if err != nil {
		return err
	}
	var gens []*deploy.GeneratedRotation
	runtime := a.runtime()
	if len(generated) > 0 {
		step = u.Step("", "Plan generated secrets")
		var details []string
		for _, key := range generated {
			g, gerr := deploy.PlanGeneratedRotation(d, key)
			if gerr != nil {
				err = gerr
				step.Fail(err, "check x-yoho.generate and x-yoho.rotate; see docs/site/secrets.md#rotating-generated-secrets")
				return &silentError{err}
			}
			gens = append(gens, g)
			details = append(details, generatedDetail(g))
			u.Event("generated_rotation_plan", "", map[string]any{
				"key": g.Key, "service": g.Service, "strategy": g.Strategy, "detected": g.Detected,
				"user": g.User, "consumers": g.Consumers,
			})
		}
		if err = a.checkRotateConfig(ctx, d); err != nil {
			step.Fail(err, "deploy those changes with `yoho deploy`, or undo them, then rotate")
			return &silentError{err}
		}
		step.Done(strings.Join(details, "; "))
	}
	if len(gens) > 0 && !dryRun {
		h := hosts[0].Host
		unlock, lerr := deploy.AcquireLock(ctx, h, a.cfg.App, a.destName, deploy.LockInfo{Performer: performer(), Version: cur.Version, Command: "secrets rotate"})
		if lerr != nil {
			err = lerr
			u.Error(err, appDirHint(err))
			return &silentError{err}
		}
		defer func() {
			if uerr := unlock(context.WithoutCancel(ctx)); uerr != nil && err == nil {
				err = uerr
			}
		}()
		runtime = deploy.Compose{LockHeld: true}
		for _, g := range gens {
			step = u.Step(hosts[0].Name, "Rotate generated %s (%s via %s)", g.Key, g.Strategy, g.Service)
			if rerr := deploy.RotateGeneratedSecret(ctx, h, a.cfg.App, a.destName, performer(), g, func(f string, args ...any) {
				fmt.Fprintf(step.Output(), f+"\n", args...)
			}); rerr != nil {
				err = rerr
				step.Fail(err, fmt.Sprintf("the old %s is still in use; fix the cause and re-run `yoho secrets rotate --generated %s` (it reuses the pending value)", g.Key, g.Key))
				return &silentError{err}
			}
			step.Done("new value in place")
		}
	}
	step = u.Step("", "Compare secrets")
	changed, err := deploy.DetectChangedServices(ctx, d, cur)
	if err != nil {
		step.Fail(err, "check the current Release's secrets generation on the Server")
		return &silentError{err}
	}
	if len(services) > 0 {
		// The new generation and Release audit cover every Service, so
		// leaving out a changed one would record values it never got.
		if other := changedServicesNotRequested(changed, services); len(other) > 0 {
			err = fmt.Errorf("secrets of %s also changed", strings.Join(other, ", "))
			step.Fail(err, "name those Services too, or run `yoho secrets rotate` without Service names")
			return &silentError{err}
		}
		for _, name := range services {
			if !slices.Contains(changed, name) {
				u.Info("Service %s: secrets unchanged", name)
			}
		}
		changed = intersectServices(changed, services)
	}
	if len(gens) == 0 {
		if err = a.checkRotateConfig(ctx, d); err != nil {
			step.Fail(err, "deploy those changes with `yoho deploy`, or undo them, then rotate")
			return &silentError{err}
		}
	}
	if dryRun {
		for _, c := range deploy.ConsumersOf(gens) {
			if !slices.Contains(changed, c) {
				changed = append(changed, c)
			}
		}
		slices.Sort(changed)
	}
	if len(changed) == 0 {
		step.Done("no values changed")
	} else {
		step.Done(strings.Join(changed, ", "))
	}
	u.Plan(rotatePlan(d, changed, hosts[0].Name))
	if dryRun {
		u.Finished(nil, "Dry run; nothing was changed")
		return nil
	}
	if len(changed) == 0 {
		u.Finished(nil, "Secrets unchanged; nothing to restart")
		return nil
	}

	s.hook = a.hookFunc(cur.Version, "secrets rotate", hosts)
	d, err = a.newDeploy(s, cur.Images, false)
	if err != nil {
		return err
	}
	d.Restart, d.RestartFrom = changed, cur
	rel, err := a.rollout(ctx, s, d, runtime)
	if err != nil {
		if len(gens) > 0 {
			u.Warn("The new generated values are in place and the database uses them; run `yoho secrets rotate` to restart the remaining Services")
		}
		return err
	}
	// Scoped deploys read the generation the operator last pushed.
	if err := hosts[0].Host.Run(ctx, remote.Cmd{Script: "test -f " + remote.Quote(release.AccessSecretsFile(a.cfg.App, a.destName))}); err == nil {
		u.Warn("Members' scoped deploys still use the secrets last pushed; run `yoho access secrets push`")
	}
	u.Finished(nil, fmt.Sprintf("Rotated secrets for %s@%s on %s in %s; restarted %s",
		a.cfg.App, shortVersion(rel.Version), a.destName, time.Since(start).Round(100*time.Millisecond), strings.Join(changed, ", ")))
	return nil
}

func generatedDetail(g *deploy.GeneratedRotation) string {
	s := g.Key + ": " + g.Strategy
	if g.User != "" {
		s += " user " + g.User
	}
	s += " on " + g.Service
	if g.Detected {
		s += " (detected)"
	}
	return s
}

// gitSHARe matches a version taken from git (version.FromGit).
var gitSHARe = regexp.MustCompile(`^[0-9a-f]{40}(` + version.UncommittedMarker + `[0-9a-f]+)?$`)

// checkRotateVersion refuses to rotate from compose files other than the
// deployed Version's: rotate recompiles the local project, so local edits
// would ship with the new secrets.
func (a *app) checkRotateVersion(deployed string) error {
	if !gitSHARe.MatchString(deployed) {
		a.ui.Warn("Version %s is not a git commit; rotate uses the local compose files as they are", shortVersion(deployed))
		return nil
	}
	local, err := version.FromGit(a.dir)
	if err != nil {
		return err
	}
	if local != deployed {
		return fmt.Errorf("local checkout is %s but %s runs %s", shortVersion(local), a.destName, shortVersion(deployed))
	}
	return nil
}

// checkRotateConfig refuses when the local project differs from the
// deployed one in more than secrets (a git-ignored .env, say): rotate
// recompiles it, so that change would ship too.
func (a *app) checkRotateConfig(ctx context.Context, d *plan.Deploy) error {
	p, ok := a.runtime().(plan.Planner)
	if !ok {
		return nil
	}
	changes, err := p.Diff(ctx, d)
	if err != nil {
		return err
	}
	var drift []string
	for _, ch := range changes {
		if ch.Kind != "service" {
			continue
		}
		if ch.Action == plan.ActionDelete {
			drift = append(drift, ch.Name+": no longer in the compose files")
			continue
		}
		for _, r := range ch.Reasons {
			if strings.HasPrefix(r, "config changed") || r == "proxy settings changed" || r == "release_command changed" {
				drift = append(drift, ch.Name+": "+r)
			}
		}
	}
	if len(drift) > 0 {
		return fmt.Errorf("the local project differs from the deployed one in more than secrets (%s)", strings.Join(drift, "; "))
	}
	return nil
}

func (a *app) currentDeployedRelease(ctx context.Context, hosts []plan.NamedHost) (*release.Release, error) {
	rels, err := a.runtime().Releases(ctx, &plan.Deploy{
		App: a.cfg.App, Destination: a.destName, Servers: hosts, Out: io.Discard,
	})
	if err != nil {
		return nil, err
	}
	for i := range rels {
		if rels[i].Status == "deployed" {
			rel := rels[i]
			return &rel, nil
		}
	}
	return nil, nil
}

func intersectServices(changed, requested []string) []string {
	want := map[string]bool{}
	for _, name := range requested {
		want[name] = true
	}
	var out []string
	for _, name := range changed {
		if want[name] {
			out = append(out, name)
		}
	}
	return out
}

func changedServicesNotRequested(changed, requested []string) []string {
	want := map[string]bool{}
	for _, name := range requested {
		want[name] = true
	}
	var out []string
	for _, name := range changed {
		if !want[name] {
			out = append(out, name)
		}
	}
	return out
}

func rotatePlan(d *plan.Deploy, names []string, server string) []plan.Change {
	var out []plan.Change
	for _, name := range names {
		ch := plan.Change{Kind: "service", Name: name, Server: server, Reasons: []string{"secrets changed"}}
		if d != nil && d.Ext[name].Proxy != nil {
			ch.Action = plan.ActionUpdate
		} else {
			ch.Action = plan.ActionReplace
			ch.Downtime = true
		}
		out = append(out, ch)
	}
	return out
}
