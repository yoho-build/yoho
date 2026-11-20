package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/ui"
)

func previewPruneCmd(g *globals) *cobra.Command {
	var olderThanStr string
	var keep int
	var includeUnknownAge bool
	var yes bool
	var autoApprove bool

	c := &cobra.Command{
		Use:   "prune",
		Short: "Remove stale marked previews matching age or keep-N rules",
		Long: `Select marked previews on the template's Servers using --older-than and/or
--keep, print a reviewable plan, ask for confirmation, and remove each
preview through the same verified teardown path as preview down.

Older preview records without timestamps are skipped by --older-than unless
--include-unknown-age is passed. Pass --yes or --auto-approve in CI and with
--json. A failure on one preview does not stop the remaining previews.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) (err error) {
			u := g.ui(cmd)
			finished := false
			defer func() {
				if err != nil && !finished {
					u.Finished(err, "")
				}
			}()
			a, err := g.loadPreviewTemplate(cmd)
			if err != nil {
				return err
			}
			u = a.ui

			var olderThan time.Duration
			if cmd.Flags().Changed("older-than") {
				olderThan, err = deploy.ParsePreviewDuration(olderThanStr)
				if err != nil {
					return a.fail(&hintError{err.Error(), "pass a positive duration such as --older-than 7d or --older-than 48h"})
				}
			}
			hasKeep := cmd.Flags().Changed("keep")
			if hasKeep && keep < 0 {
				return a.fail(&hintError{"--keep must be 0 or greater", "pass --keep 0 to remove all previews or a positive count to retain"})
			}
			if olderThan == 0 && !hasKeep {
				return a.fail(&hintError{
					"specify --older-than DURATION and/or --keep N",
					"for example: yoho preview prune --older-than 7d or yoho preview prune --keep 3",
				})
			}

			confirmed := yes || autoApprove
			if err := checkPreviewPruneConfirm(confirmed, isTerminal(cmd.InOrStdin()), g.json); err != nil {
				return a.fail(err)
			}

			ctx := cmd.Context()
			hosts, closeHosts, err := a.connect(ctx)
			if err != nil {
				return err
			}
			defer closeHosts()

			now := time.Now().UTC()
			opts := deploy.PreviewPruneOptions{
				OlderThan:         olderThan,
				HasKeep:           hasKeep,
				Keep:              keep,
				IncludeUnknownAge: includeUnknownAge,
				Now:               now,
			}

			type hostCandidate struct {
				host plan.NamedHost
				cand deploy.PreviewPruneCandidate
			}
			var targets []hostCandidate
			skippedUnknown := []string{}

			for _, h := range hosts {
				previews, err := deploy.ListPreviews(ctx, h.Host, a.cfg.App)
				if err != nil {
					return err
				}
				filtered := make([]deploy.PreviewRecord, 0, len(previews))
				for _, p := range previews {
					if _, configured := a.cfg.Destinations[p.Destination]; configured {
						continue
					}
					filtered = append(filtered, p)
				}
				prPlan := deploy.SelectPrunePreviews(filtered, opts)
				for _, c := range prPlan.Remove {
					targets = append(targets, hostCandidate{host: h, cand: c})
				}
				for _, s := range prPlan.SkippedUnknownAge {
					skippedUnknown = append(skippedUnknown, s.Name)
				}
			}

			if len(targets) == 0 {
				if len(skippedUnknown) > 0 {
					a.ui.Info("Skipped %s with unknown age (%s); pass --include-unknown-age to include them",
						ui.Count(len(skippedUnknown), "preview", ""), strings.Join(skippedUnknown, ", "))
				}
				a.ui.Event("preview.prune", "", map[string]any{
					"outcomes":            []map[string]any{},
					"removed":             []string{},
					"absent":              []string{},
					"failed":              []map[string]string{},
					"skipped_unknown_age": skippedUnknown,
				})
				a.ui.Finished(nil, "No previews to prune")
				finished = true
				return nil
			}

			planItems := make([]map[string]any, 0, len(targets))
			a.ui.Title("Preview prune plan (%s to remove)", ui.Count(len(targets), "preview", ""))
			for _, t := range targets {
				age, lastDeploy := formatPreviewTimestamps(t.cand.Record, now)
				planItems = append(planItems, map[string]any{
					"name":        t.cand.Record.Name,
					"destination": t.cand.Record.Destination,
					"server":      t.host.Name,
					"hosts":       t.cand.Record.Hosts,
					"age":         age,
					"last_deploy": lastDeploy,
					"reason":      t.cand.Reason,
				})
				if !g.json {
					a.ui.Info("  - %s [%s] (age: %s, last deploy: %s) — %s",
						t.cand.Record.Name, t.host.Name, age, lastDeploy, t.cand.Reason)
				}
			}
			a.ui.Event("preview.prune_plan", "", map[string]any{
				"remove":              planItems,
				"skipped_unknown_age": skippedUnknown,
			})
			if len(skippedUnknown) > 0 && !g.json {
				a.ui.Info("  = skipped %s with unknown age (%s); pass --include-unknown-age to include them",
					ui.Count(len(skippedUnknown), "preview", ""), strings.Join(skippedUnknown, ", "))
			}

			if !confirmed {
				if err := askPreviewPrune(cmd.InOrStdin(), cmd.OutOrStdout(), len(targets)); err != nil {
					return a.fail(err)
				}
			}

			removed := []string{}
			absent := []string{}
			failed := []map[string]string{}
			outcomes := make([]map[string]any, 0, len(targets))
			var outcomeParts []string

			for _, t := range targets {
				rec := t.cand.Record
				step := a.ui.Step(t.host.Name, "Remove preview %s and its volumes", rec.Name)
				p, hint, rerr := a.removePreviewOnHost(ctx, t.host, rec.Name, rec.Destination, "preview prune", &rec)
				if errors.Is(rerr, errPreviewChanged) {
					step.Skip("preview changed since selection")
					outcomes = append(outcomes, map[string]any{"name": rec.Name, "server": t.host.Name, "status": "skipped_changed"})
					outcomeParts = append(outcomeParts, rec.Name+" (skipped_changed)")
					continue
				}
				if rerr != nil {
					step.Fail(rerr, hint)
					failed = append(failed, map[string]string{"name": rec.Name, "server": t.host.Name, "error": rerr.Error()})
					outcomes = append(outcomes, map[string]any{"name": rec.Name, "server": t.host.Name, "status": "failed", "error": rerr.Error()})
					outcomeParts = append(outcomeParts, fmt.Sprintf("%s (failed: %v)", rec.Name, rerr))
					continue
				}
				hostsOut := []string{}
				if p == nil {
					step.Done("preview is already absent")
					absent = append(absent, rec.Name)
					outcomes = append(outcomes, map[string]any{"name": rec.Name, "server": t.host.Name, "status": "absent"})
					outcomeParts = append(outcomeParts, rec.Name+" (absent)")
				} else {
					hostsOut = p.Hosts
					step.Done()
					removed = append(removed, rec.Name)
					outcomes = append(outcomes, map[string]any{"name": rec.Name, "server": t.host.Name, "status": "removed"})
					outcomeParts = append(outcomeParts, rec.Name+" (removed)")
				}
				a.ui.Event("preview.down", "", map[string]any{"name": rec.Name, "hosts": hostsOut})
			}

			a.ui.Event("preview.prune", "", map[string]any{
				"outcomes":            outcomes,
				"removed":             removed,
				"absent":              absent,
				"failed":              failed,
				"skipped_unknown_age": skippedUnknown,
			})

			summary := fmt.Sprintf("Pruned %s: %s", ui.Count(len(removed), "preview", ""), strings.Join(outcomeParts, ", "))
			if len(failed) > 0 {
				ferr := fmt.Errorf("%s failed to prune", ui.Count(len(failed), "preview", ""))
				summary = fmt.Sprintf("Pruned %s, %s failed: %s",
					ui.Count(len(removed), "preview", ""), ui.Count(len(failed), "preview", ""), strings.Join(outcomeParts, ", "))
				a.ui.Finished(ferr, summary)
				finished = true
				return &silentError{ferr}
			}
			a.ui.Finished(nil, summary)
			finished = true
			return nil
		},
	}

	c.Flags().StringVar(&olderThanStr, "older-than", "", "Remove previews older than DURATION (for example 7d, 48h, 2w)")
	c.Flags().IntVar(&keep, "keep", -1, "Keep the N newest previews and remove the rest")
	c.Flags().BoolVar(&includeUnknownAge, "include-unknown-age", false, "With --older-than, also prune older records without timestamps")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "Remove selected previews without asking for confirmation")
	c.Flags().BoolVar(&autoApprove, "auto-approve", false, "Remove selected previews without asking for confirmation")
	addLockWaitFlag(c)
	return c
}

// removePreviewOnHost runs the single verified preview teardown path shared by
// `preview down` and `preview prune`: collision check, CheckPreviewDir,
// per-preview Destination lock, and deploy.RemovePreview.
func (a *app) removePreviewOnHost(ctx context.Context, h plan.NamedHost, previewName, dest, command string, selected *deploy.PreviewRecord) (*deploy.PreviewRecord, string, error) {
	if _, exists := a.cfg.Destinations[dest]; exists {
		return nil, "configured Destinations cannot be removed as previews",
			fmt.Errorf("preview %q collides with configured Destination %s", previewName, dest)
	}
	if _, err := deploy.CheckPreviewDir(ctx, h.Host, a.cfg.App, dest); err != nil {
		return nil, "only marked previews or empty preview directories can be removed", err
	}
	target := *a
	target.destName = dest
	target.previewName = previewName
	unlock, err := target.acquireLock(ctx, h.Host, deploy.LockInfo{Command: command})
	if err != nil {
		return nil, hintFor(err, ""), err
	}
	defer func() { _ = unlock(context.WithoutCancel(ctx)) }()
	if selected != nil {
		current, err := deploy.ReadPreview(ctx, h.Host, a.cfg.App, dest)
		if err != nil {
			return nil, "only valid marked previews can be removed", err
		}
		if current != nil && !reflect.DeepEqual(current, selected) {
			return nil, "", errPreviewChanged
		}
	}
	p, err := deploy.RemovePreview(ctx, h.Host, a.cfg.App, dest, h.Server.Sudo)
	if err != nil {
		return nil, "only marked previews can be removed; rerun to finish an interrupted teardown", err
	}
	return p, "", nil
}

var errPreviewChanged = errors.New("preview changed since selection")

// admitPreview is called under the Destination lock. The App lock makes
// counting and reserving a slot atomic across different preview names.
func (a *app) admitPreview(ctx context.Context, h remote.Host, record deploy.PreviewRecord) (err error) {
	unlock, err := a.waitLock(ctx, func(ctx context.Context) (func(context.Context) error, error) {
		return deploy.AcquirePreviewLock(ctx, h, a.cfg.App, deploy.LockInfo{Performer: performer(), Command: "preview up"})
	})
	if err != nil {
		return err
	}
	defer func() {
		if uerr := unlock(context.WithoutCancel(ctx)); uerr != nil && err == nil {
			err = uerr
		}
	}()
	if err := a.checkPreviewMaxActive(ctx, h); err != nil {
		return err
	}
	return deploy.MarkPreview(ctx, h, record)
}

func (a *app) checkPreviewMaxActive(ctx context.Context, h remote.Host) error {
	if a.cfg.Previews == nil || a.cfg.Previews.MaxActive <= 0 {
		return nil
	}
	previews, err := deploy.ListPreviews(ctx, h, a.cfg.App)
	if err != nil {
		return err
	}
	active := make([]deploy.PreviewRecord, 0, len(previews))
	for _, p := range previews {
		if _, configured := a.cfg.Destinations[p.Destination]; configured {
			continue
		}
		active = append(active, p)
	}
	if err := deploy.CheckMaxActive(active, a.destName, a.cfg.Previews.MaxActive); err != nil {
		return a.fail(&hintError{
			err.Error(),
			"run `yoho preview prune --older-than 7d` or `yoho preview down --name <name>` to remove old previews",
		})
	}
	return nil
}

func checkPreviewPruneConfirm(confirmed, tty, jsonOut bool) error {
	if confirmed {
		return nil
	}
	if jsonOut {
		return &hintError{"--json requires --yes or --auto-approve (there is no one to confirm)", "pass --yes or --auto-approve"}
	}
	if !tty {
		return &hintError{"refusing to prune previews without confirmation: stdin is not a terminal", "pass --yes or --auto-approve"}
	}
	return nil
}

func askPreviewPrune(in io.Reader, out io.Writer, count int) error {
	r := bufio.NewReader(in)
	fmt.Fprintf(out, "\nRemove %s and all their volumes? Only 'yes' will be accepted: ", ui.Count(count, "preview", ""))
	line, _ := r.ReadString('\n')
	if strings.TrimSpace(line) != "yes" {
		return errors.New("preview prune cancelled")
	}
	return nil
}

func formatPreviewTimestamps(p deploy.PreviewRecord, now time.Time) (age, lastDeploy string) {
	if p.CreatedAt.IsZero() {
		age = "unknown"
	} else {
		age = deploy.FormatPreviewAge(now.Sub(p.CreatedAt))
	}
	if p.LastDeployedAt.IsZero() {
		lastDeploy = "unknown"
	} else {
		lastDeploy = deploy.FormatPreviewAge(now.Sub(p.LastDeployedAt)) + " ago"
	}
	return age, lastDeploy
}
