package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/servicecheck"
)

// checkHost answers the readiness and check commands of one Service with
// one container at 172.18.0.5 and dials through dial.
type checkHost struct {
	current string // readlink of current, "" when none
	dial    func(ctx context.Context, network, addr string) (net.Conn, error)
	dials   atomic.Int32
	scripts []string
}

func (h *checkHost) Name() string { return "s1" }
func (h *checkHost) Run(ctx context.Context, c remote.Cmd) error {
	_, err := h.Output(ctx, c)
	return err
}
func (h *checkHost) Output(_ context.Context, c remote.Cmd) (string, error) {
	h.scripts = append(h.scripts, c.Script)
	switch {
	case strings.HasPrefix(c.Script, "readlink "):
		return h.current, nil
	case strings.Contains(c.Script, "docker inspect --format"):
		return "0123456789abcdef|/test-ws-1|test|test_default|test_default=172.18.0.5|yoho=172.19.0.9", nil
	case strings.Contains(c.Script, "'ps' '-q' 'ws'"):
		return "0123456789abcdef\n", nil
	}
	return "", nil // the readiness wait script: ready
}
func (h *checkHost) WriteFile(context.Context, string, []byte, os.FileMode, bool) error {
	return nil
}
func (h *checkHost) ReadFile(context.Context, string, bool) ([]byte, error) {
	return nil, os.ErrNotExist
}
func (h *checkHost) Close() error { return nil }
func (h *checkHost) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	h.dials.Add(1)
	return h.dial(ctx, network, addr)
}

func fastChecks(t *testing.T) {
	t.Helper()
	old := servicecheck.PollInterval
	servicecheck.PollInterval = 10 * time.Millisecond
	t.Cleanup(func() { servicecheck.PollInterval = old })
}

// to dials the test server whatever address the check asks for, after
// checking it is the container's address on the project network.
func to(t *testing.T, srv string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != "172.18.0.5:8080" {
			t.Errorf("dialed %s, want the container's project network address 172.18.0.5:8080", addr)
		}
		var d net.Dialer
		return d.DialContext(ctx, network, srv)
	}
}

func checkRunner(h remote.Host, out io.Writer) *runner {
	return &runner{h: h, out: out, server: "s1", cc: composeCLI{project: "test", dir: "/srv/apps/a/d", file: "/srv/apps/a/d/releases/v2/compose.yaml"}}
}

func TestReadinessRunsServiceCheck(t *testing.T) {
	fastChecks(t)
	ws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" || r.Host != "localhost" {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(204)
	}))
	defer ws.Close()
	h := &checkHost{dial: to(t, ws.Listener.Addr().String())}
	var out bytes.Buffer
	sp := servicePlan{Name: "ws", Replicas: 1, Check: checkWithDefaults(&config.ServiceCheck{Type: "http", Port: 8080, Path: "/health"})}
	if err := checkRunner(h, &out).waitService(context.Background(), sp); err != nil {
		t.Fatalf("check: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "checking Service ws: http /health on port 8080") || !strings.Contains(out.String(), "Service ws check passed") {
		t.Fatalf("output:\n%s", out.String())
	}
	// A Service with a check gets no "no Docker healthcheck" notice.
	if strings.Contains(strings.Join(h.scripts, "\n"), "only guarantees the process is running") {
		t.Fatal("weak readiness notice printed for a Service with a check")
	}
}

func TestReadinessCheckSlowThenOK(t *testing.T) {
	fastChecks(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	h := &checkHost{}
	ok := to(t, ln.Addr().String())
	h.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if h.dials.Load() < 4 {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
		}
		return ok(ctx, network, addr)
	}
	sp := servicePlan{Name: "ws", Replicas: 1, Check: checkWithDefaults(&config.ServiceCheck{Type: "tcp", Port: 8080, Timeout: "5s"})}
	var out bytes.Buffer
	if err := checkRunner(h, &out).waitService(context.Background(), sp); err != nil {
		t.Fatalf("slow start: %v\n%s", err, out.String())
	}
	if n := h.dials.Load(); n != 4 {
		t.Fatalf("dials = %d, want 4", n)
	}
}

