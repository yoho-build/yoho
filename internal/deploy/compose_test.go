package deploy

import (
	"context"
	"io"
	"testing"

	"github.com/yoho-build/yoho/internal/migrate"
	"github.com/yoho-build/yoho/internal/remote"
	"strings"
	"time"
)

func TestComposeDeployWithCallerLock(t *testing.T) {
	withRoot(t)
	ctx := context.Background()
	h := &dockerHost{local: &remote.Local{}, respond: respond(nil)}
	d := testDeploy(t, h, io.Discard)
	d.Project = loadProject(t, "services:\n  worker:\n    image: busybox\n    deploy: {replicas: 0}\n")
	d.Ext = nil
	d.ServiceSecrets = nil
	unlock, err := AcquireLock(ctx, h, d.App, d.Destination, LockInfo{Command: "deploy --cutover"})
	if err != nil {
		t.Fatal(err)
	}
	defer unlock(ctx)
	j := migrate.NewJournal(d.App, d.Destination, "", time.Now())
	j.State = migrate.StateDone
	j.Retired = []migrate.RetiredContainer{{ID: "retired1", Name: "old-worker", RestartPolicy: "unless-stopped"}}
	if err := j.Save(ctx, h); err != nil {
		t.Fatal(err)
	}
	if _, err := (Compose{LockHeld: true}).Deploy(ctx, d); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.all(), "docker update '--restart=unless-stopped' 'retired1'") {
		t.Fatal("did not restore restart policy before Compose could reuse containers")
	}
	if _, err := AcquireLock(ctx, h, d.App, d.Destination, LockInfo{}); err == nil {
		t.Fatal("Compose released caller's lock")
	}
}
