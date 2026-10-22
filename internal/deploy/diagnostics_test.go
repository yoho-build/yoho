package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/secrets"
)

// Unlike Output-only mocks, this exercises the actual streaming writer contract.
type diagnosticHost struct {
	*remote.Local
	run     func(context.Context, remote.Cmd) error
	output  func(context.Context, remote.Cmd) (string, error)
	scripts []string
}

func (h *diagnosticHost) Run(ctx context.Context, c remote.Cmd) error {
	h.scripts = append(h.scripts, c.Script)
	if h.run != nil {
		return h.run(ctx, c)
	}
	return nil
}
func (h *diagnosticHost) Output(ctx context.Context, c remote.Cmd) (string, error) {
	h.scripts = append(h.scripts, c.Script)
	if h.output != nil {
		out, err := h.output(ctx, c)
		if out != "" || err != nil || !strings.Contains(c.Script, "kamal-proxy list --json") {
			return out, err
		}
	}
	if strings.Contains(c.Script, "kamal-proxy list --json") {
		return "null", nil
	}
	return "", nil
}

func TestDiagnosticsDecodeRedactAndBound(t *testing.T) {
	secret := "quote\"secret\nnext-secret-line"
	for _, invalid := range []bool{false, true} {
		t.Run(map[bool]string{false: "decoded", true: "invalid"}[invalid], func(t *testing.T) {
			var out bytes.Buffer
			state := map[string]any{"status": "exited", "exit_code": 137, "oom_killed": true, "error": secret, "health": map[string]any{"Status": "unhealthy", "Log": []map[string]any{{"ExitCode": 1, "Output": secret + strings.Repeat("x", 3000)}}}}
			raw, _ := json.Marshal(state)
			// A legal alternative JSON spelling must be decoded before masking.
			raw = bytes.ReplaceAll(raw, []byte("next-secret"), append([]byte{92}, []byte("u006eext-secret")...))
			if invalid {
				raw = append(raw[:len(raw)-1], []byte(strings.Repeat("y", 20000))...)
			}
			h := &diagnosticHost{Local: &remote.Local{}, run: func(ctx context.Context, c remote.Cmd) error {
				if ctx.Err() != nil {
					t.Fatal("diagnostics inherited cancellation")
				}
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 31*time.Second {
					t.Fatal("unbounded diagnostics")
				}
				if c.Stderr == nil {
					t.Fatal("diagnostic stderr could escape")
				}
				if strings.Contains(c.Script, ".Config") || strings.Contains(c.Script, ".Env") {
					t.Fatal("sensitive inspect")
				}
				if strings.Contains(c.Script, "docker inspect") {
					_, err := c.Stdout.Write(raw)
					return err
				}
				if strings.Contains(c.Script, "docker logs --tail 20") {
					for _, piece := range []string{"safe\nquote\"sec", "ret\nnext-secret-line\n", strings.Repeat("z", 100000)} {
						if _, err := io.WriteString(c.Stdout, piece); err != nil {
							return err
						}
					}
					return nil
				}
				t.Fatalf("unexpected script %s", c.Script)
				return nil
			}, output: func(context.Context, remote.Cmd) (string, error) {
				t.Fatal("diagnostics used unbounded Output")
				return "", nil
			}}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			r := runner{h: h, out: &out, secretVals: []string{secret}}
			r.diagnose(ctx, "web", []string{"id"})
			text := out.String()
			for _, leak := range []string{"quote\"secret", "next-secret-line", `next-secret`, "quote\\\"secret"} {
				if strings.Contains(text, leak) {
					t.Fatalf("secret leaked: %q", leak)
				}
			}
			if len(text) > 33000 {
				t.Fatalf("output unbounded: %d", len(text))
			}
			if invalid {
				if !strings.Contains(text, "payload omitted") || strings.Contains(text, "yyyy") {
					t.Fatal("invalid JSON was emitted")
				}
			} else if !strings.Contains(text, "exit=137 oom=true") || !strings.Contains(text, "health exit=1 output=***") {
				t.Fatalf("missing diagnostics: %s", text)
			}
		})
	}
}

