package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/yoho-build/yoho/internal/backup"
	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/ui"
)

// restoreCleanup is `yoho backup restore --cleanup`: remove the restore
// staging of this Destination with the Destination lock held, so a running
// restore never loses its staging or recovery record.
func restoreCleanup(cmd *cobra.Command, g *globals, jobName string) (err error) {
	a, err := g.loadForJob(cmd, jobName)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	u := a.ui
	defer func() {
		if err != nil {
			u.Finished(err, "")
		}
	}()
	hosts, closeHosts, err := a.connect(ctx)
	if err != nil {
		return err
	}
	defer closeHosts()
	h := hosts[0]
	step := u.Step(h.Name, "Lock %s (%s)", a.cfg.App, a.destName)
	unlock, err := a.backupLock(h.Host, "backup restore --cleanup")(ctx)
	if err != nil {
		var le *deploy.LockedError
		hint := backupHint(err)
		if errors.As(err, &le) {
			hint = "a restore, drill, deploy or Backup of this Destination is running; wait for it to finish, then rerun `yoho backup restore --cleanup`"
		}
		step.Fail(err, hint)
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
	n, err := removeStaging(ctx, u, h.Name, h.Host, a.cfg.App, a.destName, backup.StagingRestore)
	if err != nil {
		return err
	}
	if err := release(); err != nil {
		return fmt.Errorf("unlock: %w", err)
	}
	u.Finished(nil, fmt.Sprintf("Removed %d restore staging directories of %s (%s)", n, a.cfg.App, a.destName))
	return nil
}

// stagingAge renders how long ago a staging directory was created.
func stagingAge(d time.Duration) string {
	switch {
	case d < 0:
		return "just now"
	case d < time.Minute:
		return d.Round(time.Second).String() + " ago"
	case d < 48*time.Hour:
		return d.Round(time.Minute).String() + " ago"
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

// warnLeftovers reports what killed or interrupted drills and restores of
// dest left on the Server, and how to remove it.
func warnLeftovers(u *ui.UI, dest string, l backup.Leftovers) {
	now := time.Now()
	for _, p := range l.Drills {
		u.Warn("leftover drill project %s of an interrupted `yoho backup drill`; remove it with `yoho backup drill --cleanup -d %s`", p, dest)
		u.Event("leftover_drill", "", map[string]any{"project": p})
	}
	for _, d := range l.Staging {
		age := stagingAge(d.Age(now))
		switch {
		case d.Kind == backup.StagingDrill:
			u.Warn("leftover drill staging %s (%s) may hold a decrypted Backup; remove it with `yoho backup drill --cleanup -d %s`", d.Path, age, dest)
		case d.PriorState:
			u.Warn("interrupted restore staging %s (%s) holds prior-state.json and may hold a decrypted Backup; a successful `yoho backup restore` resumes the Services it stopped and removes it, or remove it with `yoho backup restore --cleanup -d %s`", d.Path, age, dest)
		default:
			u.Warn("restore staging %s (%s) holds Backup files (unloaded dumps); remove it with `yoho backup restore --cleanup -d %s`", d.Path, age, dest)
		}
		u.Event("leftover_staging", "", map[string]any{
			"kind": d.Kind, "path": d.Path, "created_at": d.Created.Format(time.RFC3339),
			"age_s": int64(d.Age(now).Seconds()), "prior_state": d.PriorState,
		})
	}
}

// removeStaging removes the leftover staging directories of one kind. The
// caller holds the Destination lock. It returns how many were removed.
func removeStaging(ctx context.Context, u *ui.UI, server string, h remote.Host, app, dest, kind string) (int, error) {
	step := u.Step(server, "Find %s staging of %s (%s)", kind, app, dest)
	dirs, err := backup.ListStaging(ctx, h, app, dest, kind)
	if err != nil {
		step.Fail(err, "check the ownership and mode of the Destination's Backup directory; rerun with -v")
		return 0, &silentError{err}
	}
	step.Done(fmt.Sprintf("%d", len(dirs)))
	now := time.Now()
	for _, d := range dirs {
		if d.PriorState {
			u.Warn("removing the recovery record of an interrupted restore (%s): Services it stopped are not resumed by a later restore; check `yoho app ps -d %s` and start them with `yoho deploy -d %s`", stagingAge(d.Age(now)), dest, dest)
		}
		step := u.Step(server, "Remove %s (%s)", d.Path, stagingAge(d.Age(now)))
		if err := backup.RemoveStaging(ctx, h, app, dest, d); err != nil {
			step.Fail(err, "Yoho removes only staging directories it can prove it created; inspect "+d.Path+" by hand")
			return 0, &silentError{err}
		}
		step.Done()
		u.Event("staging_removed", "", map[string]any{"kind": d.Kind, "path": d.Path, "created_at": d.Created.Format(time.RFC3339), "prior_state": d.PriorState})
	}
	return len(dirs), nil
}
