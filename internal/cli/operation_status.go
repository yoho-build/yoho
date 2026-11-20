package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/yoho-build/yoho/internal/operation"
)

func init() { extraCommands = append(extraCommands, operationStatusCmd) }
func operationStatusCmd(g *globals) *cobra.Command {
	var id string
	c := &cobra.Command{Use: "status --operation ID", Short: "Read a mutation's recoverable outcome", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		a, err := g.load(cmd)
		if err != nil {
			return err
		}
		if !operation.ValidID(id) {
			return fmt.Errorf("--operation requires a valid operation ID")
		}
		hosts, closeHosts, err := a.connect(cmd.Context())
		if err != nil {
			return err
		}
		defer closeHosts()
		r, err := operation.Read(cmd.Context(), hosts[0].Host, a.cfg.App, a.destName, id)
		if err != nil {
			return err
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
		a.ui.FinishedOutcome(r.Outcome, r.Changed, r.Unchanged)
		return nil
	}}
	c.Flags().StringVar(&id, "operation", "", "Operation ID emitted by a mutating command")
	return c
}
