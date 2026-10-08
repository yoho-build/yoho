package ui

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/plan"
)

var sample = []plan.Change{
	{Kind: "service", Name: "web", Action: plan.ActionUpdate, Reasons: []string{"image a → b"}},
	{Kind: "service", Name: "db", Action: plan.ActionReplace, Downtime: true, Reasons: []string{"config changed: ports"}},
	{Kind: "service", Name: "new", Action: plan.ActionCreate},
	{Kind: "route", Name: "old", Action: plan.ActionDelete, Reasons: []string{"stale"}},
	{Kind: "service", Name: "same", Action: plan.ActionNoop},
}

func TestPlanHuman(t *testing.T) {
	var b bytes.Buffer
	New(&b, Human, false).Plan(sample)
	out := b.String()
	for _, want := range []string{
		"~   service web: update in-place (zero downtime)", "        image a → b",
		"-/+ service db: replace (brief downtime)", "+   service new: create",
		"-   route old: delete", "Plan: 1 to add, 1 to change, 1 to replace, 1 to destroy.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "same") {
		t.Error("noop must be hidden")
	}
}

func TestPlanNoChanges(t *testing.T) {
	var b bytes.Buffer
	New(&b, Human, false).Plan([]plan.Change{{Kind: "service", Name: "web", Action: plan.ActionNoop}})
	if strings.TrimSpace(b.String()) != "No changes. Your Servers match the configuration." {
		t.Errorf("%q", b.String())
	}
	if HasChanges(nil) {
		t.Error("nil has no changes")
	}
}

func TestPlanColor(t *testing.T) {
	u := New(&bytes.Buffer{}, Human, false)
	u.color = true
	var b bytes.Buffer
	u.w = &b
	u.Plan(sample)
	if !strings.Contains(b.String(), yellow+"  -/+") || !strings.Contains(b.String(), red+"  -   route") {
		t.Errorf("colors: %q", b.String())
	}
}

func TestPlanJSON(t *testing.T) {
	var b bytes.Buffer
	New(&b, JSON, false).Plan(sample)
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != len(sample)+1 {
		t.Fatalf("%d lines:\n%s", len(lines), b.String())
	}
	var first, last map[string]any
	json.Unmarshal([]byte(lines[0]), &first)
	json.Unmarshal([]byte(lines[len(lines)-1]), &last)
	if first["event"] != "change" || first["action"] != "update" || first["name"] != "web" {
		t.Errorf("%v", first)
	}
	if last["event"] != "plan_summary" || last["add"] != 1.0 || last["replace"] != 1.0 || last["destroy"] != 1.0 || last["change"] != 1.0 {
		t.Errorf("%v", last)
	}
}
