package deploy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/remote"
)

func TestReadAndReleaseLock(t *testing.T) {
	withRoot(t)
	ctx := context.Background()
	h := &remote.Local{}
	if _, held, err := ReadLock(ctx, h, "shop", "production"); err != nil || held {
		t.Fatalf("free lock: %v %v", held, err)
	}
	if _, held, err := ReleaseLock(ctx, h, "shop", "production", false); err != nil || held {
		t.Fatalf("release a free lock: %v %v", held, err)
	}
	if _, err := AcquireLock(ctx, h, "shop", "production", LockInfo{Performer: "ci", Command: "deploy", Version: "v3"}); err != nil {
		t.Fatal(err)
	}
	info, held, err := ReleaseLock(ctx, h, "shop", "production", false)
	var le *LockedError
	if !held || !errors.As(err, &le) || info.Command != "deploy" {
		t.Fatalf("a deploy's lock released without --force: %+v %v", info, err)
	}
	if _, held, _ := ReadLock(ctx, h, "shop", "production"); !held {
		t.Fatal("lock gone")
	}
	if _, held, err := ReleaseLock(ctx, h, "shop", "production", true); err != nil || !held {
		t.Fatalf("--force: %v", err)
	}
	if _, err := AcquireLock(ctx, h, "shop", "production", LockInfo{Performer: "dave", Command: ManualLockCommand, Message: "maintenance"}); err != nil {
		t.Fatal(err)
	}
	_, err = AcquireLock(ctx, h, "shop", "production", LockInfo{Performer: "ci", Command: "deploy"})
	if !errors.As(err, &le) || !le.Manual() || !strings.Contains(err.Error(), `"maintenance"`) || !strings.Contains(err.Error(), "`yoho lock release`") {
		t.Fatalf("err %v", err)
	}
	info, held, err = ReleaseLock(ctx, h, "shop", "production", false)
	if err != nil || !held || info.Message != "maintenance" {
		t.Fatalf("manual release: %+v %v", info, err)
	}
	if _, held, _ := ReadLock(ctx, h, "shop", "production"); held {
		t.Fatal("manual lock still held")
	}
}