func TestDiagnosticContainerAndTotalOutputCaps(t *testing.T) {
	var out bytes.Buffer
	calls := 0
	h := &diagnosticHost{Local: &remote.Local{}, run: func(_ context.Context, c remote.Cmd) error {
		if strings.Contains(c.Script, "docker inspect") {
			calls++
			raw, _ := json.Marshal(map[string]any{"status": "exited", "exit_code": 1, "error": strings.Repeat("x", 10000)})
			_, err := c.Stdout.Write(raw)
			return err
		}
		_, err := io.WriteString(c.Stdout, strings.Repeat("z", 3000))
		return err
	}}
	r := runner{h: h, out: &out}
	r.diagnose(context.Background(), "web", []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10"})
	if calls != 8 || out.Len() > 33000 || !strings.Contains(out.String(), "output truncated") {
		t.Fatalf("caps: calls=%d output=%d", calls, out.Len())
	}
}

func TestRollbackDiagnosticsOmitUnredactableText(t *testing.T) {
	var out bytes.Buffer
	h := &diagnosticHost{Local: &remote.Local{}, run: func(_ context.Context, c remote.Cmd) error {
		if strings.Contains(c.Script, ".State.Error") || strings.Contains(c.Script, ".State.Health") || strings.Contains(c.Script, "docker logs") {
			t.Fatal("requested arbitrary retained-secret text")
		}
		_, err := io.WriteString(c.Stdout, `{"status":"exited","exit_code":7,"oom_killed":false}`)
		return err
	}}
	r := runner{h: h, out: &out, omitDiagnosticText: true}
	r.diagnose(context.Background(), "web", []string{"id"})
	if !strings.Contains(out.String(), "exit=7") || !strings.Contains(out.String(), "retained secret values") {
		t.Fatalf("missing safe rollback diagnostics: %s", out.String())
	}
}

func TestCappedBufferDoesNotExposeUnboundedFastPaths(t *testing.T) {
	b := &cappedBuffer{limit: 8}
	if _, ok := any(b).(io.ReaderFrom); ok {
		t.Fatal("ReadFrom bypasses limit")
	}
	if _, ok := any(b).(io.StringWriter); ok {
		t.Fatal("WriteString bypasses limit")
	}
	_, err := io.Copy(b, strings.NewReader(strings.Repeat("x", 100000)))
	if !errors.Is(err, errDiagnosticLimit) || b.Len() != 8 {
		t.Fatalf("unbounded copy: %d %v", b.Len(), err)
	}
}

func TestLogCaptureDoesNotFlushPartialSecretAtCutoff(t *testing.T) {
	secret := "unique-secret-abcdefgh"
	for _, rawLimit := range []int{8, 64} {
		for _, outputLimit := range []int{3, 12} {
			sink := &cappedBuffer{limit: outputLimit}
			capture := &logCapture{sink: sink, remaining: rawLimit}
			capture.redact = secrets.NewDiagnosticRedactor(discardOverflow{sink}, secret)
			_, err := capture.Write([]byte("ok " + secret + " tail"))
			if err == nil && !capture.cut {
				_ = capture.redact.Flush()
			}
			if strings.Contains(sink.String(), "unique") {
				t.Fatalf("partial secret leaked with limits %d/%d: %q", rawLimit, outputLimit, sink.String())
			}
			if sink.Len() > outputLimit {
				t.Fatal("sink exceeded cap")
			}
		}
	}
	// Interruption after an unresolved prefix must also leave it unpublished.
	h := &diagnosticHost{Local: &remote.Local{}, run: func(_ context.Context, c remote.Cmd) error {
		_, _ = io.WriteString(c.Stdout, "safe unique-secret-")
		return context.Canceled
	}}
	text, err := DiagnosticText(context.Background(), h, "test", []string{secret})
	if !errors.Is(err, context.Canceled) || strings.Contains(text, "unique") {
		t.Fatalf("interrupted stream: %q %v", text, err)
	}
}

func TestDiagnosticsFailureUnavailablePreservesCause(t *testing.T) {
	cause := errors.New("readiness failed")
	var out bytes.Buffer
	h := &diagnosticHost{Local: &remote.Local{}, run: func(_ context.Context, c remote.Cmd) error {
		if strings.Contains(c.Script, "docker inspect") {
			_, _ = io.WriteString(c.Stdout, `{"error":"secret-prefix`)
		}
		return errors.New("DO NOT PRINT THIS PAYLOAD")
	}}
	r := runner{h: h, out: &out, phase: "plain-readiness"}
	err := r.failure(context.Background(), cause, "worker", []string{"id"})
	if !errors.Is(err, cause) || strings.Contains(out.String(), "secret-prefix") || strings.Contains(out.String(), "DO NOT PRINT") {
		t.Fatalf("unavailable diagnostics changed/leaked cause: %v %s", err, out.String())
	}
}

func TestFailureDiagnosticsBeforeCandidateCleanup(t *testing.T) {
	for _, failAt := range []string{"proxy", "worker", "switch"} {
		t.Run(failAt, func(t *testing.T) {
			cause := errors.New("test failure")
			var out bytes.Buffer
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h := &diagnosticHost{Local: &remote.Local{}}
			h.output = func(_ context.Context, c remote.Cmd) (string, error) {
				if strings.Contains(c.Script, "kamal-proxy list --json") {
					// An update: the route exists, so it switches last.
					return `{"app-prod-api":{"hosts":[""],"targets":["old:80"]}}`, nil
				}
				if strings.Contains(c.Script, "'--scale'") {
					return "fresh", nil
				}
				if strings.Contains(c.Script, "{{.Name}}") {
					return "/fresh", nil
				}
				return "", nil
			}
			h.run = func(cctx context.Context, c remote.Cmd) error {
				if (failAt == "proxy" && strings.Contains(c.Script, "set -- 'fresh'")) || (failAt == "worker" && strings.Contains(c.Script, "'ps' '--all' '-q' 'worker'")) || (failAt == "switch" && strings.Contains(c.Script, "kamal-proxy' 'deploy")) {
					return cause
				}
				if strings.Contains(c.Script, "docker ps -aq") {
					_, _ = io.WriteString(c.Stdout, "fresh\n")
				}
				if strings.Contains(c.Script, `"oom_killed"`) {
					if cctx.Err() != nil {
						t.Fatal("cancelled diagnostic context")
					}
					_, _ = io.WriteString(c.Stdout, `{"status":"exited","exit_code":1,"oom_killed":false}`)
				}
				if strings.Contains(c.Script, "docker logs") {
					_, _ = io.WriteString(c.Stdout, "captured before removal\n")
				}
				return nil
			}
			r := runner{h: h, out: &out, app: "app", dest: "prod"}
			plans := []servicePlan{{Name: "api", Replicas: 1, Proxy: proxyWithDefaults(&config.ServiceProxy{})}, {Name: "worker", Replicas: 1, DependsOn: []string{"api"}}}
			err := r.cutover(ctx, plans)
			if !errors.Is(err, cause) {
				t.Fatalf("cause lost: %v", err)
			}
			scripts := strings.Join(h.scripts, "\n")
			needle := "docker logs --tail 20"
			if failAt == "worker" {
				needle = `"oom_killed"`
			}
			logs, remove := strings.Index(scripts, needle), strings.Index(scripts, "docker rm -f 'fresh'")
			if logs < 0 || remove < logs {
				t.Fatalf("diagnostics not before cleanup: %s", scripts)
			}
			var failure *Failure
			if !errors.As(err, &failure) || len(failure.Services) != 2 {
				t.Fatalf("missing facts: %v", err)
			}
			if failAt == "worker" && failure.Services[0].ProxyRoute != "not-attempted" {
				t.Fatalf("wrong route facts: %+v", failure.Services)
			}
			if strings.Contains(out.String(), "previous version keeps serving") {
				t.Fatal("App-wide promise")
			}
		})
	}
}

func TestPartialCutoverFactsAfterTwoSwitches(t *testing.T) {
	cause := errors.New("third route failed")
	h := &diagnosticHost{Local: &remote.Local{}}
	h.output = func(_ context.Context, c remote.Cmd) (string, error) {
		for _, name := range []string{"a", "b", "c"} {
			if strings.Contains(c.Script, "'--scale' '"+name+"=1'") {
				return "fresh-" + name, nil
			}
			if strings.Contains(c.Script, "{{.Name}}") && strings.Contains(c.Script, "'fresh-"+name+"'") {
				return "/fresh-" + name, nil
			}
		}
		return "", nil
	}
	h.run = func(_ context.Context, c remote.Cmd) error {
		if strings.Contains(c.Script, "kamal-proxy' 'deploy' 'app-prod-c'") {
			return cause
		}
		return nil
	}
	r := runner{h: h, out: io.Discard, app: "app", dest: "prod"}
	plans := []servicePlan{{Name: "worker", Replicas: 1}}
	for _, name := range []string{"a", "b", "c"} {
		plans = append(plans, servicePlan{Name: name, Replicas: 1, Proxy: proxyWithDefaults(&config.ServiceProxy{})})
	}
	err := r.cutover(context.Background(), plans)
	var failure *Failure
	if !errors.Is(err, cause) || !errors.As(err, &failure) {
		t.Fatalf("missing error: %v", err)
	}
	want := []string{"not-applicable", "switched", "switched", "absent"}
	for i, f := range failure.Services {
		if f.Convergence != "complete" || f.ProxyRoute != want[i] {
			t.Fatalf("wrong partial facts: %+v", failure.Services)
		}
	}
}

func TestRotatedSecretsUncertainSwitchUsesOnlyFreshDiagnostics(t *testing.T) {
	oldToken, newToken := "old-token-not-in-current-inventory", "new-token-current-inventory"
	var out bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := &diagnosticHost{Local: &remote.Local{}}
	h.output = func(_ context.Context, c remote.Cmd) (string, error) {
		switch {
		case strings.Contains(c.Script, "'--scale'"):
			return "old-id\nfresh-id", nil
		case strings.Contains(c.Script, "status=$(docker inspect"):
			return "old-id", nil
		case strings.Contains(c.Script, "{{.Name}}"):
			return "/fresh-id", nil
		case strings.Contains(c.Script, "kamal-proxy list"):
			return `{}`, nil
		}
		return "", nil
	}
	inspected, logged := false, false
	h.run = func(dctx context.Context, c remote.Cmd) error {
		if strings.Contains(c.Script, "kamal-proxy' 'deploy") {
			cancel()
			return context.Canceled
		}
		if strings.Contains(c.Script, "docker ps -aq") {
			t.Fatal("uncertain switch rediscovered retained replicas")
		}
		if strings.Contains(c.Script, `"oom_killed"`) {
			if dctx.Err() != nil {
				t.Fatal("diagnostics received cancelled context")
			}
			if !strings.Contains(c.Script, "'fresh-id'") || strings.Contains(c.Script, "'old-id'") {
				t.Fatal("retained replica inspected for arbitrary text")
			}
			inspected = true
			raw, _ := json.Marshal(map[string]any{"status": "running", "exit_code": 0, "error": newToken, "health": map[string]any{"Status": "unhealthy", "Log": []map[string]any{{"ExitCode": 1, "Output": newToken}}}})
			_, err := c.Stdout.Write(raw)
			return err
		}
		if strings.Contains(c.Script, "docker logs") {
			if !strings.Contains(c.Script, "'fresh-id'") {
				_, _ = io.WriteString(c.Stdout, oldToken)
				t.Fatal("old log requested")
			}
			logged = true
			_, err := io.WriteString(c.Stdout, "fresh diagnostics "+newToken+"\n")
			return err
		}
		if strings.Contains(c.Script, "docker rm -f 'fresh-id'") {
			t.Fatal("uncertain candidate removed")
		}
		return nil
	}
	r := runner{h: h, out: &out, app: "app", dest: "prod", secretVals: []string{newToken}}
	err := r.cutover(ctx, []servicePlan{{Name: "web", Replicas: 1, Proxy: proxyWithDefaults(&config.ServiceProxy{})}})
	if !errors.Is(err, context.Canceled) || !inspected || !logged || !strings.Contains(out.String(), "fresh diagnostics") {
		t.Fatalf("fresh diagnostics/cause lost: %v %s", err, out.String())
	}
	if strings.Contains(out.String()+err.Error(), oldToken) || strings.Contains(out.String()+err.Error(), newToken) {
		t.Fatal("rotated secret leaked")
	}
}

func TestRotatedSecretsPrerequisiteAndPlainFailuresSuppressRetainedText(t *testing.T) {
	for _, phase := range []string{"prerequisite", "plain-readiness", "plain-start"} {
		t.Run(phase, func(t *testing.T) {
			oldToken, newToken := "old-token-retained-generation", "new-token-current-generation"
			cause := errors.New("startup gate failed")
			var out bytes.Buffer
			h := &diagnosticHost{Local: &remote.Local{}, run: func(_ context.Context, c remote.Cmd) error {
				if strings.Contains(c.Script, "'ps' '--all' '-q' 'db'") {
					return cause
				}
				if phase == "plain-start" && strings.Contains(c.Script, "'up' '-d' '--no-deps' 'db'") {
					return cause
				}
				if strings.Contains(c.Script, "docker ps -aq") {
					_, _ = io.WriteString(c.Stdout, "old-id\n")
					return nil
				}
				if strings.Contains(c.Script, `"oom_killed"`) {
					if strings.Contains(c.Script, ".State.Error") || strings.Contains(c.Script, ".State.Health") {
						t.Fatal("requested retained arbitrary text")
					}
					// Even an overbroad response must not make unknown Error/health fields
					// viewer-facing when this replica's inventory is unverified.
					raw, _ := json.Marshal(map[string]any{"status": "exited", "exit_code": 17, "oom_killed": true, "error": oldToken, "health": map[string]any{"Status": "unhealthy", "Log": []map[string]any{{"ExitCode": 1, "Output": oldToken}}}})
					_, err := c.Stdout.Write(raw)
					return err
				}
				if strings.Contains(c.Script, "docker logs") {
					_, _ = io.WriteString(c.Stdout, oldToken)
					t.Fatal("requested retained logs")
				}
				return nil
			}}
			r := runner{h: h, out: &out, secretVals: []string{newToken}}
			var err error
			if phase == "prerequisite" {
				err = r.startDependencies(context.Background(), []servicePlan{{Name: "db", Replicas: 1}, {Name: "web", Replicas: 1, DependsOn: []string{"db"}, ReleaseCommand: []string{"migrate"}}})
			} else {
				err = r.cutover(context.Background(), []servicePlan{{Name: "db", Replicas: 1}})
			}
			if !errors.Is(err, cause) || !strings.Contains(out.String(), "exit=17 oom=true") || !strings.Contains(out.String(), "output omitted") {
				t.Fatalf("safe metadata/cause lost: %v %s", err, out.String())
			}
			if strings.Contains(out.String()+err.Error(), oldToken) || strings.Contains(out.String()+err.Error(), newToken) {
				t.Fatal("retained secret leaked")
			}
		})
	}
}

func TestPrerequisiteBatchDiagnosticsShareBudgetsAndSampleBothServices(t *testing.T) {
	cause := errors.New("batch startup failed")
	oldToken, newToken := "retained-old-token", "current-new-token"
	var out bytes.Buffer
	sampled := map[string]int{}
	var deadline time.Time
	h := &diagnosticHost{Local: &remote.Local{}, run: func(ctx context.Context, c remote.Cmd) error {
		if strings.Contains(c.Script, "'up' '-d' '--no-recreate'") {
			return cause
		}
		d, ok := ctx.Deadline()
		if !ok || ctx.Err() != nil || time.Until(d) > 31*time.Second {
			t.Fatal("unbounded/cancelled batch diagnostic context")
		}
		if deadline.IsZero() {
			deadline = d
		} else if !deadline.Equal(d) {
			t.Fatal("diagnostic Services got separate time budgets")
		}
		if strings.Contains(c.Script, "docker ps -aq") {
			service := "db"
			if strings.Contains(c.Script, "service=api") {
				service = "api"
			}
			ids := []string{}
			for i := 0; i < 10; i++ {
				ids = append(ids, service+"-id-"+strings.Repeat("x", i))
			}
			_, err := io.WriteString(c.Stdout, strings.Join(ids, "\n"))
			return err
		}
		if strings.Contains(c.Script, "docker inspect") {
			service := "db"
			if strings.Contains(c.Script, "'api-id-") {
				service = "api"
			}
			sampled[service]++
			if strings.Contains(c.Script, ".State.Error") || strings.Contains(c.Script, ".State.Health") {
				t.Fatal("batch sampled unknown secret text")
			}
			raw, _ := json.Marshal(map[string]any{"status": "exited", "exit_code": 7, "oom_killed": false, "error": oldToken})
			_, err := c.Stdout.Write(raw)
			return err
		}
		if strings.Contains(c.Script, "docker logs") {
			t.Fatal("batch requested unknown-generation logs")
		}
		return nil
	}}
	r := runner{h: h, out: &out, secretVals: []string{newToken}}
	plans := []servicePlan{{Name: "api", Replicas: 1, DependsOn: []string{"db"}}, {Name: "db", Replicas: 1}, {Name: "web", Replicas: 1, DependsOn: []string{"api", "db"}, ReleaseCommand: []string{"migrate"}}}
	err := r.startDependencies(context.Background(), plans)
	if !errors.Is(err, cause) || sampled["api"] != 4 || sampled["db"] != 4 || out.Len() > 33000 {
		t.Fatalf("batch budgets/samples: %v %v %d", err, sampled, out.Len())
	}
	if !strings.Contains(out.String(), "sampling 2 of 2") || strings.Contains(out.String()+err.Error(), oldToken) {
		t.Fatal("misleading batch or leaked retained text")
	}
}

func TestReleasePrerequisiteDiagnostics(t *testing.T) {
	cause := errors.New("database unhealthy")
	var out bytes.Buffer
	h := &diagnosticHost{Local: &remote.Local{}, run: func(_ context.Context, c remote.Cmd) error {
		if strings.Contains(c.Script, "'ps' '--all' '-q' 'db'") {
			return cause
		}
		if strings.Contains(c.Script, "docker ps -aq") {
			_, _ = io.WriteString(c.Stdout, "db-id\n")
		}
		if strings.Contains(c.Script, "docker logs") {
			_, _ = io.WriteString(c.Stdout, "database failed\n")
		}
		return nil
	}}
	r := runner{h: h, out: &out}
	err := r.startDependencies(context.Background(), []servicePlan{{Name: "db", Replicas: 1}, {Name: "web", Replicas: 1, DependsOn: []string{"db"}, ReleaseCommand: []string{"migrate"}}})
	var failure *Failure
	if !errors.Is(err, cause) || !errors.As(err, &failure) || failure.Phase != "release-prerequisites-readiness" || !strings.Contains(out.String(), "error/logs/health output omitted") {
		t.Fatalf("missing prerequisite diagnostics: %v %s", err, out.String())
	}
}
