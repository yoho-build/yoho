package cli

import (
	"errors"

	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/proxy"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// hintFor returns the failure hint for err: ownership collisions get their
// own, everything else the given default.
func hintFor(err error, def string) string {
	var conflict *proxy.RouteConflictError
	if errors.As(err, &conflict) {
		return proxy.RouteConflictHint
	}
	var oe *release.OwnershipError
	if errors.As(err, &oe) {
		return "rename the App or Destination (names join App and Destination with '-', so another App's resources collide)"
	}
	var me *deploy.MigrationPendingError
	if errors.As(err, &me) {
		return "rerun `yoho deploy` or `yoho apply` to finish the app_dir migration"
	}
	var ae *deploy.AppDirError
	if errors.As(err, &ae) && ae.Hint != "" {
		return ae.Hint
	}
	// The Gate knows why it refused; its hint beats the generic one.
	var ge *remote.GateError
	if errors.As(err, &ge) && ge.Hint != "" {
		return ge.Hint
	}
	return def
}
