package gate

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/operation"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
)

// The detached worker owns this record, so an SSH client disappearing cannot
// overwrite its outcome. Operators can query it through status --operation.
func recordOperation(ctx context.Context, d *plan.Deploy, command string, run func() (*release.Release, error)) (*release.Release, error) {
	if d.OperationID == "" {
		return run()
	}
	h := d.Servers[0].Host
	r := operation.Record{ID: d.OperationID, Command: command, Started: time.Now().UTC(), State: "running", Services: map[string]string{}}
	if err := operation.Save(ctx, h, d.App, d.Destination, r); err != nil {
		return nil, err
	}
	d.OnService = func(ctx context.Context, name, state string) {
		r.Services[name] = state
		_ = operation.Save(ctx, h, d.App, d.Destination, r)
	}
	rel, err := run()
	r.State = "finished"
	r.Outcome = "success"
	if err != nil {
		r.Outcome = "failed"
		var failure *deploy.Failure
		if errors.As(err, &failure) {
			for _, f := range failure.Services {
				if f.ProxyRoute == "switched" {
					r.Services[f.Service] = "changed"
				}
			}
		}
		for name, state := range r.Services {
			if state == "changed" {
				r.Changed = append(r.Changed, name)
			} else {
				r.Unchanged = append(r.Unchanged, name)
			}
		}
		if len(r.Changed) > 0 {
			r.Outcome = "partial"
		}
	}
	sort.Strings(r.Changed)
	sort.Strings(r.Unchanged)
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if e := operation.Save(finish, h, d.App, d.Destination, r); e != nil && err == nil {
		return rel, e
	}
	_ = operation.Prune(finish, h, d.App, d.Destination, r.ID)
	return rel, err
}
