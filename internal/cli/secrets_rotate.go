package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yoho-build/yoho/internal/composefile"
	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
)

func secretsRotateCmd(g *globals) *cobra.Command {
	var dryRun bool
	c := &cobra.Command{
		Use:   "rotate [service...]",
		Short: "Re-resolve secrets and zero-downtime restart Services whose values changed",
		Long: `Re-resolve secrets from the configured provider (1Password, Bitwarden, or
.yoho/secrets) without rebuilding images or changing code. Services whose
values changed are restarted through the Proxy (zero downtime). Stateful
Services follow their normal stop-first rules. A Release is recorded so
yoho rollback works.

Pass Service names to rotate only those. --dry-run lists what would restart
and changes nothing. Secret values are never printed.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			return a.rotateSecrets(cmd.Context(), args, dryRun)
		},
	}
	c.Flags().BoolVar(&dryRun, "dry-run", false, "List Services that would restart; change nothing")
	return c
}

func (a *app) rotateSecrets(ctx context.Context, services []string, dryRun bool) (err error) {
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
	step.Done(shortVersion(cur.Version))
	s.ver = cur.Version

	d, err := a.newDeploy(s, cur.Images, true)
	if err != nil {
		return err
	}
	step = u.Step("", "Compare secrets")
	changed, err := deploy.DetectChangedServices(ctx, d, cur)
	if err != nil {
		step.Fail(err, "check the current Release's secrets generation on the Server")
		return &silentError{err}
	}
	if len(services) > 0 {
		for _, name := range services {
			if !slices.Contains(changed, name) {
				u.Info("Service %s: secrets unchanged", name)
			}
		}
		for _, name := range changedServicesNotRequested(changed, services) {
			u.Info("Service %s also changed; omitted (not named)", name)
		}
		changed = intersectServices(changed, services)
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
	d.Restart = changed
	rel, err := a.rollout(ctx, s, d, a.runtime())
	if err != nil {
		return err
	}
	u.Finished(nil, fmt.Sprintf("Rotated secrets for %s@%s on %s in %s; restarted %s",
		a.cfg.App, shortVersion(rel.Version), a.destName, time.Since(start).Round(100*time.Millisecond), strings.Join(changed, ", ")))
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
