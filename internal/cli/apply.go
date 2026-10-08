package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yoho-build/yoho/internal/build"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/proxy"
	"github.com/yoho-build/yoho/internal/ui"
	"github.com/yoho-build/yoho/internal/version"
)

func init() { extraCommands = append(extraCommands, planCmd, applyCmd) }

// Overridable in tests.
var (
	applyInput io.Reader = stdinReader
	isTTY                = stdinIsTTY
	exitFunc             = os.Exit
)

type applyOptions struct {
	Version     string
	SkipBuild   bool
	AutoApprove bool
	// ShowPlan prints the plan first and skips work when nothing changes
	// (`yoho apply`). `yoho deploy` leaves it false.
	ShowPlan bool
}

func planCmd(g *globals) *cobra.Command {
	var ver string
	var detailed bool
	c := &cobra.Command{
		Use:   "plan",
		Short: "Show what apply would change on the Servers (read-only)",
		Long: `Compare the Yoho file and compose files with what runs on the Servers
(containers, Proxy routes, Tunnel, Scheduled Jobs) and show what 'yoho apply'
would create, update, replace or delete. Nothing is built or changed.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			changes, err := a.plan(cmd.Context(), ver)
			if err != nil {
				return err
			}
			if detailed && ui.HasChanges(changes) {
				exitFunc(2)
			}
			return nil
		},
	}
	c.Flags().StringVar(&ver, "version", "", "Version to plan for (default: git SHA)")
	c.Flags().BoolVar(&detailed, "detailed-exitcode", false, "Exit with code 2 when there are changes (0: none, 1: error)")
	return c
}

func applyCmd(g *globals) *cobra.Command {
	var o applyOptions
	c := &cobra.Command{
		Use:   "apply",
		Short: "Converge the Servers to the configuration after showing the plan",
		Long: `Show the plan, ask for confirmation, then build, ship and deploy what
changed, converge the Tunnel and Scheduled Jobs, and remove what was deleted
from the configuration.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			o.ShowPlan = true
			return a.apply(cmd.Context(), o)
		},
	}
	c.Flags().StringVar(&o.Version, "version", "", "Version to deploy (default: git SHA)")
	c.Flags().BoolVar(&o.SkipBuild, "skip-build", false, "Use images already built for this version")
	c.Flags().BoolVar(&o.AutoApprove, "auto-approve", false, "Apply without asking for confirmation")
	return c
}

// plan connects read-only, prints the plan and returns the changes.
func (a *app) plan(ctx context.Context, ver string) (changes []plan.Change, err error) {
	u := a.ui
	defer func() {
		if err != nil {
			u.Finished(err, "")
		}
	}()
	if ver, err = a.resolveDeployVersion(ver); err != nil {
		return nil, err
	}
	u.Title("Planning %s@%s on %s", a.cfg.App, shortVersion(ver), a.destName)
	s, err := a.openSession(ctx, ver, true)
	if err != nil {
		return nil, err
	}
	defer s.close()
	changes, _, err = a.collectChanges(ctx, s)
	if err != nil {
		return nil, err
	}
	u.Plan(changes)
	return changes, nil
}

// collectChanges gathers changes from the runtime, Tunnel and Scheduled
// Jobs. deployNeeded reports whether the runtime has anything to roll out
// (always true when the runtime cannot plan).
func (a *app) collectChanges(ctx context.Context, s *session) (changes []plan.Change, deployNeeded bool, err error) {
	// Image references the Version would produce, without building.
	images := map[string]string{}
	for _, name := range sortedKeys(s.r.Project.Services) {
		if s.r.Project.Services[name].Build != nil {
			images[name] = build.ImageName(a.cfg.Registry, a.cfg.App, name, s.ver)
		}
	}
	d, err := a.newDeploy(s, images, true)
	if err != nil {
		return nil, false, err
	}
	step := a.ui.Step("", "Read state of %s", s.hosts[0].Name)
	deployNeeded = true
	if p, ok := a.runtime().(plan.Planner); ok {
		rc, perr := p.Diff(ctx, d)
		if perr != nil {
			step.Fail(perr, "check that Docker is running on the Server")
			return nil, false, &silentError{perr}
		}
		changes = append(changes, rc...)
		deployNeeded = false
		for _, c := range rc {
			if c.Action != plan.ActionNoop {
				deployNeeded = true
			}
		}
	}
	token, err := tunnelToken(s.store, a.proxyConfig().Tunnel)
	if err != nil {
		step.Fail(err, "")
		return nil, false, &silentError{err}
	}
	tc, err := a.planTunnel(ctx, s.hosts, token)
	if err != nil {
		step.Fail(err, "check that Docker is running on the Server")
		return nil, false, &silentError{err}
	}
	changes = append(changes, tc...)
	sc, err := a.planSchedules(ctx, s.hosts, s.store)
	if err != nil {
		step.Fail(err, "check the Scheduled Jobs on the Server")
		return nil, false, &silentError{err}
	}
	changes = append(changes, sc...)
	step.Done()
	return changes, deployNeeded, nil
}