func TestReadinessCheckFailureNamesRollback(t *testing.T) {
	fastChecks(t)
	refused := func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	}
	sp := servicePlan{Name: "ws", Replicas: 1, Check: checkWithDefaults(&config.ServiceCheck{Type: "websocket", Port: 8080, Path: "/sync", Timeout: "200ms"})}
	for _, tc := range []struct{ current, rollback string }{
		{"releases/v1", "`yoho rollback v1`"},
		{"", ""},            // first deploy: nothing to roll back to
		{"releases/v2", ""}, // redeploy of the same Version
	} {
		h := &checkHost{current: tc.current, dial: refused}
		var out bytes.Buffer
		start := time.Now()
		err := checkRunner(h, &out).waitService(context.Background(), sp)
		var ce *CheckError
		if !errors.As(err, &ce) {
			t.Fatalf("current %q: err = %v, want a CheckError", tc.current, err)
		}
		if el := time.Since(start); el > 3*time.Second {
			t.Fatalf("check ran %s past its 200ms timeout", el)
		}
		if !strings.Contains(err.Error(), "Service ws check failed: container test-ws-1: websocket /sync on 172.18.0.5:8080") {
			t.Fatalf("error: %v", err)
		}
		var f *Failure
		if !errors.As(err, &f) {
			t.Fatalf("not a diagnosed Failure: %v", err)
		}
		hint := ce.Hint()
		if tc.rollback == "" && strings.Contains(hint, "yoho rollback") || tc.rollback != "" && !strings.Contains(hint, tc.rollback+" (volumes and migrations are not reverted)") {
			t.Fatalf("current %q: hint %q", tc.current, hint)
		}
		if h.dials.Load() < 2 {
			t.Fatalf("check was not retried: %d dials", h.dials.Load())
		}
	}
}

func TestReadinessCheckRefusesForeignContainer(t *testing.T) {
	fastChecks(t)
	h := &foreignHost{checkHost{dial: func(context.Context, string, string) (net.Conn, error) {
		t.Error("dialed a container of another project")
		return nil, errors.New("no")
	}}}
	sp := servicePlan{Name: "ws", Replicas: 1, Check: checkWithDefaults(&config.ServiceCheck{Type: "tcp", Port: 8080, Timeout: "1s"})}
	err := checkRunner(h, io.Discard).waitService(context.Background(), sp)
	if err == nil || !strings.Contains(err.Error(), "does not belong to compose project test") {
		t.Fatalf("err = %v", err)
	}
}

type foreignHost struct{ checkHost }

func (h *foreignHost) Output(ctx context.Context, c remote.Cmd) (string, error) {
	if strings.Contains(c.Script, "docker inspect --format") {
		return "0123456789abcdef|/other-ws-1|other|other_default|other_default=172.20.0.5", nil
	}
	return h.checkHost.Output(ctx, c)
}
func (h *foreignHost) Run(ctx context.Context, c remote.Cmd) error {
	_, err := h.Output(ctx, c)
	return err
}

// plan.json records the check with defaults, and Rollback/Diff read it back.
func TestPlanJSONRecordsCheck(t *testing.T) {
	root := withRoot(t)
	h := &dockerHost{local: &remote.Local{}, respond: respond(nil)}
	d := testDeploy(t, h, io.Discard)
	cf := d.Ext["cloudflared"]
	cf.Check = &config.ServiceCheck{Type: "websocket", Port: 8080, Path: "/sync"}
	d.Ext["cloudflared"] = cf
	_, plans, err := compile(d, "primary", planGenDir, nil, planKey, "", DaemonLogging{Driver: "json-file"})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeReleaseFiles(context.Background(), h, filepath.Join(root, "rel"), []byte("services: {}\n"), plans); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, "rel", "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	var back []servicePlan
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	want := config.ServiceCheck{Type: "websocket", Port: 8080, Path: "/sync", Host: "localhost", Timeout: "30s"}
	for _, sp := range back {
		switch {
		case sp.Name == "cloudflared" && (sp.Check == nil || *sp.Check != want):
			t.Fatalf("recorded check %+v, want %+v", sp.Check, want)
		case sp.Name != "cloudflared" && sp.Check != nil:
			t.Fatalf("%s has a check", sp.Name)
		}
	}
	if !bytes.Contains(b, []byte(`"check": {`)) {
		t.Fatalf("plan.json:\n%s", b)
	}
}

