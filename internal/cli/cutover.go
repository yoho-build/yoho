package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/migrate"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/proxy"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/secrets"
)

// newTunnelAPI builds the Cloudflare client; tests replace it.
var newTunnelAPI = func(apiToken, tunnelToken string) (migrate.CloudflareAPI, error) {
	return proxy.NewTunnelAPI(apiToken, tunnelToken)
}

// cutoverRun drives a Destination's one-shot migration cutover inside
// apply (ADR 0012): preflight and stop after build and ship, the claim
// check before the rollout, the ingress switch after it.
type cutoverRun struct {
	a      *app
	host   plan.NamedHost
	action migrate.Action
	j      *migrate.Journal
	opts   migrate.Options
	plan   *migrate.Plan
	hook   plan.HookFunc
}

// cutoverAPI resolves the API and tunnel tokens of migrate.tunnel and
// returns the client, or nil when no ingress is switched.
func (a *app) cutoverAPI(store *secrets.Store) (migrate.CloudflareAPI, error) {
	m := a.dest.Migrate
	if m == nil || m.Tunnel == nil {
		return nil, nil
	}
	var apiToken string
	var ok bool
	if store != nil {
		apiToken, ok = store.Get(m.Tunnel.APITokenSecret)
	}
	if !ok || strings.TrimSpace(apiToken) == "" {
		return nil, fmt.Errorf("migrate.tunnel.api_token_secret %s is missing or empty\nhint: add %s to .yoho/secrets: a Cloudflare API token with Account > Cloudflare Tunnel > Edit", m.Tunnel.APITokenSecret, m.Tunnel.APITokenSecret)
	}
	t := a.proxyConfig().Tunnel
	if t == nil || t.TokenSecret == "" {
		return nil, errors.New("migrate.tunnel needs proxy.tunnel.token_secret to know which tunnel to repoint")
	}
	tunnelTok, err := tunnelToken(store, t)
	if err != nil {
		return nil, err
	}
	return newTunnelAPI(strings.TrimSpace(apiToken), tunnelTok)
}

// cutoverEvent emits one `cutover` event (names and timings, no values).
func (a *app) cutoverEvent(phase, human string, j *migrate.Journal, fields map[string]any) {
	f := map[string]any{"phase": phase, "destination": a.destName}
	if j != nil {
		f["cutover"] = j.ID
	}
	for k, v := range fields {
		f[k] = v
	}
	a.ui.Event("cutover", human, f)
}

func containerNames(cs []migrate.Container) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

// beginCutover reads the Journal and decides, before anything is built,
// whether this deploy cuts over. nil means a normal deploy.
func (a *app) beginCutover(ctx context.Context, s *session, resume, allowEmpty bool) (*cutoverRun, error) {
	m := a.dest.Migrate
	if m == nil {
		return nil, nil
	}
	h := s.hosts[0]
	step := a.ui.Step(h.Name, "Cutover journal")
	j, err := migrate.ReadJournal(ctx, h.Host, a.cfg.App, a.destName)
	if err != nil {
		step.Fail(err, "")
		return nil, &silentError{err}
	}
	action, err := migrate.Next(j, resume)
	if err != nil {
		step.Fail(err, "see `yoho migrate status`")
		return nil, &silentError{err}
	}
	if action == migrate.Skip {
		step.Done(j.ID + " done; deploying normally (remove destinations." + a.destName + ".migrate when you like)")
		return nil, nil
	}
	cf, err := a.cutoverAPI(s.store)
	if err != nil {
		step.Fail(err, "")
		return nil, &silentError{err}
	}
	if action == migrate.Start {
		j = migrate.NewJournal(a.cfg.App, a.destName, m.From, time.Now())
		step.Done("new cutover " + j.ID)
	} else {
		step.Done("resuming " + j.ID)
	}
	return &cutoverRun{
		a: a, host: h, action: action, j: j, hook: s.hook,
		opts: migrate.Options{App: a.cfg.App, Destination: a.destName, Migrate: *m, CF: cf, AllowEmpty: allowEmpty},
	}, nil
}

