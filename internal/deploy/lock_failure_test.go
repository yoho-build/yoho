package deploy

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/remote"
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
	for _, failure := range []string{"record", "mkdir"} {
		t.Run(failure, func(t *testing.T) {
			withRoot(t)
			ctx := context.Background()
			local := &remote.Local{}
			dir := LockDir("shop", "production")
			inject := "cat() { printf partial; return 1; }"
			if failure == "mkdir" {
				inject = "mkdir() { if [ \"$1\" = " + remote.Quote(dir) + " ]; then return 1; fi; command mkdir \"$@\"; }"
			}
			_, err := AcquireLock(ctx, lockFailureHost{Host: local, inject: inject}, "shop", "production", LockInfo{})
			var locked *LockedError
			if err == nil || errors.As(err, &locked) {
				t.Fatalf("filesystem failure reported as held/success: %v", err)
			}
			if !strings.Contains(err.Error(), "free disk space") {
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
