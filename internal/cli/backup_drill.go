package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yoho-build/yoho/internal/backup"
	"github.com/yoho-build/yoho/internal/deploy"
)

func backupDrillCmd(g *globals) *cobra.Command {
	var id string
	var keep, cleanup bool
	c := &cobra.Command{
		Use:   "drill [JOB]",
		Short: "Prove a Backup restores: restore it into a throwaway project and check health",
		Long: `Restore a Backup (--id, default latest) into a throwaway compose project
yoho-<app>-<destination>-drill-<id> on the Destination's Server: fresh drill
volumes, one internal network, no published ports and no Proxy route. Yoho
starts the backed-up Services from the deployed Release, loads restore_dump,
waits for their healthchecks, runs x-yoho.backup.verify, reports per-phase
timings (download, restore, healthy) as a recovery time estimate, then removes
the drill's containers, volumes and network. The running App is not stopped or
changed. Before restoring, the drill runs the real restore's read-only
isolation preflight against the App and fails wherever ` + "`yoho backup restore`" + `
would refuse.

--keep leaves the drill running for inspection; remove it later with
--cleanup, which takes the Destination lock (so it never removes a drill that
is still running), removes every drill of this App/Destination and never
touches objects without the drill labels.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			if cleanup && (keep || cmd.Flags().Changed("id")) {
				return errors.New("--cleanup cannot be combined with --keep or --id")
			}
			name := ""
			if len(args) > 0 {
				name = args[0]
			}
			a, err := g.loadForJob(cmd, name)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			u := a.ui
			start := time.Now()
			defer func() {
				if err != nil {
					u.Finished(err, "")
				}
			}()
			if cleanup {
				hosts, closeHosts, err := a.connect(ctx)
				if err != nil {
					return err
				}
				defer closeHosts()
				h := hosts[0]
				// A running drill holds the Destination lock; take it so
				// cleanup never removes a drill another operator is running.
				step := u.Step(h.Name, "Lock %s (%s)", a.cfg.App, a.destName)
				unlock, err := a.backupLock(h.Host, "backup drill --cleanup")(ctx)
				if err != nil {
					step.Fail(err, drillCleanupLockHint(err))
					return &silentError{err}
				}
				step.Done()
				unlocked := false
				release := func() error {
					unlocked = true
					uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
					defer cancel()
					return unlock(uctx)
				}
				defer func() {
					if !unlocked {
						_ = release() // a failed cleanup already reports its error
					}
				}()
				step = u.Step(h.Name, "Find drills of %s (%s)", a.cfg.App, a.destName)
				projects, err := backup.ListDrills(ctx, h.Host, a.cfg.App, a.destName)
				if err != nil {
					step.Fail(err, "rerun with -v to see the remote output")
					return &silentError{err}
				}
				step.Done(fmt.Sprintf("%d", len(projects)))
				for _, p := range projects {
					step := u.Step(h.Name, "Remove drill %s", p)
					if err := backup.RemoveDrill(ctx, h.Host, a.cfg.App, a.destName, p); err != nil {
						step.Fail(err, "inspect the project with `docker ps -a --filter label=com.docker.compose.project="+p+"`; Yoho only removes drill-labelled objects")
						return &silentError{err}
					}
					step.Done()
					u.Event("drill_removed", "", map[string]any{"project": p})
				}
				if err := release(); err != nil {
					return fmt.Errorf("unlock: %w", err)
				}
				u.Finished(nil, fmt.Sprintf("Removed %d drills of %s (%s)", len(projects), a.cfg.App, a.destName))
				return nil
			}
			if id == "" {
				id = "latest"
			}
			if a.dest.Runtime == "swarm" {
				err = errors.New("backup drill supports compose Destinations only")
				u.Error(err, "drill a restore of the Backup into a compose Destination with `yoho backup restore --from`")
				return &silentError{err}
			}
			j, err := a.prepareBackup(ctx, name, true)
			if err != nil {
				return err
			}
			u.Title("Restore drill of %s (%s) from %s", a.cfg.App, a.destName, j.targetName)
			hosts, closeHosts, err := a.connect(ctx)
			if err != nil {
				return err
			}
			defer closeHosts()
			h := hosts[0]
			step := u.Step(h.Name, "Find Backup %s in %s", id, j.targetName)
			entries, err := backup.List(ctx, backup.ListOptions{
				App: a.cfg.App, Destination: a.destName, Project: a.project(),
				Target: j.target, Password: j.password, TargetEnv: j.env, RcloneConfig: j.rcloneConf, Host: h.Host,
			})
			if err != nil {
				step.Fail(err, backupHint(err))
				return &silentError{err}
			}
			var found *backup.Entry
			for i, e := range entries {
				if e.ID == id || (id == "latest" && i == 0) {
					found = &entries[i]
					break
				}
			}
			if found == nil {
				err = fmt.Errorf("no Backup %q in %s", id, j.targetName)
				step.Fail(err, "see `yoho backup list`; take one with `yoho backup run`")
				return &silentError{err}
			}
			step.Done(found.Time.Local().Format("2006-01-02 15:04:05"))

			step = u.Step(h.Name, "Drill %s", found.ID)
			out, flush := j.redact(step.Output())
			res, err := backup.Drill(ctx, backup.DrillOptions{
				App: a.cfg.App, Destination: a.destName,
				Services: j.services, Target: j.target, Password: j.password, TargetEnv: j.env, RcloneConfig: j.rcloneConf,
				Host: h.Host, Out: out, Lock: a.backupLock(h.Host, "backup drill"), ID: found.ID, Keep: keep,
				Runtime: a.dest.Runtime,
				Phase: func(p backup.DrillPhase) {
					u.Event("drill_phase", "", map[string]any{"phase": p.Name, "duration_ms": p.Duration.Milliseconds(), "detail": p.Detail})
				},
			})
			flush()
			if err != nil {
				step.Fail(err, drillHint(err))
				return &silentError{err}
			}
			step.Done(res.Project)
			rows := make([][]string, 0, len(res.Phases)+1)
			for _, p := range res.Phases {
				rows = append(rows, []string{p.Name, p.Duration.Round(100 * time.Millisecond).String(), p.Detail})
			}
			rows = append(rows, []string{"RTO estimate", res.RTO.Round(100 * time.Millisecond).String(), "download + restore + healthy"})
			u.Table([]string{"PHASE", "TIME", "DETAIL"}, rows)
			for _, s := range res.Unloaded {
				u.Warn("dump of %s was not loaded (no x-yoho.backup.restore_dump)", s)
			}
			if len(res.Verified) == 0 {
				u.Info("no x-yoho.backup.verify command: the drill proved restore and health, not your data; add one to check it")
			}
			u.Event("drill", "", map[string]any{
				"project": res.Project, "backup_id": found.ID, "services": res.Services, "volumes": res.Volumes, "dumps": res.Dumps,
				"verified": res.Verified, "rto_ms": res.RTO.Milliseconds(), "kept": res.Kept,
			})
			if res.Kept {
				u.Info("drill %s is still running on %s (internal network, no published ports)", res.Project, h.Name)
				u.Info("inspect: docker compose -p %s ps", res.Project)
				u.Info("remove:  yoho backup drill --cleanup -d %s", a.destName)
			}
			u.Finished(nil, fmt.Sprintf("Restore drill of %s (%s) from %s passed in %s; RTO estimate %s",
				a.cfg.App, a.destName, found.ID, time.Since(start).Round(100*time.Millisecond), res.RTO.Round(100*time.Millisecond)))
			return nil
		},
	}
	c.Flags().StringVar(&id, "id", "", "Backup ID to drill (default latest)")
	c.Flags().BoolVar(&keep, "keep", false, "leave the drill project running for inspection")
	c.Flags().BoolVar(&cleanup, "cleanup", false, "remove leftover drill projects of this App/Destination and exit")
	return c
}

func drillCleanupLockHint(err error) string {
	var le *deploy.LockedError
	if errors.As(err, &le) {
		return "a drill, deploy or Backup of this Destination is running; wait for it to finish, then rerun `yoho backup drill --cleanup`"
	}
	return backupHint(err)
}

func drillHint(err error) string {
	s := err.Error()
	switch {
	case strings.HasPrefix(s, backup.RestoreRefusedPrefix):
		return "the drill isolates what it restores, but `yoho backup restore` onto the running App refuses this; fix the refusal (e.g. remove published ports or host networking from Services with restore_dump or post_restore, or deploy the App first), deploy, and rerun the drill"
	case strings.Contains(s, "deploy first"):
		return "deploy the App first; the drill runs the deployed Release's Services"
	case strings.Contains(s, "did not become healthy"), strings.Contains(s, "not ready"):
		return "the restored Service did not pass its healthcheck; rerun with --keep and inspect its logs"
	case strings.Contains(s, "verify"):
		return "the x-yoho.backup.verify command failed against the restored data; rerun with --keep to inspect"
	case strings.Contains(s, "--cleanup"):
		return "remove the leftover drill with `yoho backup drill --cleanup`"
	}
	return backupHint(err)
}