// planTunnel compares the Cloudflare Tunnel connector with config.
func (a *app) planTunnel(ctx context.Context, hosts []plan.NamedHost, token string) ([]plan.Change, error) {
	cfg := a.proxyConfig().Tunnel
	owner := proxy.TunnelOwner(a.cfg.App, a.destName)
	var out []plan.Change
	for _, h := range hosts {
		st, err := proxy.TunnelStatusOf(ctx, h.Host)
		if err != nil {
			return nil, err
		}
		ch := plan.Change{Kind: "tunnel", Name: proxy.TunnelContainer, Server: h.Name}
		switch {
		case cfg == nil && st.OwnedBy(owner):
			ch.Action = plan.ActionDelete
			ch.Reasons = []string{"no longer in config"}
		case cfg == nil:
			// Absent, or managed by another App on this Server: not ours to remove.
			continue
		case !st.Exists():
			ch.Action = plan.ActionCreate
			if cfg.TokenSecret == "" {
				ch.Reasons = []string{"Quick Tunnel"}
			} else {
				ch.Reasons = []string{"token tunnel → " + proxy.TunnelOrigin}
			}
		default:
			var why []string
			wantMode := "quick"
			if cfg.TokenSecret != "" {
				wantMode = "token"
			}
			if st.Mode != wantMode {
				why = append(why, "mode "+st.Mode+" → "+wantMode)
			}
			image := cfg.Image
			if image == "" {
				image = proxy.DefaultTunnelImage
			}
			if st.Containers[0].Image != image {
				why = append(why, "image "+st.Containers[0].Image+" → "+image)
			}
			want := cfg.Replicas
			if want < 1 {
				want = 1
			}
			if len(st.Containers) != want {
				why = append(why, fmt.Sprintf("connectors %d → %d", len(st.Containers), want))
			}
			if wantHash := proxy.TunnelConfigHash(*cfg, token); st.Mode == wantMode && st.Containers[0].Image == image {
				for _, c := range st.Containers {
					// Same image and mode but another hash: the token rotated.
					if c.ConfigHash != "" && c.ConfigHash != wantHash {
						why = append(why, "token changed")
						break
					}
				}
			}
			if !st.Healthy() {
				why = append(why, "connector not running or not registered")
			}
			if len(why) == 0 {
				ch.Action = plan.ActionNoop
			} else {
				ch.Action = plan.ActionUpdate
				ch.Reasons = why
			}
		}
		out = append(out, ch)
	}
	return out, nil
}

// confirmApply asks Terraform-style; only "yes" proceeds.
func (a *app) confirmApply(in io.Reader, tty, auto bool) error {
	if auto {
		return nil
	}
	if a.g.json {
		return errors.New("--json requires --auto-approve (there is no one to confirm)")
	}
	if !tty {
		return errors.New("refusing to apply without confirmation: stdin is not a terminal\nhint: pass --auto-approve")
	}
	a.ui.Info("")
	a.ui.Info("Do you want to perform these actions? Only 'yes' will be accepted:")
	line, _ := bufio.NewReader(in).ReadString('\n')
	if strings.TrimSpace(line) != "yes" {
		return errors.New("apply cancelled")
	}
	return nil
}

// apply converges the Destination to config: plan, confirm, build, ship,
// deploy, Tunnel, Scheduled Jobs.
func (a *app) apply(ctx context.Context, o applyOptions) (err error) {
	u := a.ui
	start := time.Now()
	defer func() {
		if err != nil {
			u.Finished(err, "")
		}
	}()
	// Fail before any work when we could not ask.
	if o.ShowPlan {
		if err = a.confirmPrecheck(o); err != nil {
			return err
		}
	}
	ver, err := a.resolveDeployVersion(o.Version)
	if err != nil {
		return err
	}
	if version.IsUncommitted(ver) {
		u.Warn("deploying uncommitted changes as %s", ver)
	}
	if o.ShowPlan {
		u.Title("Applying %s@%s to %s", a.cfg.App, shortVersion(ver), a.destName)
	} else {
		u.Title("Deploying %s@%s to %s", a.cfg.App, shortVersion(ver), a.destName)
	}
	s, err := a.openSession(ctx, ver, false)
	if err != nil {
		return err
	}
	defer s.close()

	// Fail before building or changing anything when the managed Tunnel's
	// token cannot be resolved.
	if _, terr := tunnelToken(s.store, a.proxyConfig().Tunnel); terr != nil {
		u.Step("", "Cloudflare Tunnel").Fail(terr, "")
		return &silentError{terr}
	}

	deployNeeded := true
	if o.ShowPlan {
		var changes []plan.Change
		if changes, deployNeeded, err = a.collectChanges(ctx, s); err != nil {
			return err
		}
		u.Plan(changes)
		if !ui.HasChanges(changes) {
			u.Finished(nil, "")
			return nil
		}
		if err = a.confirmApply(applyInput, isTTY(), o.AutoApprove); err != nil {
			return err
		}
	}

	urls := a.urls(s.r, s.hosts)
	version := ver
	if deployNeeded {
		s.hook = a.hookFunc(ver, "deploy", s.hosts)
		images, berr := a.buildAndShip(ctx, s, o.SkipBuild)
		if berr != nil {
			return berr
		}
		d, derr := a.newDeploy(s, images, false)
		if derr != nil {
			return derr
		}
		rel, rerr := a.rollout(ctx, s, d)
		if rerr != nil {
			return rerr
		}
		version = rel.Version
	}
	tunnelURLs, err := a.reconcileTunnel(ctx, s)
	if err != nil {
		return err
	}
	urls += tunnelURLs
	if err = a.applySchedules(ctx, s.hosts, s.store); err != nil {
		u.Step("", "Scheduled Jobs").Fail(err, "run `yoho schedule status`")
		return &silentError{err}
	}
	u.Finished(nil, fmt.Sprintf("Deployed %s@%s to %s in %s%s", a.cfg.App, shortVersion(version), a.destName, time.Since(start).Round(100*time.Millisecond), urls))
	return nil
}

// confirmPrecheck rejects an unconfirmable apply before connecting anywhere.
func (a *app) confirmPrecheck(o applyOptions) error {
	if o.AutoApprove {
		return nil
	}
	if a.g.json {
		return errors.New("--json requires --auto-approve (there is no one to confirm)")
	}
	if !isTTY() {
		return errors.New("refusing to apply without confirmation: stdin is not a terminal\nhint: pass --auto-approve")
	}
	return nil
}
