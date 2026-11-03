package swarm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/release"
)

func TestDeployAndRollbackRefusePendingRotation(t *testing.T) {
	withRoot(t)
	ctx := context.Background()
	h := newFake(newSim())
	if _, err := (Runtime{}).Deploy(ctx, testDeploy(t, h, nil)); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(release.AppDir("shop", "production"), "generated")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "POSTGRES_PASSWORD.next"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := testDeploy(t, h, nil)
	d.Version = "v2"
	var pe *deploy.PendingRotationError
	if _, err := (Runtime{}).Deploy(ctx, d); !errors.As(err, &pe) {
		t.Fatalf("deploy err %v", err)
	}
	if _, err := (Runtime{}).Rollback(ctx, testDeploy(t, h, nil), "v1"); !errors.As(err, &pe) {
		t.Fatalf("rollback err %v", err)
	}
}

func TestDiffAnnotatesDefaultLogging(t *testing.T) {
	_, doc := compileTest(t, func(in *compileInput) { in.LogDriver = deploy.DaemonLogging{Driver: "json-file"} })
	des := get(t, doc, "services", "web").(map[string]any)
	cur := map[string]any{}
	for k, v := range des {
		if k != "logging" {
			cur[k] = v
		}
	}
	reasons, err := diffReasons(cur, des)
	if err != nil {
		t.Fatal(err)
	}
	want := "config changed: " + deploy.DefaultLoggingNote
	if len(reasons) != 1 || reasons[0] != want {
		t.Fatalf("reasons %q, want %q", reasons, want)
	}
	own := map[string]any{}
	for k, v := range des {
		own[k] = v
	}
	own["logging"] = map[string]any{"driver": "local"}
	if reasons, _ := diffReasons(cur, own); len(reasons) != 1 || reasons[0] != "config changed: logging" {
		t.Fatalf("own logging: %q", reasons)
	}
}
