package cli

import (
	"errors"

	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/proxy"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/secrets"
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
	var df *deploy.Failure
	if errors.As(err, &df) && df.Network != nil {
		return poolHint(df.Network)
	}
	// The Gate knows why it refused; its hint beats the generic one.
	var ge *remote.GateError
	if errors.As(err, &ge) && ge.Hint != "" {
		return ge.Hint
	}
	return def
}

// poolHint explains Docker address pool exhaustion with the subnet found free
// on the Server and the compose snippet that pins it.
// secretsHint maps a failed secrets resolution to its own hint; def is the
// caller's text for failures Yoho cannot place.
func secretsHint(err error, def string) string {
	var pe *secrets.ProviderError
	if !errors.As(err, &pe) {
		return def
	}
	switch pe.Class {
	case secrets.ApprovalTimeout:
		return "the " + pe.Provider + " approval prompt was not answered in time; approve it when it appears and rerun (or pass --secrets-retries 1)"
	case secrets.NotSignedIn:
		return "sign in to " + pe.Provider + " (`op signin`, `bw unlock`, or set BWS_ACCESS_TOKEN) and rerun"
	case secrets.NotFound:
		return "check the ref in secrets.values or .yoho/secrets: " + pe.Provider + " could not find that item or field"
	}
	return def
}

func poolHint(n *deploy.NetworkHint) string {
	return "Docker has no free address pool; add networks.default.ipam.config[].subnet: " + n.SuggestedSubnet +
		" to compose, or set destinations.<name>.subnet\n" + n.Snippet
}
