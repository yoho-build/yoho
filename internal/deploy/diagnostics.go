package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/secrets"
)

const diagnosticContainers = 8

// DiagnosticText captures bounded redacted text for runtime diagnostics. It
// never flushes an unresolved secret prefix after interruption or input cutoff.
// The caller supplies the overall deadline. It must not feed pre-truncated text.
func DiagnosticText(ctx context.Context, h remote.Host, script string, values []string) (string, error) {
	sink := &cappedBuffer{limit: 4096}
	capture := &logCapture{sink: sink, remaining: 65536, drain: true}
	capture.redact = secrets.NewDiagnosticRedactor(discardOverflow{sink}, values...)
	err := h.Run(ctx, remote.Cmd{Script: script, Stdout: capture, Stderr: io.Discard})
	if err == nil && !capture.cut {
		_ = capture.redact.Flush()
	}
	text := sink.String()
	if err != nil || capture.cut || sink.cut {
		text += "\n[diagnostics incomplete/truncated]"
	}
	return text, err
}

// cappedBuffer never retains more than limit bytes. A cutoff is not EOF: callers
// must not decode incomplete JSON or flush a redactor's unresolved secret tail.
type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
	cut   bool
}

func (b *cappedBuffer) Len() int       { return b.buf.Len() }
func (b *cappedBuffer) String() string { return b.buf.String() }
func (b *cappedBuffer) Bytes() []byte  { return b.buf.Bytes() }

var errDiagnosticLimit = errors.New("diagnostic byte limit")

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := min(len(p), max(0, b.limit-b.Len()))
	_, _ = b.buf.Write(p[:n])
	if n < len(p) {
		b.cut = true
		return n, errDiagnosticLimit
	}
	return len(p), nil
}

// logCapture caps raw input work independently of the redacted output cap.
// Chunks keep Redactor memory bounded even if a Host passes a giant Write.
type logCapture struct {
	redact    *secrets.Redactor
	sink      *cappedBuffer
	remaining int
	cut       bool
	drain     bool
}

func (w *logCapture) Write(p []byte) (int, error) {
	original := len(p)
	for len(p) > 0 {
		n := min(len(p), 4096, w.remaining)
		if n == 0 {
			w.cut = true
			if w.drain {
				return original, nil
			}
			return original - len(p), errDiagnosticLimit
		}
		// The sink can stop retaining output without stopping redaction mid-secret.
		_, err := w.redact.Write(p[:n])
		if err != nil {
			return original - len(p), err
		}
		w.remaining -= n
		p = p[n:]
	}
	if w.remaining == 0 {
		w.cut = true
	}
	return original, nil
}

type discardOverflow struct{ b *cappedBuffer }

func (w discardOverflow) Write(p []byte) (int, error) { _, _ = w.b.Write(p); return len(p), nil }

// diagnosticRun deliberately discards stderr and never includes a Host error's
// text: Docker/SSH errors can themselves contain secrets or truncated payloads.
func (r *runner) diagnosticRun(ctx context.Context, script string, limit int) (*cappedBuffer, error) {
	b := &cappedBuffer{limit: limit}
	err := r.h.Run(ctx, remote.Cmd{Script: script, Stdout: b, Stderr: io.Discard})
	return b, err
}

func (r *runner) diagnosticRedact(text string) string {
	var b bytes.Buffer
	w := secrets.NewDiagnosticRedactor(&b, r.secretVals...)
	_, _ = w.Write([]byte(text))
	// State strings may themselves have been cut off by Docker's health-log
	// limit. Only publish bytes proven not to begin a known secret.
	return b.String()
}

// Explicit IDs must be verified fresh candidates from prepareProxied. A Service
// listing is not proof of secret generation: retained replicas may use rotated
// values absent from this deploy's inventory. Those receive metadata only.
type diagnosticSample struct {
	service string
	fresh   []string
}

func (r *runner) diagnose(ctx context.Context, service string, ids []string) {
	r.diagnoseSamples(ctx, []diagnosticSample{{service: service, fresh: ids}})
}

