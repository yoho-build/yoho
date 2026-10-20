package gate

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// YOHO_E2E=1 with a local Docker: deploy, list and roll back an App through
// the Gate on remote.Local, the way sshd runs it on a Server.
func TestE2EGateDeploy(t *testing.T) {
	if os.Getenv("YOHO_E2E") != "1" {
		t.Skip("YOHO_E2E=1 not set")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	setup(t, "members:\n  alice: {verbs: [deploy, rollback, releases, ps, logs, dash]}\n")
	writeSettings(t, Settings{Server: "local"})
	project := release.ProjectName("shop", "production")
	t.Cleanup(func() {
		exec.Command("docker", "compose", "-p", project, "down", "--remove-orphans").Run()
	})
	g := client(t, "alice", &remote.Local{HostName: "local"})
	ctx := context.Background()
	compose := func(msg string) string {
		return "services:\n  app:\n    image: busybox:latest\n    command: [sh, -c, 'echo " + msg + "; sleep 300']\n"
	}
	for _, v := range []string{"v1", "v2"} {
		var rel release.Release
		var out bytes.Buffer
		if err := g.Call(ctx, VerbDeploy, DeployArgs{Version: v, Compose: compose(v)}, nil, &out, &rel); err != nil {
			t.Fatalf("deploy %s: %v\n%s", v, err, out.String())
		}
		if rel.Version != v || rel.Status != "deployed" || !strings.HasPrefix(rel.Performer, "alice (gate)") {
			t.Fatalf("release %+v", rel)
		}
	}
	var rels []release.Release
	if err := g.Call(ctx, VerbReleases, nil, nil, nil, &rels); err != nil || len(rels) != 2 || rels[0].Version != "v2" {
		t.Fatalf("releases %v %+v", err, rels)
	}
	var out bytes.Buffer
	if err := g.Call(ctx, VerbPS, nil, nil, &out, nil); err != nil || !strings.Contains(out.String(), project+"-app-1") {
		t.Fatalf("ps %v %q", err, out.String())
	}
	if err := g.Call(ctx, VerbRollback, RollbackArgs{Version: "v1"}, nil, io.Discard, nil); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	var s struct{ Containers []struct{ Service string } }
	if err := g.Call(ctx, VerbDash, nil, nil, nil, &s); err != nil || len(s.Containers) == 0 {
		t.Fatalf("dash %v %+v", err, s)
	}
	evil := "services:\n  app:\n    image: busybox:latest\n    volumes: [\"/var/run/docker.sock:/var/run/docker.sock\"]\n"
	if err := g.Call(ctx, VerbDeploy, DeployArgs{Version: "v3", Compose: evil}, nil, io.Discard, nil); err == nil || !strings.Contains(err.Error(), "docker.sock") {
		t.Fatalf("socket deploy: %v", err)
	}
}