func TestCompileRejectsInvalidCheck(t *testing.T) {
	withRoot(t)
	for name, ext := range map[string]config.ServiceExt{
		"proxy":   {Proxy: &config.ServiceProxy{Port: 3000}, Check: &config.ServiceCheck{Type: "tcp", Port: 3000}},
		"no port": {Check: &config.ServiceCheck{Type: "tcp"}},
		"timeout": {Check: &config.ServiceCheck{Type: "tcp", Port: 1, Timeout: "11m"}},
	} {
		h := &dockerHost{local: &remote.Local{}, respond: respond(nil)}
		d := testDeploy(t, h, io.Discard)
		svc := "cloudflared"
		if ext.Proxy != nil {
			svc = "web"
		}
		d.Ext[svc] = ext
		if err := validate(d); err == nil || !strings.Contains(err.Error(), "x-yoho.check") {
			t.Errorf("%s: validate = %v", name, err)
		}
	}
	// A plan.json check on a one-shot job is refused too (rollback).
	done := true
	if err := validateReadinessPlans([]servicePlan{{Name: "job", Replicas: 1, CompletedSuccessfully: &done, Check: &config.ServiceCheck{Type: "tcp", Port: 1}}}); err == nil {
		t.Error("check on a one-shot job accepted")
	}
}