// prepare runs after build and ship: preflight (nothing changed on
// failure), the pre-cutover Hook, stopping the old containers and the
// claim check.
func (c *cutoverRun) prepare(ctx context.Context) error {
	a, h := c.a, c.host
	start := time.Now()
	step := a.ui.Step(h.Name, "Cutover preflight")
	var resumeJ *migrate.Journal
	if c.action == migrate.Resume {
		resumeJ = c.j
	}
	p, err := migrate.Preflight(ctx, h.Host, c.opts, resumeJ)
	if err != nil {
		step.Fail(err, "nothing was stopped or changed; fix the problem and rerun")
		return &silentError{err}
	}
	c.plan = p
	var hostnames []string
	for _, r := range p.Rules {
		hostnames = append(hostnames, r.Hostname)
	}
	step.Done(fmt.Sprintf("%d old container(s), %d ingress rule(s)", len(p.Containers), len(p.Rules)))
	a.cutoverEvent("preflight", "", c.j, map[string]any{
		"containers": containerNames(p.Containers), "hostnames": hostnames, "duration_ms": time.Since(start).Milliseconds(),
	})
	if c.hook != nil {
		if err := c.hook(ctx, "pre-cutover", map[string]string{"YOHO_CUTOVER": c.j.ID}); err != nil {
			return err
		}
	}

	start = time.Now()
	step = a.ui.Step(h.Name, "Stop old containers")
	if err := p.Stop(ctx, h.Host, c.j); err != nil {
		step.Fail(err, "")
		return c.fail(ctx, &silentError{err})
	}
	step.Done(strings.Join(containerNames(c.j.Stopped), ", "))
	a.cutoverEvent("stopped", "", c.j, map[string]any{"containers": containerNames(c.j.Stopped), "duration_ms": time.Since(start).Milliseconds()})

	start = time.Now()
	step = a.ui.Step(h.Name, "Check claimed data")
	if err := p.CheckClaims(ctx, h.Host); err != nil {
		step.Fail(err, "")
		return c.fail(ctx, &silentError{err})
	}
	c.j.ClaimsClear = true
	if err := c.j.Save(ctx, h.Host); err != nil {
		step.Fail(err, "")
		return c.fail(ctx, &silentError{err})
	}
	step.Done()
	a.cutoverEvent("claims_clear", "", c.j, map[string]any{
		"paths": c.opts.Migrate.Claim.Paths, "volumes": c.opts.Migrate.Claim.Volumes, "duration_ms": time.Since(start).Milliseconds(),
	})
	return nil
}

// finish switches ingress and marks the Journal done; rel is the Release
// the cutover deployed, nil when nothing needed deploying.
func (c *cutoverRun) finish(ctx context.Context, rel *release.Release) error {
	a, h := c.a, c.host
	if rel != nil {
		c.j.Version, c.j.Deployed = rel.Version, true
	}
	if t := c.opts.Migrate.Tunnel; t != nil {
		start := time.Now()
		step := a.ui.Step(h.Name, "Switch tunnel ingress to %s", t.TargetService())
		wrote, err := c.plan.SwitchIngress(ctx, h.Host, c.j)
		if err != nil {
			step.Fail(err, "")
			return c.fail(ctx, &silentError{err})
		}
		detail := strings.Join(t.Hostnames, ", ")
		if !wrote {
			detail += " (already switched)"
		}
		step.Done(detail)
		a.cutoverEvent("ingress_switched", "", c.j, map[string]any{"hostnames": t.Hostnames, "service": t.TargetService(), "duration_ms": time.Since(start).Milliseconds()})
	}
	if err := c.plan.Finish(ctx, h.Host, c.j); err != nil {
		// Everything is switched; a rerun with --cutover-resume only records done.
		return fmt.Errorf("%w\nhint: rerun with --cutover-resume to record the cutover as done", err)
	}
	down := c.j.Downtime().Round(100 * time.Millisecond)
	a.cutoverEvent("done", fmt.Sprintf("  Cutover %s done; old containers stopped %s before it finished", c.j.ID, down), c.j,
		map[string]any{"downtime_ms": c.j.Downtime().Milliseconds(), "version": c.j.Version})
	if c.hook != nil {
		if err := c.hook(ctx, "post-cutover", map[string]string{"YOHO_CUTOVER": c.j.ID}); err != nil {
			return err
		}
	}
	return nil
}

