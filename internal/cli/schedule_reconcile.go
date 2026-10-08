package cli

import (
	"context"

	"github.com/yoho-dev/yoho/internal/plan"
	"github.com/yoho-dev/yoho/internal/secrets"
)

// planSchedules returns the changes apply would make to Scheduled Jobs on the
// Destination's Servers: jobs with a schedule in config but not installed
// (create), installed with a different spec (update), installed but no longer
// in config (delete). Read-only.
func (a *app) planSchedules(ctx context.Context, hosts []plan.NamedHost) ([]plan.Change, error) {
	return nil, nil // implemented by the release/schedule work
}

// applySchedules converges Scheduled Jobs to config (install/update/remove).
func (a *app) applySchedules(ctx context.Context, hosts []plan.NamedHost, store *secrets.Store) error {
	return nil // implemented by the release/schedule work
}
