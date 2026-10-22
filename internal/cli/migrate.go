package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/migrate"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/secrets"
)

func init() { extraCommands = append(extraCommands, migrateCmd) }

func migrateCmd(g *globals) *cobra.Command {
	c := &cobra.Command{
		Use:   "migrate",
		Short: "Inspect the one-shot migration cutover of destinations.<name>.migrate",
		Long: `A Destination's migrate block makes the next deploy or apply cut over from
another platform once: stop the old containers after the build, check that no
running container still mounts the claimed data, deploy, then repoint the
Cloudflare Tunnel ingress. Undo it with 'yoho rollback --cutover'.`,
	}
	c.AddCommand(&cobra.Command{
		Use:   "plan",
		Short: "Run the cutover preflight (read-only): old containers, claimed data, ingress rules",
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			return a.migratePlan(cmd.Context())
		},
	})
	c.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show the cutover journal on the Server",
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			return a.migrateStatus(cmd.Context())
		},
	})
	return c
}

func (a *app) requireMigrate() error {
	if a.dest.Migrate == nil {
		return a.fail(&hintError{"destinations." + a.destName + ".migrate is not set", "add a migrate block to the Destination that cuts over (see `yoho schema`)"})
	}
	return nil
}

// cutoverSecrets resolves secrets only when the cutover switches ingress.
func (a *app) cutoverSecrets(ctx context.Context) (*secrets.Store, error) {
	if a.dest.Migrate == nil || a.dest.Migrate.Tunnel == nil {
		return nil, nil
	}
	step := a.ui.Step("", "Resolve secrets")
	store, err := a.loadSecrets(ctx, step)
	if err != nil {
		step.Fail(err, secretsHint(err, "check .yoho/secrets and that op / bw / bws are signed in"))
		return nil, &silentError{err}
	}
	step.Done(fmt.Sprintf("%d keys", len(store.Keys())))
	return store, nil
}

func (a *app) migratePlan(ctx context.Context) (err error) {
	if err := a.requireMigrate(); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			a.ui.Finished(err, "")
		}
	}()
	a.ui.Title("Cutover plan for %s on %s", a.cfg.App, a.destName)
	store, err := a.cutoverSecrets(ctx)
	if err != nil {
		return err
	}
	hosts, closeHosts, err := a.connect(ctx)
	if err != nil {
		return err
	}
	defer closeHosts()
	return a.migratePlanOn(ctx, hosts[0], store)
}