// diagnoseBatch samples the Services of a batch `compose up` that failed
// without naming the Service at fault.
func (r *runner) diagnoseBatch(ctx context.Context, services []string) {
	samples := make([]diagnosticSample, 0, min(len(services), diagnosticContainers))
	for _, service := range services[:min(len(services), diagnosticContainers)] {
		samples = append(samples, diagnosticSample{service: service})
	}
	r.logf("diagnostics: batch startup failed; sampling %d of %d Services, not identifying a single failing Service", len(samples), len(services))
	r.diagnoseSamples(ctx, samples)
}

// Every sample shares one deadline, container allowance and output sink.
func (r *runner) diagnoseSamples(ctx context.Context, samples []diagnosticSample) {
	originalOut := r.out
	output := &cappedBuffer{limit: 32768}
	r.out = secrets.NewDiagnosticRedactor(discardOverflow{output}, r.secretVals...)
	defer func() {
		_ = r.out.(*secrets.Redactor).Flush()
		r.out = originalOut
		if output.Len() > 0 {
			fmt.Fprintln(originalOut, output.String())
		}
		if output.cut {
			r.logf("diagnostics output truncated at 32 KiB")
		}
	}()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	remaining := diagnosticContainers
	perService := diagnosticContainers / max(1, len(samples))
	for _, sample := range samples {
		if ctx.Err() != nil || remaining == 0 {
			r.logf("diagnostics: shared time/container budget exhausted")
			break
		}
		remaining -= r.collectDiagnosticSample(ctx, sample.service, sample.fresh, min(perService, remaining))
	}
}

func (r *runner) collectDiagnosticSample(ctx context.Context, service string, ids []string, limit int) (used int) {
	omitText := r.omitDiagnosticText || ids == nil
	if ids == nil {
		// Docker filters avoid Compose collecting an unbounded ps listing. One-offs
		// are excluded before diagnostics, just as they are from readiness gates.
		script := "set -eu\ndocker ps -aq --filter " + remote.Quote("label=com.docker.compose.project="+r.cc.project) + " --filter " + remote.Quote("label=com.docker.compose.service="+service) + " --filter " + remote.Quote("label=com.docker.compose.oneoff=False")
		b, err := r.diagnosticRun(ctx, script, 4096)
		if err != nil || b.cut {
			r.logf("diagnostics Service %s: container listing unavailable or exceeded limit", service)
			return
		}
		ids = lines(b.String())
	}
	if len(ids) == 0 {
		r.logf("diagnostics Service %s: no containers found", service)
		return
	}
	if len(ids) > limit {
		r.logf("diagnostics Service %s: limited to %d containers", service, limit)
		ids = ids[:limit]
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			r.logf("diagnostics Service %s: time budget exhausted", service)
			break
		}
		used++
		// Only State fields: never full inspect, Config, environment or healthcheck
		// commands. Decode BEFORE redaction (JSON escapes may encode secret bytes).
		format := `{"status":{{json .State.Status}},"exit_code":{{json .State.ExitCode}},"oom_killed":{{json .State.OOMKilled}},"error":{{json .State.Error}},"health":{{if .State.Health}}{{json .State.Health}}{{else}}null{{end}}}`
		if omitText {
			format = `{"status":{{json .State.Status}},"exit_code":{{json .State.ExitCode}},"oom_killed":{{json .State.OOMKilled}}}`
		}
		b, err := r.diagnosticRun(ctx, "set -eu\ndocker inspect -f "+remote.Quote(format)+" "+remote.Quote(id), 16384)
		var state struct {
			Status string `json:"status"`
			Exit   int    `json:"exit_code"`
			OOM    bool   `json:"oom_killed"`
			Error  string `json:"error"`
			Health *struct {
				Status string
				Log    []struct {
					ExitCode int
					Output   string
				}
			} `json:"health"`
		}
		if err != nil || b.cut || json.Unmarshal(b.Bytes(), &state) != nil {
			r.logf("diagnostics Service %s container %s: State unavailable or invalid (payload omitted)", service, id)
		} else {
			if omitText {
				r.logf("diagnostics Service %s container %s: state=%s exit=%d oom=%t", service, id, state.Status, state.Exit, state.OOM)
			} else {
				r.logf("diagnostics Service %s container %s: state=%s exit=%d oom=%t error=%s", service, id, state.Status, state.Exit, state.OOM, r.diagnosticRedact(state.Error))
			}
			if !omitText && state.Health != nil {
				r.logf("diagnostics health=%s", state.Health.Status)
				for _, entry := range state.Health.Log[max(0, len(state.Health.Log)-2):] {
					text := r.diagnosticRedact(entry.Output)
					if len(text) > 2048 {
						text = text[:2048] + " [truncated]"
					}
					r.logf("diagnostics health exit=%d output=%s", entry.ExitCode, text)
				}
			}
		}
		if omitText {
			r.logf("diagnostics error/logs/health output omitted: retained secret values or unverified container generations cannot be safely redacted")
			continue
		}
		sink := &cappedBuffer{limit: 2048}
		w := &logCapture{sink: sink, remaining: 65536}
		w.redact = secrets.NewDiagnosticRedactor(discardOverflow{sink}, r.secretVals...)
		err = r.h.Run(ctx, remote.Cmd{Script: "set -eu\ndocker logs --tail 20 " + remote.Quote(id) + " 2>&1", Stdout: w, Stderr: io.Discard})
		// Never flush unresolved prefixes, even at EOF: Docker may already have
		// truncated a log record. Output already proved safe remains available.
		text := strings.TrimSpace(sink.String())
		if text != "" {
			r.logf("diagnostics recent logs Service %s container %s:\n%s", service, id, text)
		}
		if err != nil || w.cut || sink.cut {
			r.logf("diagnostics logs unavailable/incomplete or truncated")
		}
	}
	return used
}