// A check-only change is an update without downtime, never a replace.
func TestDiffCheckOnlyChange(t *testing.T) {
	h, _, root := deployed(t)
	d := testDeploy(t, h, io.Discard)
	cf := d.Ext["cloudflared"]
	cf.Check = &config.ServiceCheck{Type: "tcp", Port: 8080}
	d.Ext["cloudflared"] = cf
	cs, err := Compose{}.Diff(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	ch := byKey(cs)["service cloudflared"]
	if ch.Action != plan.ActionUpdate || ch.Downtime || !slices.Equal(ch.Reasons, []string{"check changed"}) {
		t.Fatalf("added check: %+v", ch)
	}

	// Record the check in the current Release: the same compose is a noop,
	// and a changed check is an update again.
	pj := filepath.Join(root, "apps", "shop", "production", "current", "plan.json")
	b, err := os.ReadFile(pj)
	if err != nil {
		t.Fatal(err)
	}
	var plans []servicePlan
	if err := json.Unmarshal(b, &plans); err != nil {
		t.Fatal(err)
	}
	for i := range plans {
		if plans[i].Name == "cloudflared" {
			plans[i].Check = checkWithDefaults(cf.Check)
		}
	}
	b, _ = json.Marshal(plans)
	if err := os.WriteFile(pj, b, 0o600); err != nil {
		t.Fatal(err)
	}
	cs, err = Compose{}.Diff(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if ch := byKey(cs)["service cloudflared"]; ch.Action != plan.ActionNoop {
		t.Fatalf("recorded check: %+v", ch)
	}
	cf.Check = &config.ServiceCheck{Type: "tcp", Port: 8080, Timeout: "1m"}
	d.Ext["cloudflared"] = cf
	cs, err = Compose{}.Diff(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if ch := byKey(cs)["service cloudflared"]; ch.Action != plan.ActionUpdate || ch.Downtime || !slices.Equal(ch.Reasons, []string{"check changed"}) {
		t.Fatalf("changed timeout: %+v", ch)
	}
	if !samePlanProxy(servicePlan{Name: "x", Check: &config.ServiceCheck{Type: "tcp", Port: 1}}, servicePlan{Name: "x"}) {
		t.Fatal("samePlanProxy compares checks")
	}
}

// The compose loader keeps x-yoho in the project, so it reaches the
// compiled file. Docker leaves extensions out of its config hash, and the
// plan must too: a check added there is still only "check changed".
func TestDiffIgnoresXYohoInCompiledCompose(t *testing.T) {
	h, _, _ := deployed(t)
	d := testDeploy(t, h, io.Discard)
	cf := d.Ext["cloudflared"]
	cf.Check = &config.ServiceCheck{Type: "tcp", Port: 8080}
	d.Ext["cloudflared"] = cf
	s := d.Project.Services["cloudflared"]
	s.Extensions = types.Extensions{"x-yoho": map[string]any{"check": map[string]any{"type": "tcp", "port": 8080}}}
	d.Project.Services["cloudflared"] = s
	cs, err := Compose{}.Diff(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if ch := byKey(cs)["service cloudflared"]; ch.Action != plan.ActionUpdate || ch.Downtime || !slices.Equal(ch.Reasons, []string{"check changed"}) {
		t.Fatalf("check in x-yoho: %+v", ch)
	}
}

// dialHost is a dockerHost that can dial (as *remote.SSH and the Gate's
// remote.Local can).
type dialHost struct {
	*dockerHost
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

func (h *dialHost) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return h.dial(ctx, network, addr)
}

// A deploy runs the check; a rollback runs the check its target Release
// recorded, not today's compose.
func TestDeployAndRollbackRunRecordedCheck(t *testing.T) {
	withRoot(t)
	fastChecks(t)
	ctx := context.Background()
	var dialed []string
	up := true
	h := &dialHost{dockerHost: &dockerHost{local: &remote.Local{}}}
	h.dial = func(_ context.Context, _, addr string) (net.Conn, error) {
		dialed = append(dialed, addr)
		if !up {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
		}
		a, b := net.Pipe()
		go b.Close()
		return a, nil
	}
	base := respond(nil)
	h.respond = func(s string) (string, error) {
		switch {
		case strings.Contains(s, "'ps' '-q' 'cloudflared'"):
			return "cf1", nil
		case strings.Contains(s, "docker inspect --format '{{.Id}}"):
			return "cf1full|/yoho-shop-production-cloudflared-1|yoho-shop-production|yoho-shop-production_default|yoho-shop-production_default=172.18.0.7", nil
		}
		return base(s)
	}
	var out bytes.Buffer
	d := testDeploy(t, h, &out)
	cf := d.Ext["cloudflared"]
	cf.Check = &config.ServiceCheck{Type: "tcp", Port: 2000, Timeout: "100ms"}
	d.Ext["cloudflared"] = cf
	if _, err := (Compose{}).Deploy(ctx, d); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !slices.Equal(dialed, []string{"172.18.0.7:2000"}) {
		t.Fatalf("deploy dialed %v", dialed)
	}

	// v2 has no check; rolling back to v1 runs v1's check.
	d.Version = "v2"
	cf.Check = nil
	d.Ext["cloudflared"] = cf
	if _, err := (Compose{}).Deploy(ctx, d); err != nil {
		t.Fatal(err)
	}
	dialed, up = nil, false
	_, err := (Compose{}).Rollback(ctx, d, "v1")
	var ce *CheckError
	if !errors.As(err, &ce) || ce.Service != "cloudflared" || ce.Previous != "v2" {
		t.Fatalf("rollback: %v (%+v)", err, ce)
	}
	if len(dialed) == 0 || dialed[0] != "172.18.0.7:2000" {
		t.Fatalf("rollback dialed %v", dialed)
	}
}

// Release prerequisites start with --no-recreate, so their containers may
// still be the previous Release's: their check waits for the cutover.
func TestPrerequisiteReadinessSkipsServiceCheck(t *testing.T) {
	fastChecks(t)
	h := &checkHost{dial: func(context.Context, string, string) (net.Conn, error) {
		t.Error("checked a prerequisite before cutover")
		return nil, errors.New("no")
	}}
	plans := []servicePlan{
		{Name: "ws", Replicas: 1, Stateful: true, Check: checkWithDefaults(&config.ServiceCheck{Type: "tcp", Port: 8080, Timeout: "200ms"})},
		{Name: "web", Replicas: 1, ReleaseCommand: []string{"migrate"}},
	}
	var out bytes.Buffer
	if err := checkRunner(h, &out).startDependencies(context.Background(), plans); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "checking Service ws") {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestServiceCheckNeedsEveryReplica(t *testing.T) {
	fastChecks(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	h := &checkHost{dial: to(t, ln.Addr().String())}
	sp := servicePlan{Name: "ws", Replicas: 2, Check: checkWithDefaults(&config.ServiceCheck{Type: "tcp", Port: 8080, Timeout: "200ms"})}
	err = checkRunner(h, io.Discard).serviceCheck(context.Background(), sp, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "1 of 2 container(s) running") {
		t.Fatalf("err = %v", err)
	}
}
