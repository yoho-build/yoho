package ui

import (
	"fmt"
	"strings"

	"github.com/yoho-build/yoho/internal/plan"
)

// PlanCounts tallies changes by Action (noop excluded from the summary).
type PlanCounts struct{ Add, Change, Replace, Destroy, Noop int }

// CountPlan tallies changes.
func CountPlan(changes []plan.Change) PlanCounts {
	var c PlanCounts
	for _, ch := range changes {
		switch ch.Action {
		case plan.ActionCreate:
			c.Add++
		case plan.ActionUpdate:
			c.Change++
		case plan.ActionReplace:
			c.Replace++
		case plan.ActionDelete:
			c.Destroy++
		default:
			c.Noop++
		}
	}
	return c
}

// HasChanges reports whether any change is not a noop.
func HasChanges(changes []plan.Change) bool {
	c := CountPlan(changes)
	return c.Add+c.Change+c.Replace+c.Destroy > 0
}

// Plan renders changes Terraform-style (human) or as one `change` event per
// change plus a `plan_summary` event (JSON). Noops are only counted.
func (u *UI) Plan(changes []plan.Change) {
	u.mu.Lock()
	defer u.mu.Unlock()
	c := CountPlan(changes)
	if u.mode == JSON {
		for _, ch := range changes {
			f := map[string]any{"kind": ch.Kind, "name": ch.Name, "action": string(ch.Action)}
			if ch.Server != "" {
				f["server"] = ch.Server
			}
			if len(ch.Reasons) > 0 {
				f["reasons"] = ch.Reasons
			}
			if ch.Downtime {
				f["downtime"] = true
			}
			u.event("change", f)
		}
		u.event("plan_summary", map[string]any{"add": c.Add, "change": c.Change, "replace": c.Replace, "destroy": c.Destroy, "noop": c.Noop})
		return
	}
	if !HasChanges(changes) {
		fmt.Fprintln(u.w, u.paint(green+bold, "No changes. Your Servers match the configuration."))
		return
	}
	for _, ch := range changes {
		var sym, style, note string
		switch ch.Action {
		case plan.ActionCreate:
			sym, style, note = "+", green, "create"
		case plan.ActionUpdate:
			sym, style, note = "~", yellow, "update in-place (zero downtime)"
		case plan.ActionReplace:
			sym, style, note = "-/+", yellow, "replace (brief downtime)"
		case plan.ActionDelete:
			sym, style, note = "-", red, "delete"
		default:
			continue
		}
		if ch.Action == plan.ActionUpdate && ch.Kind != "service" && ch.Kind != "route" {
			note = "update"
		}
		name := ch.Kind + " " + ch.Name
		if ch.Server != "" {
			name += " [" + ch.Server + "]"
		}
		fmt.Fprintln(u.w, u.paint(style, fmt.Sprintf("  %-3s %s: %s", sym, name, note)))
		for _, r := range ch.Reasons {
			fmt.Fprintln(u.w, u.paint(dim, "        "+strings.TrimSpace(r)))
		}
	}
	fmt.Fprintln(u.w)
	line := fmt.Sprintf("Plan: %d to add, %d to change, %d to replace, %d to destroy.", c.Add, c.Change, c.Replace, c.Destroy)
	fmt.Fprintln(u.w, u.paint(bold, line))
}
