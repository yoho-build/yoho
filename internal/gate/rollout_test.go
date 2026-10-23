package gate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
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

	req, err := remote.EncodeGateRequest("shop", "production", VerbDeploy, DeployArgs{Version: "v1", Compose: plainCompose, Rollout: "r1"})
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
	if err := client(t, "alice", &fakeHost{}).Call(context.Background(), VerbDeploy, DeployArgs{Version: "v1", Rollout: "r1", Attach: true}, nil, io.Discard, &got); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if got.Status != "deployed" || got.Version != "v1" {
		t.Fatalf("attach release %+v", got)
	}
	// A late attach still finds r1's outcome after another rollout took
	// state.json.
	if err := writeRolloutState("shop", "production", &rolloutState{ID: "r3", Verb: VerbDeploy, Version: "v3", Status: "failed"}); err != nil {
		t.Fatal(err)
	}
	got = release.Release{}
	if err := client(t, "alice", &fakeHost{}).Call(context.Background(), VerbDeploy, DeployArgs{Version: "v1", Rollout: "r1", Attach: true}, nil, io.Discard, &got); err != nil || got.Status != "deployed" {
		t.Fatalf("late attach: %+v %v", got, err)
	}
	// Another attempt at v1 whose session dropped before it started must
	// not report r1's outcome as its own.
	err = client(t, "alice", &fakeHost{}).Call(context.Background(), VerbDeploy, DeployArgs{Version: "v1", Rollout: "r2", Attach: true}, nil, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "did not start") {
		t.Fatalf("attach to another rollout: %v", err)
	}
}

func TestRolloutRefusesWhileAnotherRuns(t *testing.T) {
	setup(t, "")
	writeSettings(t, Settings{Server: "srv"})
	st := &rolloutState{ID: "r1", Verb: VerbDeploy, Version: "v1", Member: "bob", PID: os.Getpid(), Status: rolloutStatusRunning}
	if err := writeRolloutState("shop", "production", st); err != nil {
		t.Fatal(err)
	}
	err := client(t, "alice", &fakeHost{}).Call(context.Background(), VerbDeploy, DeployArgs{Version: "v2", Compose: plainCompose, Rollout: "r2"}, nil, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "another deploy is running") {
		t.Fatalf("got %v", err)
	}
	if got, _ := readRolloutState("shop", "production"); got == nil || got.ID != "r1" {
		t.Fatalf("state replaced: %+v", got)
	}
}

func TestAttachDetectsDeadWorker(t *testing.T) {
	setup(t, "")
	writeSettings(t, Settings{Server: "srv"})
	old := gateRunning
	t.Cleanup(func() { gateRunning = old })
	gateRunning = func(int) bool { return false }
	st := &rolloutState{ID: "r1", Verb: VerbDeploy, Version: "v1", PID: 999999, Status: rolloutStatusRunning}
	if err := writeRolloutState("shop", "production", st); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- client(t, "alice", &fakeHost{}).Call(context.Background(), VerbDeploy, DeployArgs{Version: "v1", Rollout: "r1", Attach: true}, nil, io.Discard, nil)
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "stopped without recording a result") {
			t.Fatalf("got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("attach waits on a dead worker")
	}
}

func TestAttachMissingRollout(t *testing.T) {
	setup(t, "")
	writeSettings(t, Settings{Server: "srv"})
	err := client(t, "alice", &fakeHost{}).Call(context.Background(), VerbDeploy, DeployArgs{Version: "v9", Compose: plainCompose, Rollout: "r9", Attach: true}, nil, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "did not start") {
		t.Fatalf("got %v", err)
	}
}

func TestAttachWaitsWhilePreparing(t *testing.T) {
	setup(t, "")
	writeSettings(t, Settings{Server: "srv"})
	// The Gate that took r1 is still pulling: this process stands in for it.
	st := &rolloutState{ID: "r1", Verb: VerbDeploy, Version: "v1", Member: "alice", PID: os.Getpid(), Status: rolloutStatusPreparing}
	if err := writeRolloutState("shop", "production", st); err != nil {
		t.Fatal(err)
	}
	// The log is the previous rollout's until r1 runs.
	if err := os.WriteFile(rolloutLogPath("shop", "production"), []byte(`{"out":"old rollout\n"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	done := make(chan error, 1)
	go func() {
		done <- client(t, "alice", &fakeHost{}).Call(context.Background(), VerbDeploy, DeployArgs{Version: "v1", Rollout: "r1", Attach: true}, nil, &out, nil)
	}()
	select {
	case err := <-done:
		t.Fatalf("attach returned while preparing: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	// A new deploy is refused meanwhile.
	err := client(t, "alice", &fakeHost{}).Call(context.Background(), VerbDeploy, DeployArgs{Version: "v2", Compose: plainCompose, Rollout: "r2"}, nil, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "another deploy is running") {
		t.Fatalf("concurrent deploy: %v", err)
	}
	prep := &call{req: &remote.GateRequest{App: "shop", Destination: "production"}}
	prep.abandonRollout("r1", errors.New("pull app:v1: boom"))
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("attach did not see the outcome")
	}
	if strings.Contains(out.String(), "old rollout") {
		t.Fatalf("replayed the previous rollout's log: %q", out.String())
	}
}

func TestFailedPrepareRecordsOutcome(t *testing.T) {
	setup(t, "")
	// No settings: the deploy fails after it was prepared.
	err := client(t, "alice", &fakeHost{}).Call(context.Background(), VerbDeploy, DeployArgs{Version: "v1", Compose: plainCompose, Rollout: "r1"}, nil, io.Discard, nil)
	if err == nil {
		t.Fatal("want error")
	}
	st, _ := readRolloutState("shop", "production")
	if st == nil || st.ID != "r1" || st.Status != "failed" {
		t.Fatalf("state %+v", st)
	}
	aerr := client(t, "alice", &fakeHost{}).Call(context.Background(), VerbDeploy, DeployArgs{Version: "v1", Rollout: "r1", Attach: true}, nil, io.Discard, nil)
	if aerr == nil || strings.Contains(aerr.Error(), "did not start") || aerr.Error() != err.Error() {
		t.Fatalf("attach %v, deploy %v", aerr, err)
	}
}

func TestRolloutAliveRejectsReusedPID(t *testing.T) {
	if procStart(os.Getpid()) == "" {
		t.Skip("no /proc")
	}
	old := gateRunning
	t.Cleanup(func() { gateRunning = old })
	gateRunning = func(int) bool { return true }
	cmd := exec.Command("sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	pid := cmd.Process.Pid
	if !rolloutAlive(&rolloutState{PID: pid, Start: procStart(pid)}, "") {
		t.Fatal("live worker reported dead")
	}
	if rolloutAlive(&rolloutState{PID: pid, Start: "1"}, "") {
		t.Fatal("reused pid reported alive")
	}
	// A Gate given the pid of a dead one does not take its rollout for itself.
	if rolloutAlive(&rolloutState{PID: os.Getpid(), Start: "1"}, "") {
		t.Fatal("own reused pid reported alive")
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
