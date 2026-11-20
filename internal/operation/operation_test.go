package operation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

func TestRecordRecoveryAndAtomicUpdates(t *testing.T) {
	old := release.Root
	release.Root = filepath.Join(t.TempDir(), "yoho")
	t.Cleanup(func() { release.Root = old })
	h := &remote.Local{}
	ctx := context.Background()
	r := Record{ID: "operation1", Command: "deploy", Started: time.Now().UTC(), State: "running", Services: map[string]string{}}
	if err := Save(ctx, h, "shop", "production", r); err != nil {
		t.Fatal(err)
	}
	got, err := Read(ctx, h, "shop", "production", r.ID)
	if err != nil || got.Outcome != "unknown" {
		t.Fatalf("%+v %v", got, err)
	}
	lock := filepath.Join(release.AppDir("shop", "production"), "lock")
	if err = os.Mkdir(lock, 0700); err != nil {
		t.Fatal(err)
	}
	got, err = Read(ctx, h, "shop", "production", r.ID)
	if err != nil || got.State != "running" || got.Outcome != "unknown" || !got.LockHeld {
		t.Fatalf("%+v %v", got, err)
	}
	r.Services["web"] = "changed"
	r.State = "finished"
	r.Result = Result{Outcome: "partial", Changed: []string{"web"}, Unchanged: []string{"worker"}}
	if err = Save(ctx, h, "shop", "production", r); err != nil {
		t.Fatal(err)
	}
	got, err = Read(ctx, h, "shop", "production", r.ID)
	if err != nil || got.Outcome != "partial" || len(got.Changed) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	for _, id := range []string{"../escape", "a/b", "", "$(cmd)"} {
		r.ID = id
		if err = Save(ctx, h, "shop", "production", r); err == nil {
			t.Fatalf("accepted %q", id)
		}
	}
}
func TestPruneKeepsFifty(t *testing.T) {
	old := release.Root
	release.Root = filepath.Join(t.TempDir(), "yoho")
	t.Cleanup(func() { release.Root = old })
	h := &remote.Local{}
	ctx := context.Background()
	for i := 0; i < 52; i++ {
		r := Record{ID: fmt.Sprintf("op%d", i), Started: time.Unix(int64(i), 0), State: "finished", Result: Result{Outcome: "success"}}
		if err := Save(ctx, h, "shop", "production", r); err != nil {
			t.Fatal(err)
		}
	}
	counted := &countingHost{Host: h}
	if err := Prune(ctx, counted, "shop", "production", "op51"); err != nil {
		t.Fatal(err)
	}
	if counted.calls != 1 {
		t.Fatalf("pruning used %d remote commands", counted.calls)
	}
	files, err := os.ReadDir(Dir("shop", "production"))
	if err != nil || len(files) != 50 {
		t.Fatalf("files=%d %v", len(files), err)
	}
}

type countingHost struct {
	remote.Host
	calls int
}

func (h *countingHost) Run(ctx context.Context, c remote.Cmd) error {
	h.calls++
	return h.Host.Run(ctx, c)
}
func (h *countingHost) Output(ctx context.Context, c remote.Cmd) (string, error) {
	h.calls++
	return h.Host.Output(ctx, c)
}