// Failure carries observed facts, not an App-wide rollback claim. Unwrap keeps
// the original cause available to errors.Is/errors.As.
type Failure struct {
	Cause    error
	Message  string
	Phase    string
	Services []ServiceFailure
	// Network is set when Docker ran out of address pools.
	Network *NetworkHint
}
type ServiceFailure struct {
	Service     string `json:"service"`
	Convergence string `json:"convergence"`
	ProxyRoute  string `json:"proxy_route"`
}

func (e *Failure) Error() string {
	message := e.Message
	if message == "" {
		message = "details omitted"
	}
	return fmt.Sprintf("deploy phase %s: %s", e.Phase, message)
}
func (e *Failure) Unwrap() error { return e.Cause }
func (e *Failure) FailureFacts() any {
	services := e.Services
	if services == nil {
		services = []ServiceFailure{}
	}
	facts := map[string]any{"phase": e.Phase, "services": services}
	if e.Network != nil {
		facts["network"] = map[string]any{"name": e.Network.Network, "default": e.Network.Default, "suggested_subnet": e.Network.SuggestedSubnet}
	}
	return facts
}
func (r *runner) failure(ctx context.Context, err error, service string, ids []string) error {
	if !r.diagnosed {
		r.logf("deploy failed in phase %s", r.phase)
		if service != "" {
			r.diagnose(ctx, service, ids)
		}
		r.diagnosed = true
	}
	facts := append([]ServiceFailure(nil), r.facts...)
	var live []string
	for _, f := range facts {
		r.logf("Service %s: convergence=%s Proxy route=%s", f.Service, f.Convergence, f.ProxyRoute)
		if f.ProxyRoute == "switched" {
			live = append(live, RouteName(r.app, r.dest, f.Service))
		}
	}
	message := r.diagnosticRedact(err.Error())
	if len(live) > 0 {
		// Switched routes target ready new containers and stay; the next
		// deploy updates them in the normal order.
		r.logf("Proxy routes already live on the new containers: %s", strings.Join(live, ", "))
		message += " (Proxy routes already live: " + strings.Join(live, ", ") + ")"
	}
	return &Failure{Cause: err, Message: message, Phase: r.phase, Services: facts}
}
func (r *runner) fact(service string) *ServiceFailure {
	for i := range r.facts {
		if r.facts[i].Service == service {
			return &r.facts[i]
		}
	}
	r.facts = append(r.facts, ServiceFailure{Service: service, Convergence: "not-attempted", ProxyRoute: "not-applicable"})
	return &r.facts[len(r.facts)-1]
}
