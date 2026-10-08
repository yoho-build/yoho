package cli

import (
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/ui"
)

func TestConfirmApply(t *testing.T) {
	a := &app{g: &globals{}, ui: ui.Discard()}
	if err := a.confirmApply(strings.NewReader("yes\n"), false, false); err == nil || !strings.Contains(err.Error(), "--auto-approve") {
		t.Errorf("no TTY must refuse: %v", err)
	}
	if err := a.confirmApply(strings.NewReader(""), false, true); err != nil {
		t.Errorf("auto-approve: %v", err)
	}
	if err := a.confirmApply(strings.NewReader("yes\n"), true, false); err != nil {
		t.Errorf("yes: %v", err)
	}
	for _, in := range []string{"y\n", "no\n", "Yes\n", ""} {
		if err := a.confirmApply(strings.NewReader(in), true, false); err == nil {
			t.Errorf("%q must not be accepted", in)
		}
	}
	j := &app{g: &globals{json: true}, ui: ui.Discard()}
	if err := j.confirmApply(strings.NewReader("yes\n"), true, false); err == nil || !strings.Contains(err.Error(), "--json") {
		t.Errorf("json without auto-approve: %v", err)
	}
}

func TestApplyPrecheckRefusesWithoutTTY(t *testing.T) {
	old := isTTY
	isTTY = func() bool { return false }
	defer func() { isTTY = old }()
	a := &app{g: &globals{}, ui: ui.Discard()}
	if err := a.confirmPrecheck(applyOptions{}); err == nil {
		t.Error("want refusal")
	}
	if err := a.confirmPrecheck(applyOptions{AutoApprove: true}); err != nil {
		t.Error(err)
	}
}
