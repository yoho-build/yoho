package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/operation"
	"github.com/yoho-build/yoho/internal/plan"
)

func init() { extraCommands = append(extraCommands, operationStatusCmd) }
func operationStatusCmd(g *globals) *cobra.Command {
	var id string
	c := &cobra.Command{Use: "status --operation ID", Short: "Read a mutation's recoverable outcome", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		a, err := g.load(cmd)
		if err != nil {
			return err
		}
		if id == "" && a.dest.Roles != nil {
			return a.rolesStatus(cmd.Context())
		}
		if !operation.ValidID(id) {
			return fmt.Errorf("--operation requires a valid operation ID")
		}
		var hosts []plan.NamedHost
		var closeHosts func()
		if a.dest.Roles != nil {
			hosts, closeHosts, err = a.connectForLocks()
		} else {
			hosts, closeHosts, err = a.connect(cmd.Context())
		}
		if err != nil {
			return err
		}
		defer closeHosts()
		var r operation.Record
		if a.dest.Roles == nil {
			r, err = operation.Read(cmd.Context(), hosts[0].Host, a.cfg.App, a.destName, id)
		} else {
			var missing []error
			found := false
			for _, h := range hosts {
				candidate, e := operation.Read(cmd.Context(), h.Host, a.cfg.App, a.destName, id)
				if e != nil {
					missing = append(missing, e)
					continue
				}
				if !found {
					r = candidate
					found = true
				}
			}
			if !found {
				err = errors.Join(missing...)
			}
		}

		if err != nil {
			return err
		}
		if a.dest.Roles != nil && r.State == "running" {
			r.LockHeld = false
			uncertain := false
			for _, h := range hosts {
				_, held, e := deploy.ReadLock(cmd.Context(), h.Host, a.cfg.App, a.destName)
				if e != nil {
					uncertain = true
					continue
				}
				r.LockHeld = r.LockHeld || held
			}
			r.Outcome = "unknown"
			if r.LockHeld && !uncertain {
				r.Outcome = "running"
			}
		}
		fields := map[string]any{"operation_id": r.ID, "outcome": r.Outcome, "state": r.State, "services": r.Services, "lock_held": r.LockHeld}
		if r.Outcome == "partial" {
			if r.Changed == nil {
				r.Changed = []string{}
			}
			if r.Unchanged == nil {
				r.Unchanged = []string{}
			}
			fields["changed"], fields["unchanged"] = r.Changed, r.Unchanged
		}
		a.ui.Event("operation_status", fmt.Sprintf("Operation %s: %s (%s)", id, r.Outcome, fmt.Sprintf("%s; lock held: %t", r.State, r.LockHeld)), fields)
		if r.Outcome == "running" {
			a.ui.Finished(nil, "")
		} else {
			a.ui.FinishedOutcome(r.Outcome, r.Changed, r.Unchanged)
		}
		return nil
	}}
	c.Flags().StringVar(&id, "operation", "", "Operation ID emitted by a mutating command")
	return c
}
