package ui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

type factFailure struct{}

func (factFailure) Error() string { return "failure" }
func (factFailure) FailureFacts() any {
	return map[string]any{"phase": "proxy-switch", "services": []map[string]string{{"service": "web", "proxy_route": "switched", "convergence": "complete"}, {"service": "worker", "proxy_route": "not-applicable", "convergence": "attempted"}}}
}

func TestStepFailureFactsNDJSON(t *testing.T) {
	var b bytes.Buffer
	u := New(&b, JSON, false)
	step := u.Step("server", "Deploy")
	step.Fail(fmt.Errorf("outer: %w", factFailure{}), "inspect Services; no automatic rollback")
	for _, line := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event["event"] == "step_fail" {
			facts, ok := event["failure_facts"].(map[string]any)
			if !ok || facts["phase"] != "proxy-switch" || len(facts["services"].([]any)) != 2 {
				t.Fatalf("missing facts: %s", line)
			}
			return
		}
	}
	t.Fatal("step_fail missing")
}
func TestStepFailureWithoutFactsRemainsCompatible(t *testing.T) {
	var b bytes.Buffer
	u := New(&b, JSON, false)
	u.Step("", "Deploy").Fail(errors.New("old error"), "hint")
	if strings.Contains(b.String(), "failure_facts") {
		t.Fatal("invented facts")
	}
}
