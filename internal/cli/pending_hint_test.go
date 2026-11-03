package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/deploy"
)

func TestLockAndPendingRotationHints(t *testing.T) {
	le := &deploy.LockedError{Dir: "/var/lib/yoho/apps/shop/production/lock", Holder: deploy.LockInfo{Performer: "carol", Command: "secrets rotate"}}
	h := hintFor(fmt.Errorf("wrapped: %w", le), "Services may already have changed")
	if !strings.Contains(h, "nothing was changed") || !strings.Contains(h, "yoho lock release --force") || !strings.Contains(h, "rotate --generated KEY` is finished by running it again") {
		t.Errorf("lock hint %q", h)
	}
	if strings.Contains(h, "may already have changed") {
		t.Errorf("lock hint claims changes: %q", h)
	}
	if h := appDirHint(le); !strings.Contains(h, "yoho lock release --force") {
		t.Errorf("appDirHint %q", h)
	}
	gate := &deploy.LockedError{Dir: "/var/lib/yoho/appdirs/shop/d/production/gate"}
	if h := hintFor(gate, ""); !strings.Contains(h, "remove "+gate.Dir) || strings.Contains(h, "yoho lock release") {
		t.Errorf("gate hint %q", h)
	}
	manual := &deploy.LockedError{Dir: le.Dir, Holder: deploy.LockInfo{Performer: "dave", Command: deploy.ManualLockCommand, Message: "db maintenance"}}
	if h := hintFor(manual, ""); !strings.Contains(h, "`yoho lock release`") || !strings.Contains(h, "--lock-wait") {
		t.Errorf("manual hint %q", h)
	}
	if !strings.Contains(manual.Error(), `"db maintenance"`) || !strings.Contains(manual.Error(), "yoho lock acquire") {
		t.Errorf("manual error %q", manual.Error())
	}
	le.Holder.Command = "deploy"
	if h := hintFor(le, ""); strings.Contains(h, "rotate") {
		t.Errorf("deploy holder hint mentions rotate: %q", h)
	}
	if h := hintFor(&deploy.PendingRotationError{Keys: []string{"K"}}, "def"); h != deploy.PendingRotationHint {
		t.Errorf("pending hint %q", h)
	}
}
