package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/ui"
)

func lockApp(t *testing.T, wait time.Duration) (*app, *bytes.Buffer, remote.Host) {
	t.Helper()
	old := release.Root
	release.Root = t.TempDir()
	t.Cleanup(func() { release.Root = old })
	oldPoll := lockPollInterval
	lockPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { lockPollInterval = oldPoll })
	var out bytes.Buffer
	a := &app{g: &globals{json: true}, ui: ui.New(&out, ui.JSON, false), destName: "production",
		cfg: &config.Config{App: "shop"}, dest: config.Destination{Servers: []string{"s1"}}, lockWait: wait}
	return a, &out, &remote.Local{}
}

func TestLockWaitWaitsForTheHolderThenProceeds(t *testing.T) {
	a, out, h := lockApp(t, 5*time.Second)
	ctx := context.Background()
	held, err := deploy.AcquireLock(ctx, h, "shop", "production", deploy.LockInfo{Performer: "carol@laptop", Command: deploy.ManualLockCommand, Message: "db maintenance"})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(80 * time.Millisecond)
		_ = held(ctx)
	}()
	unlock, err := a.acquireLock(ctx, h, deploy.LockInfo{Command: "deploy", Version: "v2"})
	if err != nil {
		t.Fatal(err)
	}
	defer unlock(ctx)
	s := out.String()
	if !strings.Contains(s, `"event":"step_wait"`) || !strings.Contains(s, "held by carol@laptop (yoho lock acquire)") ||
		!strings.Contains(s, `"holder":{"command":"lock","locked":true,"manual":true,"message":"db maintenance","performer":"carol@laptop"`) || !strings.Contains(s, `"event":"lock_acquired"`) {
		t.Fatalf("output:\n%s", s)
	}
	info, ok, err := deploy.ReadLock(ctx, h, "shop", "production")
	if err != nil || !ok || info.Command != "deploy" || info.Version != "v2" {
		t.Fatalf("holder %+v %v %v", info, ok, err)
	}
}

func TestLockWaitOffRefusesAtOnceAndGivesUp(t *testing.T) {
	a, out, h := lockApp(t, 0)
	ctx := context.Background()
	if _, err := deploy.AcquireLock(ctx, h, "shop", "production", deploy.LockInfo{Performer: "carol", Command: "deploy", Version: "v1"}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := a.acquireLock(ctx, h, deploy.LockInfo{Command: "deploy"})
	var le *deploy.LockedError
	if !errors.As(err, &le) || le.Holder.Performer != "carol" || time.Since(start) > time.Second || strings.Contains(out.String(), "step_wait") {
		t.Fatalf("err %v\n%s", err, out.String())
	}
	a.lockWait = 100 * time.Millisecond
	_, err = a.acquireLock(ctx, h, deploy.LockInfo{Command: "deploy"})
	if !errors.As(err, &le) || !strings.Contains(err.Error(), "gave up after waiting 100ms") {
		t.Fatalf("err %v", err)
	}
}

func TestDeployLockStepShowsHolder(t *testing.T) {
	a, out, h := lockApp(t, 0)
	ctx := context.Background()
	if _, err := deploy.AcquireLock(ctx, h, "shop", "production", deploy.LockInfo{Performer: "carol", Command: "deploy", Version: "v1"}); err != nil {
		t.Fatal(err)
	}
	_, err := a.deployLock(ctx, plan.NamedHost{Name: "s1", Host: h}, "v2")
	var le *deploy.LockedError
	if !errors.As(err, &le) {
		t.Fatalf("err %v", err)
	}
	s := out.String()
	if !strings.Contains(s, `"step":"Acquire lock"`) || !strings.Contains(s, "locked by carol (deploy v1)") || !strings.Contains(s, "--lock-wait") {
		t.Fatalf("output:\n%s", s)
	}
}

func TestLockWaitFlagDefault(t *testing.T) {
	c := rollbackCmd(&globals{})
	if err := c.ParseFlags([]string{"--lock-wait"}); err != nil {
		t.Fatal(err)
	}
	if d, _ := c.Flags().GetDuration(lockWaitFlag); d != 15*time.Minute {
		t.Errorf("bare --lock-wait = %s", d)
	}
	c = rollbackCmd(&globals{})
	if err := c.ParseFlags([]string{"--lock-wait=30s"}); err != nil {
		t.Fatal(err)
	}
	if d, _ := c.Flags().GetDuration(lockWaitFlag); d != 30*time.Second {
		t.Errorf("--lock-wait=30s = %s", d)
	}
	for _, c := range []*cobra.Command{deployCmd(&globals{}), applyCmd(&globals{})} {
		if c.Flags().Lookup(lockWaitFlag) == nil {
			t.Errorf("%s has no --lock-wait", c.Name())
		}
	}
}

func TestScheduledBackupRefusedByManualLock(t *testing.T) {
	manual := &deploy.LockedError{Dir: "/x/lock", Holder: deploy.LockInfo{Performer: "dave", Command: deploy.ManualLockCommand, Message: "upgrade"}}
	err := scheduledLockError(manual)
	if !errors.As(err, new(*deploy.LockedError)) || !strings.Contains(err.Error(), "skipped: the Destination is locked by hand") || !strings.Contains(err.Error(), `"upgrade"`) || !strings.Contains(err.Error(), "yoho lock release") {
		t.Fatalf("err %v", err)
	}
	other := &deploy.LockedError{Dir: "/x/lock", Holder: deploy.LockInfo{Performer: "ci", Command: "deploy"}}
	if scheduledLockError(other) != error(other) {
		t.Error("a deploy's lock is not a manual lock")
	}
}