// migratePlanOn runs the preflight on h and prints what it found.
func (a *app) migratePlanOn(ctx context.Context, h plan.NamedHost, store *secrets.Store) error {
	u := a.ui
	j, err := migrate.ReadJournal(ctx, h.Host, a.cfg.App, a.destName)
	if err != nil {
		return err
	}
	var resume *migrate.Journal
	switch {
	case j != nil && j.State == migrate.StateDone:
		u.Finished(nil, "Cutover "+j.ID+" is done; deploys are normal deploys")
		return nil
	case j != nil && j.State == migrate.StateStarted:
		u.Warn("cutover %s is unfinished; the preflight uses its saved containers", j.ID)
		resume = j
	}
	cf, err := a.cutoverAPI(store)
	if err != nil {
		return err
	}
	step := u.Step(h.Name, "Cutover preflight")
	p, perr := migrate.Preflight(ctx, h.Host, migrate.Options{App: a.cfg.App, Destination: a.destName, Migrate: *a.dest.Migrate, CF: cf}, resume)
	if perr != nil {
		step.Fail(perr, "nothing was changed")
	} else {
		step.Done()
	}
	for _, warning := range p.Warnings {
		u.Warn("%s", warning)
	}
	var conflicts []string
	for _, c := range p.Conflicts {
		conflicts = append(conflicts, c.String())
	}
	var rules []map[string]string
	for _, c := range p.Containers {
		u.Info("  stop      %s (%s)", c.Name, shortID(c.ID))
	}
	for _, c := range p.Conflicts {
		u.Info("  conflict  %s", c)
	}
	for _, r := range p.Rules {
		u.Info("  ingress   %s: %s → %s", r.Hostname, r.Service, a.dest.Migrate.Tunnel.TargetService())
		rules = append(rules, map[string]string{"hostname": r.Hostname, "path": r.Path, "service": r.Service})
	}
	a.cutoverEvent("plan", "", resume, map[string]any{
		"ok": perr == nil, "containers": containerNames(p.Containers), "conflicts": conflicts, "rules": rules, "warnings": p.Warnings,
	})
	if perr != nil {
		return &silentError{perr}
	}
	u.Finished(nil, "Preflight passed; the next deploy or apply cuts over")
	return nil
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func (a *app) migrateStatus(ctx context.Context) error {
	hosts, closeHosts, err := a.connect(ctx)
	if err != nil {
		return err
	}
	defer closeHosts()
	return a.migrateStatusOn(ctx, hosts[0])
}

func (a *app) migrateStatusOn(ctx context.Context, h plan.NamedHost) error {
	j, err := migrate.ReadJournal(ctx, h.Host, a.cfg.App, a.destName)
	if err != nil {
		return a.fail(err)
	}
	if j == nil {
		a.ui.Event("cutover_status", "No cutover on "+h.Name+" for "+a.cfg.App+"/"+a.destName, map[string]any{"state": "none", "destination": a.destName})
		return nil
	}
	ts := func(t *time.Time) string {
		if t == nil {
			return ""
		}
		return t.Local().Format("2006-01-02 15:04:05")
	}
	rows := [][]string{
		{"cutover", j.ID}, {"state", j.State}, {"from", j.From},
		{"started", j.StartedAt.Local().Format("2006-01-02 15:04:05")}, {"stopped", ts(j.StoppedAt)},
		{"containers", strings.Join(containerNames(j.Stopped), ", ")},
		{"claims clear", fmt.Sprint(j.ClaimsClear)}, {"version", shortVersion(j.Version)},
		{"hostnames", strings.Join(j.Hostnames, ", ")}, {"ingress switched", fmt.Sprint(j.IngressSwitched())},
		{"done", ts(j.DoneAt)}, {"rolled back", ts(j.RolledBackAt)},
	}
	if d := j.Downtime(); d > 0 {
		rows = append(rows, []string{"downtime", d.Round(100 * time.Millisecond).String()})
	}
	a.ui.Event("cutover_status", "", map[string]any{
		"cutover": j.ID, "state": j.State, "destination": j.Destination, "from": j.From,
		"started_at": j.StartedAt, "stopped_at": j.StoppedAt, "containers": containerNames(j.Stopped),
		"claims_clear": j.ClaimsClear, "version": j.Version, "hostnames": j.Hostnames,
		"ingress_switched": j.IngressSwitched(), "done_at": j.DoneAt, "rolled_back_at": j.RolledBackAt,
		"downtime_ms": j.Downtime().Milliseconds(),
	})
	if !a.g.json {
		a.ui.Table([]string{"FIELD", "VALUE"}, rows)
	}
	return nil
}

// rollbackCutover is `yoho rollback --cutover`.
func (a *app) rollbackCutover(ctx context.Context, force bool) (err error) {
	u := a.ui
	defer func() {
		if err != nil {
			u.Finished(err, "")
		}
	}()
	u.Title("Rolling back the cutover of %s on %s", a.cfg.App, a.destName)
	store, err := a.cutoverSecrets(ctx)
	if err != nil {
		return err
	}
	hosts, closeHosts, err := a.connect(ctx)
	if err != nil {
		return err
	}
	defer closeHosts()
	if err = a.ensureAppDirs(ctx, hosts); err != nil {
		return err
	}
	h := hosts[0]
	unlock, err := deploy.AcquireLock(ctx, h.Host, a.cfg.App, a.destName, deploy.LockInfo{Performer: performer(), Command: "rollback --cutover"})
	if err != nil {
		u.Step(h.Name, "Lock").Fail(err, "")
		return &silentError{err}
	}
	defer unlock(context.WithoutCancel(ctx))
	return a.rollbackCutoverOn(ctx, h, store, force)
}

func (a *app) rollbackCutoverOn(ctx context.Context, h plan.NamedHost, store *secrets.Store, force bool) error {
	u := a.ui
	step := u.Step(h.Name, "Cutover journal")
	j, err := migrate.ReadJournal(ctx, h.Host, a.cfg.App, a.destName)
	if err == nil && j == nil {
		err = errors.New("no cutover journal for " + a.cfg.App + "/" + a.destName + " on " + h.Name)
	}
	if err != nil {
		step.Fail(err, "nothing to roll back; see `yoho migrate status`")
		return &silentError{err}
	}
	if j.State == migrate.StateRolledBack {
		step.Done(j.ID + " already rolled back")
		u.Finished(nil, "Nothing to do")
		return nil
	}
	step.Done(j.ID + " " + j.State)
	var cf migrate.CloudflareAPI
	if len(j.TunnelBefore) > 0 {
		if cf, err = a.cutoverAPI(store); err != nil {
			u.Step("", "Cloudflare API").Fail(err, "")
			return &silentError{err}
		}
	}
	start := time.Now()
	step = u.Step(h.Name, "Roll back cutover %s", j.ID)
	if err := migrate.Rollback(ctx, h.Host, cf, j, migrate.RollbackOptions{Force: force}); err != nil {
		hint := ""
		var drift *migrate.DriftError
		if errors.As(err, &drift) {
			hint = "nothing was changed; compare the tunnel in Cloudflare Zero Trust with the journal (`yoho migrate status`), then rerun with --force"
		}
		step.Fail(err, hint)
		return &silentError{err}
	}
	names := containerNames(j.Stopped)
	step.Done(strings.Join(names, ", "))
	a.cutoverEvent("rolled_back", "", j, map[string]any{"containers": names, "hostnames": j.Hostnames, "duration_ms": time.Since(start).Milliseconds()})
	u.Warn("volumes and database migrations are not reverted; restore a dump if the new version migrated the data")
	u.Finished(nil, fmt.Sprintf("Rolled back cutover %s: %s stopped, %d old container(s) started", j.ID, a.project(), len(names)))
	return nil
}