// fail applies on_failure after err and returns err unchanged.
func (c *cutoverRun) fail(ctx context.Context, err error) error {
	a, h := c.a, c.host
	if c.j.StoppedAt == nil {
		return err
	}
	if c.opts.Migrate.FailurePolicy() != "restart_old" {
		a.ui.Warn("cutover %s stopped half-way (on_failure: leave); undo with `yoho rollback --cutover` or continue with `yoho deploy --cutover-resume`", c.j.ID)
		return err
	}
	start := time.Now()
	step := a.ui.Step(h.Name, "Restart old containers (on_failure: restart_old)")
	ctx = context.WithoutCancel(ctx)
	if rerr := migrate.Rollback(ctx, h.Host, c.opts.CF, c.j, migrate.RollbackOptions{Force: true}); rerr != nil {
		step.Fail(rerr, "run `yoho rollback --cutover`, or start the old containers by hand: "+strings.Join(containerNames(c.j.Stopped), " "))
		return err
	}
	step.Done(strings.Join(containerNames(c.j.Stopped), ", "))
	a.cutoverEvent("rolled_back", "", c.j, map[string]any{"containers": containerNames(c.j.Stopped), "duration_ms": time.Since(start).Milliseconds()})
	return err
}

// planCutover adds the pending cutover to a plan.
func (a *app) planCutover(ctx context.Context, h plan.NamedHost) ([]plan.Change, error) {
	m := a.dest.Migrate
	if m == nil {
		return nil, nil
	}
	j, err := migrate.ReadJournal(ctx, h.Host, a.cfg.App, a.destName)
	if err != nil {
		return nil, err
	}
	ch := plan.Change{Kind: "cutover", Name: a.destName, Server: h.Name, Downtime: true}
	switch {
	case j != nil && j.State == migrate.StateDone:
		return nil, nil
	case j != nil && j.State == migrate.StateStarted:
		ch.Action = plan.ActionUpdate
		ch.Reasons = []string{"cutover " + j.ID + " is unfinished: `yoho rollback --cutover` or `yoho deploy --cutover-resume`"}
	default:
		ch.Action = plan.ActionCreate
		ch.Reasons = []string{cutoverSummary(*m)}
	}
	return []plan.Change{ch}, nil
}

func cutoverSummary(m config.Migrate) string {
	var sel []string
	if m.Stop.ComposeProject != "" {
		sel = append(sel, "project "+m.Stop.ComposeProject)
	}
	for _, l := range m.Stop.Labels {
		sel = append(sel, "label "+l)
	}
	for _, n := range m.Stop.Containers {
		sel = append(sel, n)
	}
	s := "stop " + strings.Join(sel, ", ")
	if n := len(m.Claim.Paths) + len(m.Claim.Volumes); n > 0 {
		s += fmt.Sprintf(", check %d claim(s)", n)
	}
	if m.Tunnel != nil {
		s += ", switch " + strings.Join(m.Tunnel.Hostnames, ", ") + " → " + m.Tunnel.TargetService()
	}
	return s
}

// failed is fail for a nil-safe call site: without a cutover it returns err.
func (c *cutoverRun) failed(ctx context.Context, err error) error {
	if c == nil {
		return err
	}
	return c.fail(ctx, err)
}
