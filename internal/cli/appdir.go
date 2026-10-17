package cli

import (
	"context"
	"errors"

	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
)

// ensureAppDirs converges the canonical App directory of this Destination
// to app_dir on every Server (compose: one; Swarm: every node), moving it
// when app_dir changed and finishing an interrupted move first. Deploy and
// apply run it before the build step, rollback and `schedule install` before
// they write anything.
func (a *app) ensureAppDirs(ctx context.Context, hosts []plan.NamedHost) error {
	for _, h := range hosts {
		step := a.ui.Step(h.Name, "App directory")
		res, err := deploy.EnsureAppDir(ctx, h.Host, a.cfg.App, a.destName, a.dest.AppDir)
		if err != nil {
			step.Fail(err, appDirHint(err))
			return &silentError{err}
		}
		detail := res.Physical
		canon := release.AppDir(a.cfg.App, a.destName)
		switch res.Action {
		case "forward":
			detail = "moved " + canon + " → " + res.Physical
		case "back":
			detail = "moved back to " + canon
		case "retarget":
			detail = "moved to " + res.Physical
		case "created":
			detail = "created " + res.Physical
		}
		if res.Resumed != "" {
			detail = "finished interrupted " + res.Resumed + " migration; " + detail
		}
		step.Done(detail)
	}
	return nil
}

// planAppDirs reports app_dir changes (read-only).
func (a *app) planAppDirs(ctx context.Context, hosts []plan.NamedHost) ([]plan.Change, error) {
	var out []plan.Change
	for _, h := range hosts {
		st, err := deploy.ReadAppDir(ctx, h.Host, a.cfg.App, a.destName)
		if err != nil {
			return nil, err
		}
		if ch, ok := deploy.PlanAppDir(st, a.cfg.App, a.destName, a.dest.AppDir); ok {
			ch.Server = h.Name
			out = append(out, ch)
		}
	}
	return out, nil
}

// appDirHint is the failure hint for App directory and lock errors.
func appDirHint(err error) string {
	var ae *deploy.AppDirError
	if errors.As(err, &ae) && ae.Hint != "" {
		return ae.Hint
	}
	var le *deploy.LockedError
	if errors.As(err, &le) {
		return "another command holds the App directory; wait and rerun (a stale gate or lock is removed by hand, as the message says)"
	}
	var me *deploy.MigrationPendingError
	if errors.As(err, &me) {
		return "rerun `yoho deploy` or `yoho apply` to finish the app_dir migration"
	}
	return hintFor(err, "check app_dir in the Yoho file and the directory on the Server")
}
