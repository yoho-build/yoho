package deploy

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/safepath"
)

type lockFailureHost struct {
	remote.Host
	inject string
}

func (h lockFailureHost) Output(ctx context.Context, cmd remote.Cmd) (string, error) {
	cmd.Script = h.inject + "\n" + cmd.Script
	return h.Host.Output(ctx, cmd)
}

func TestLockFilesystemFailureAllowsRetry(t *testing.T) {
	for _, failure := range []string{"record", "mkdir", "gate", "gate-record", "canonical", "coordination"} {
		t.Run(failure, func(t *testing.T) {
			withRoot(t)
			ctx := context.Background()
			local := &remote.Local{}
			dir := LockDir("shop", "production")
			inject := "cat() { printf partial; return 1; }"
			if failure == "mkdir" {
				inject = "mkdir() { if [ \"$1\" = " + remote.Quote(dir) + " ]; then return 1; fi; command mkdir \"$@\"; }"
			}
			switch failure {
			case "gate":
				inject = "mkdir() { if [ \"$1\" = " + remote.Quote(safepath.GateDir("shop", "production")) + " ]; then echo 'No space left on device' >&2; return 1; fi; command mkdir \"$@\"; }"
			case "gate-record":
				inject = "printf() { if [ \"$1\" = %s ]; then echo 'No space left on device' >&2; return 1; fi; command printf \"$@\"; }"
			case "canonical", "coordination":
				target := safepath.MetaDir("shop", "production")
				if failure == "canonical" {
					target = strings.TrimSuffix(dir, "/lock")
				}
				inject = "mkdir() { if [ \"${2:-}\" = " + remote.Quote(target) + " ]; then echo 'No space left on device' >&2; return 1; fi; command mkdir \"$@\"; }"
			}

			_, err := AcquireLock(ctx, lockFailureHost{Host: local, inject: inject}, "shop", "production", LockInfo{})
			var locked *LockedError
			if err == nil || errors.As(err, &locked) {
				t.Fatalf("filesystem failure reported as held/success: %v", err)
			}
			if !strings.Contains(err.Error(), "free disk space") && !strings.Contains(err.Error(), "No space left on device") {
				t.Fatalf("missing hint: %v", err)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatalf("failed lock left behind: %v", err)
			}
			holder := LockInfo{
				Performer: "alice@laptop", Command: "deploy", Version: "v1", Message: "after disk recovery",
				Time: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC), PID: 123, Boot: "boot-id",
			}
			unlock, err := AcquireLock(ctx, local, "shop", "production", holder)
			if err != nil {
				t.Fatalf("retry: %v", err)
			}
			_, err = AcquireLock(ctx, local, "shop", "production", LockInfo{Performer: "bob"})
			if !errors.As(err, &locked) || locked.Holder != holder {
				t.Fatalf("contention lost holder metadata: %v (%+v)", err, locked)
			}
			info, held, err := ReadLock(ctx, local, "shop", "production")
			if err != nil || !held || info != holder {
				t.Fatalf("holder reread: %+v, held=%t, err=%v", info, held, err)
			}
			if err := unlock(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLockVanishedAfterMkdirContention(t *testing.T) {
	withRoot(t)
	ctx := context.Background()
	local := &remote.Local{}
	dir := LockDir("shop", "production")
	// Another process removes the contended directory before the caller can
	// inspect it. The mkdir result is still contention, never an I/O failure.
	inject := "mkdir() { if [ \"$1\" = " + remote.Quote(dir) + " ]; then echo 'mkdir: lock: File exists' >&2; return 1; fi; command mkdir \"$@\"; }"
	_, err := AcquireLock(ctx, lockFailureHost{Host: local, inject: inject}, "shop", "production", LockInfo{})
	var locked *LockedError
	if !errors.As(err, &locked) {
		t.Fatalf("vanished contender must be retryable: %v", err)
	}
	unlock, err := AcquireLock(ctx, local, "shop", "production", LockInfo{Performer: "retry"})
	if err != nil {
		t.Fatal(err)
	}
	if err := unlock(ctx); err != nil {
		t.Fatal(err)
	}
}
