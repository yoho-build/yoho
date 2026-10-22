package gate

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

const plainCompose = "services:\n  app:\n    image: busybox:latest\n"

func TestDeploySurvivesDroppedClient(t *testing.T) {
	setup(t, "")
	writeSettings(t, Settings{Server: "srv"})
	started := make(chan struct{})
	unblock := make(chan struct{})
	old := composeDeploy
	t.Cleanup(func() { composeDeploy = old })
	composeDeploy = func(_ context.Context, d *plan.Deploy) (*release.Release, error) {
		close(started)
		<-unblock
		rel := &release.Release{
			App: d.App, Destination: d.Destination, Version: d.Version,
			Status: "deployed", Server: "srv", Runtime: "compose",
		}
		dir := release.Dir(d.App, d.Destination, d.Version)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(rel)
		if err := os.WriteFile(filepath.Join(dir, "release.json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
		if d.Out != nil {
			io.WriteString(d.Out, "cutover done\n")
		}
		return rel, nil
	}

	req, err := remote.EncodeGateRequest("shop", "production", VerbDeploy, DeployArgs{Version: "v1", Compose: plainCompose})
	if err != nil {
		t.Fatal(err)
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() {
		inW.Write(req)
	}()
	go io.Copy(io.Discard, outR)

	done := make(chan int, 1)
	go func() {
		done <- Serve(context.Background(), Options{
			Member: "alice", Key: "SHA256:k", Version: "test",
			In: inR, Out: outW,
			HostFor: func(string) (remote.Host, error) { return &fakeHost{}, nil },
		})
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("rollout did not start")
	}
	outW.Close()
	inW.Close()

	select {
	case code := <-done:
		if code != ExitOK {
			t.Fatalf("parent exit %d, want 0 after detach", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("parent Serve did not return after the client dropped")
	}
	close(unblock)

	relPath := filepath.Join(release.Dir("shop", "production", "v1"), "release.json")
	deadline := time.Now().Add(5 * time.Second)
	var rel release.Release
	for {
		b, err := os.ReadFile(relPath)
		st, _ := readRolloutState("shop", "production")
		if err == nil && json.Unmarshal(b, &rel) == nil && rel.Status == "deployed" &&
			st != nil && st.Status == "deployed" && len(st.Result) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("release not recorded (status %q state %+v): %v", rel.Status, st, err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	var got release.Release
	if err := client(t, "alice", &fakeHost{}).Call(context.Background(), VerbDeploy, DeployArgs{Version: "v1", Attach: true}, nil, io.Discard, &got); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if got.Status != "deployed" || got.Version != "v1" {
		t.Fatalf("attach release %+v", got)
	}
}

func TestAttachMissingRollout(t *testing.T) {
	setup(t, "")
	writeSettings(t, Settings{Server: "srv"})
	err := client(t, "alice", &fakeHost{}).Call(context.Background(), VerbDeploy, DeployArgs{Version: "v9", Compose: plainCompose, Attach: true}, nil, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "no rollout of v9") {
		t.Fatalf("got %v", err)
	}
}

func TestParseRolloutRef(t *testing.T) {
	app, dest, err := parseRolloutRef("shop/production")
	if err != nil || app != "shop" || dest != "production" {
		t.Fatalf("%s %s %v", app, dest, err)
	}
	if _, _, err := parseRolloutRef("bad"); err == nil {
		t.Fatal("want error")
	}
}
