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
	if !strings.Contains(h, "nothing was changed") || !strings.Contains(h, "remove "+le.Dir) || !strings.Contains(h, "rotate --generated KEY` is finished by running it again") {
		t.Errorf("lock hint %q", h)
	}
	if strings.Contains(h, "may already have changed") {
		t.Errorf("lock hint claims changes: %q", h)
	}
	if h := appDirHint(le); !strings.Contains(h, "remove "+le.Dir) {
		t.Errorf("appDirHint %q", h)
	}
	le.Holder.Command = "deploy"
	if h := hintFor(le, ""); strings.Contains(h, "rotate") {
		t.Errorf("deploy holder hint mentions rotate: %q", h)
	}
	if h := hintFor(&deploy.PendingRotationError{Keys: []string{"K"}}, "def"); h != deploy.PendingRotationHint {
		t.Errorf("pending hint %q", h)
	}
}
