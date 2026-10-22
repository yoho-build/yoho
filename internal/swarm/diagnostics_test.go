package swarm

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/remote"
)

type diagnosticStreamHost struct {
	*fakeHost
	t      *testing.T
	script string
}

func (h *diagnosticStreamHost) Run(ctx context.Context, c remote.Cmd) error {
	h.script = c.Script
	if ctx.Err() != nil {
		h.t.Fatal("diagnostics inherited cancelled parent")
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > 31*time.Second {
		h.t.Fatal("diagnostics have no finite deadline")
	}
	if c.Stderr == nil {
		h.t.Fatal("unbounded diagnostic stderr")
	}
	for _, piece := range []string{"task failure: quoted\"", "secret\nline-secret\n", strings.Repeat("x", 100000)} {
		if _, err := io.WriteString(c.Stdout, piece); err != nil {
			return err
		}
	}
	return nil
}
func TestSwarmDiagnosticsBoundedRedactedAndDetached(t *testing.T) {
	h := &diagnosticStreamHost{fakeHost: &fakeHost{}, t: t}
	r := runner{h: h, secretVals: []string{"quoted\"secret\nline-secret"}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	services := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}
	text := r.diagnose(ctx, "stack", services)
	if strings.Contains(text, "quoted") || strings.Contains(text, "line-secret") || len(text) > 4300 {
		t.Fatalf("unsafe/unbounded diagnostics: %q", text)
	}
	if strings.Contains(h.script, "'i'") || strings.Contains(h.script, "head -n 5") || strings.Contains(h.script, "inspect") {
		t.Fatalf("unexpected diagnostic script: %s", h.script)
	}
}
func TestSwarmRotatedSecretHistoryAndRollbackMetadataOnly(t *testing.T) {
	for _, operation := range []string{"deploy-history", "rollback"} {
		t.Run(operation, func(t *testing.T) {
			withRoot(t)
			fastPolling(t)
			oldToken, newToken := "old-token-retained-task-secret", "new-token-current-task-secret"
			sim := newSim()
			h := newFake(sim)
			var out bytes.Buffer
			d := testDeploy(t, h, &out)
			d.ServiceSecrets["web"]["SECRET_KEY_BASE"] = oldToken
			if _, err := (Runtime{}).Deploy(context.Background(), d); err != nil {
				t.Fatal(err)
			}
			out.Reset()
			d.ServiceSecrets["web"]["SECRET_KEY_BASE"] = newToken
			sim.failMain = true
			h.respond = func(script string) (string, error) {
				if strings.Contains(script, "{{.Error}}") || strings.Contains(script, "{{.Status.Err}}") || strings.Contains(script, "{{.Message}}") || strings.Contains(script, "docker service logs") {
					t.Fatalf("requested unverified arbitrary task text: %s", script)
				}
				if strings.Contains(script, "docker service ps --no-trunc") {
					return "yoho-shop-production_web.1 on node-1: Failed 3 seconds ago " + oldToken + "\n", nil
				}
				text, err := sim.respond(script)
				// Exercise legacy/overbroad status output as well: UpdateMessage must
				// remain internal, never join the public failure or recovery warning.
				return strings.ReplaceAll(text, "update rolled back due to failure", oldToken), err
			}
			var err error
			if operation == "rollback" {
				_, err = (Runtime{}).Rollback(context.Background(), d, "v1")
			} else {
				d.Version = "v2"
				_, err = (Runtime{}).Deploy(context.Background(), d)
			}
			if err == nil || !strings.Contains(err.Error(), "rollback_completed") {
				t.Fatalf("metadata/gate failure lost: %v", err)
			}
			text := out.String() + err.Error()
			if strings.Contains(text, oldToken) || strings.Contains(text, newToken) {
				t.Fatalf("rotated task secret leaked: %s", text)
			}
			if operation == "deploy-history" && !strings.Contains(out.String(), "retained-spec recovery failed") {
				t.Fatal("automatic restore failure path not exercised")
			}
		})
	}
}

func TestSwarmAutoRestoreCallbackAndCommandErrorsDoNotExposeRetainedSecrets(t *testing.T) {
	withRoot(t)
	fastPolling(t)
	oldToken, newToken := "old-token-in-command-and-recovery-error", "new-token-current-inventory"
	sim := newSim()
	h := newFake(sim)
	var out bytes.Buffer
	d := testDeploy(t, h, &out)
	d.ServiceSecrets["web"]["SECRET_KEY_BASE"] = oldToken
	if _, err := (Runtime{}).Deploy(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	d.Version = "v2"
	d.ServiceSecrets["web"]["SECRET_KEY_BASE"] = newToken
	cause := errors.New(oldToken)
	h.respond = func(script string) (string, error) {
		if strings.Contains(script, "docker stack deploy") && strings.Contains(script, "/v2/compose.yaml") {
			return oldToken, cause
		}
		if strings.Contains(script, "kamal-proxy' 'deploy'") {
			return "", errors.New(oldToken)
		}
		if strings.Contains(script, "docker service ps --no-trunc") {
			return "yoho-shop-production_web.1 on node-1: Failed " + oldToken, nil
		}
		return sim.respond(script)
	}
	_, err := (Runtime{}).Deploy(context.Background(), d)
	if !errors.Is(err, cause) || !strings.Contains(out.String(), "retained-spec recovery failed") {
		t.Fatalf("cause/recovery path lost: %v %s", err, out.String())
	}
	if strings.Contains(err.Error()+out.String(), oldToken) || strings.Contains(err.Error()+out.String(), newToken) {
		t.Fatal("command or restore callback leaked retained secret")
	}
}

func TestSwarmStatusErrorPreservesInternalCauseWithoutPublicSecret(t *testing.T) {
	cause := errors.New("retained-unknown-token")
	h := &fakeHost{local: &remote.Local{}, respond: func(string) (string, error) { return "", cause }}
	r := runner{h: h, out: io.Discard, secretVals: []string{"todays-token"}}
	_, err := r.serviceStatus(context.Background(), "app")
	if !errors.Is(err, cause) || strings.Contains(err.Error(), cause.Error()) {
		t.Fatalf("unsafe status error or lost cause: %v", err)
	}
}

func TestSwarmStackCommandPartialFailureIncludesPlannedUnknownFacts(t *testing.T) {
	cause := errors.New("old-retained-token-command-error")
	h := &fakeHost{local: &remote.Local{}, respond: func(script string) (string, error) {
		if strings.Contains(script, "docker stack deploy") {
			return cause.Error(), cause
		}
		if strings.Contains(script, "docker service ls -q --filter") {
			return "S|app_worker|t1|completed|\nR|app_worker|1/1", nil
		}
		return "app_worker.1 on node-1: Failed " + cause.Error(), nil
	}}
	r := runner{h: h, out: io.Discard, secretVals: []string{"current-token"}}
	err := r.deployStack(context.Background(), "compose.yaml", "app", true, false, "web", "worker", "new")
	var failure *deploy.Failure
	if !errors.Is(err, cause) || !errors.As(err, &failure) || failure.Phase != "swarm-stack-deploy" || len(failure.Services) != 3 {
		t.Fatalf("missing partial facts/cause: %v", err)
	}
	for _, fact := range failure.Services {
		if fact.Convergence != "attempted/unknown replicas=unknown" {
			t.Fatalf("false completion: %+v", failure.Services)
		}
	}
	if strings.Contains(err.Error(), cause.Error()) {
		t.Fatal("unverified command error was published")
	}
}

func TestSwarmStatusFailureAndCancellationKeepUnknownServiceFacts(t *testing.T) {
	for _, cause := range []error{context.Canceled, errors.New("old-retained-token-status-error")} {
		t.Run(cause.Error(), func(t *testing.T) {
			h := &fakeHost{local: &remote.Local{}, respond: func(script string) (string, error) {
				if strings.Contains(script, "docker service ls -q --filter") {
					return "", cause
				}
				return "app_worker.1 on node-1: Failed old-retained-token", nil
			}}
			r := runner{h: h, out: io.Discard, secretVals: []string{"current-token"}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if cause == context.Canceled {
				cancel()
			}
			err := r.waitConverged(ctx, "app", map[string]svcStatus{"app_worker": {UpdateState: "completed", Replicas: "2/2"}}, "worker", "new")
			var failure *deploy.Failure
			if !errors.Is(err, cause) || !errors.As(err, &failure) || failure.Phase != "swarm-convergence" || len(failure.Services) != 2 {
				t.Fatalf("missing status/cancellation facts: %v", err)
			}
			for _, fact := range failure.Services {
				if fact.Convergence != "attempted/unknown replicas=unknown" {
					t.Fatalf("stale completion counts: %+v", failure.Services)
				}
			}
			if strings.Contains(err.Error(), "old-retained-token") {
				t.Fatal("status/task history secret published")
			}
		})
	}
}

func TestSwarmPreCommandStatusFailuresReportPlannedNotAttemptedFacts(t *testing.T) {
	for _, operation := range []string{"stack", "release-prerequisites"} {
		for _, cause := range []error{errors.New("retained-secret-in-preflight-error"), context.Canceled} {
			t.Run(operation+"/"+cause.Error(), func(t *testing.T) {
				h := &fakeHost{local: &remote.Local{}, respond: func(script string) (string, error) {
					if !strings.Contains(script, "docker service ls -q --filter") || !strings.Contains(script, "docker service inspect") {
						t.Fatalf("preflight failure issued an update/job/diagnostic command: %s", script)
					}
					return "", cause
				}}
				r := runner{h: h, out: io.Discard, stack: "app", secretVals: []string{"current-secret"}}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if cause == context.Canceled {
					cancel()
				}
				var err error
				wantPhase := "swarm-stack-preflight-status"
				if operation == "stack" {
					err = r.deployStack(ctx, "compose.yaml", "app", true, false, "web", "db", "worker")
				} else {
					wantPhase = "swarm-release-prerequisites-status"
					c := &compiled{Plan: stackPlan{Stack: "app", Services: []servicePlan{{Name: "web", ReleaseCommand: []string{"migrate"}, DependsOn: []string{"db"}}, {Name: "db", Stateful: true}, {Name: "worker"}}}}
					err = r.releaseCommands(ctx, c, t.TempDir(), false)
				}
				var failure *deploy.Failure
				if !errors.Is(err, cause) || !errors.As(err, &failure) || failure.Phase != wantPhase || len(failure.Services) != 3 {
					t.Fatalf("missing pre-command phase/facts/cause: %v", err)
				}
				names := map[string]bool{}
				for _, fact := range failure.Services {
					names[fact.Service] = true
					if fact.Convergence != "not-attempted/not-observed" || fact.ProxyRoute != "not-attempted" {
						t.Fatalf("pre-query claimed an update/convergence: %+v", failure.Services)
					}
				}
				for _, name := range []string{"app_web", "app_db", "app_worker"} {
					if !names[name] {
						t.Fatalf("planned Service missing: %s", name)
					}
				}
				if strings.Contains(err.Error(), "retained-secret") || strings.Contains(err.Error(), "attempted/unknown") {
					t.Fatal("unverified text or false attempted claim")
				}
				if len(h.scripts) != 1 || strings.Contains(h.all(), "docker stack deploy") || strings.Contains(h.all(), "docker stack rm") || strings.Contains(h.all(), "docker service ps -q") {
					t.Fatalf("unexpected pre-command mutation/job: %s", h.all())
				}
			})
		}
	}
}

func TestSwarmConvergenceFailureFacts(t *testing.T) {
	cause := context.Canceled
	r := runner{}
	err := r.convergenceFailure(cause, map[string]svcStatus{"worker": {UpdateState: "paused", Replicas: "0/1"}, "web": {UpdateState: "completed", Replicas: "2/2"}})
	var failure *deploy.Failure
	if !errors.Is(err, cause) || !errors.As(err, &failure) || len(failure.Services) != 2 || failure.Phase != "swarm-convergence" {
		t.Fatalf("missing facts/cause: %v", err)
	}
	if strings.Contains(err.Error(), "previous version keeps serving") {
		t.Fatal("App-wide rollback promise")
	}
}

// poolStackHost prints Docker's pool exhaustion line (merged by 2>&1) on a
// docker stack deploy, next to a secret, then fails it.
type poolStackHost struct{ *fakeHost }

func (p *poolStackHost) Run(ctx context.Context, c remote.Cmd) error {
	if strings.Contains(c.Script, "docker stack deploy") && c.Stdout != nil {
		_, _ = io.WriteString(c.Stdout, "token current-token\nfailed to create network app_default: Error response from daemon: all predefined address pools have been fully subnetted current-token\n")
		return &remote.ExitError{Host: "s1", Code: 1}
	}
	return p.fakeHost.Run(ctx, c)
}

func TestSwarmStackDeployPoolExhaustionDetected(t *testing.T) {
	h := &poolStackHost{&fakeHost{local: &remote.Local{}, respond: func(string) (string, error) { return "", nil }}}
	r := runner{h: h, out: io.Discard, secretVals: []string{"current-token"}}
	err := r.deployStack(context.Background(), "compose.yaml", "app", true, false, "web")
	if !deploy.IsPoolExhausted(err) {
		t.Fatalf("pool exhaustion missed: %v", err)
	}
	var ee *remote.ExitError
	if !errors.As(err, &ee) || strings.Contains(ee.Stderr, "current-token") || strings.Contains(err.Error(), "current-token") {
		t.Fatalf("secret leaked: %v / %q", err, ee.Stderr)
	}
}
