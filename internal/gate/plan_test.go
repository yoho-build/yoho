package gate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// dockerHost runs file and shell work on the local filesystem (inside
// release.Root = t.TempDir()) and answers every docker script from
// respond, recording it.
type dockerHost struct {
	local   remote.Local
	mu      sync.Mutex
	scripts []string
	respond func(script string) string
}

func (h *dockerHost) Name() string { return "srv" }
func (h *dockerHost) Run(ctx context.Context, c remote.Cmd) error {
	out, err := h.Output(ctx, c)
	if err == nil && c.Stdout != nil && out != "" && strings.Contains(c.Script, "docker") {
		io.WriteString(c.Stdout, out)
	}
	return err
}
func (h *dockerHost) Output(ctx context.Context, c remote.Cmd) (string, error) {
	if !strings.Contains(c.Script, "docker") {
		if c.Stdout != nil {
			var b bytes.Buffer
			c2 := c
			c2.Stdout = io.MultiWriter(&b, c.Stdout)
			err := h.local.Run(ctx, c2)
			return strings.TrimSpace(b.String()), err
		}
		return h.local.Output(ctx, c)
	}
	if c.Stdin != nil {
		io.Copy(io.Discard, c.Stdin)
	}
	h.mu.Lock()
	h.scripts = append(h.scripts, c.Script)
	h.mu.Unlock()
	if h.respond != nil {
		return h.respond(c.Script), nil
	}
	return "", nil
}
func (h *dockerHost) WriteFile(ctx context.Context, p string, b []byte, m os.FileMode, sudo bool) error {
	h.mu.Lock()
	h.scripts = append(h.scripts, "write "+p)
	h.mu.Unlock()
	return h.local.WriteFile(ctx, p, b, m, sudo)
}
func (h *dockerHost) ReadFile(ctx context.Context, p string, sudo bool) ([]byte, error) {
	return h.local.ReadFile(ctx, p, sudo)
}
func (h *dockerHost) Close() error { return nil }
func (h *dockerHost) all() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.scripts, "\n---\n")
}
func (h *dockerHost) reset() {
	h.mu.Lock()
	h.scripts = nil
	h.mu.Unlock()
}

// planCompose: a Stateful Service and a worker, neither proxied, without
// any of the limits the Gate adds.
const planCompose = `services:
  db:
    image: postgres:17
    volumes: [pgdata:/var/lib/postgresql/data]
  worker:
    image: busybox:latest
    command: [sleep, infinity]
volumes:
  pgdata: {}
`

// runningState is `docker inspect` output for one running container per
// Service of the current Release's compose file.
func runningState(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(release.AppDir("shop", "production"), "current", "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Services map[string]struct {
			Image  string            `yaml:"image"`
			Labels map[string]string `yaml:"labels"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var all []map[string]any
	for name, s := range doc.Services {
		labels := map[string]any{"com.docker.compose.service": name}
		for k, v := range s.Labels {
			labels[k] = v
		}
		all = append(all, map[string]any{
			"Name": "/yoho-shop-production-" + name + "-1", "State": map[string]any{"Running": true},
			"Config": map[string]any{"Image": s.Image, "Labels": labels},
		})
	}
	b, _ := json.Marshal(all)
	return string(b)
}

func deployAnswers(s string) string {
	switch {
	case strings.Contains(s, "{{.LoggingDriver}}"):
		return "json-file"
	case strings.Contains(s, "kamal-proxy list --json"):
		return "null"
	}
	return ""
}

// gateDeployed deploys planCompose as v1 through the Gate (the real
// runtime on a fake docker) and returns the host, answering the plan's
// docker queries with the deployed containers.
func gateDeployed(t *testing.T) *dockerHost {
	t.Helper()
	setup(t, "")
	writeSettings(t, Settings{Server: "srv"})
	h := &dockerHost{respond: deployAnswers}
	var rel release.Release
	if err := client(t, "alice", h).Call(context.Background(), VerbDeploy, DeployArgs{Version: "v1", Compose: planCompose, Rollout: "r1"}, nil, io.Discard, &rel); err != nil {
		t.Fatalf("deploy: %v\n%s", err, h.all())
	}
	if rel.Status != "deployed" || !strings.Contains(rel.Performer, " (gate)") {
		t.Fatalf("release %+v", rel)
	}
	// The in-process rollout worker appends its audit line after the
	// client has the result; wait for it (both Gate runs log the deploy).
	for deadline := time.Now().Add(5 * time.Second); ; {
		b, _ := os.ReadFile(AuditPath("shop", "production"))
		if strings.Count(string(b), `"verb":"deploy"`) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("rollout worker did not finish:\n%s", b)
		}
		time.Sleep(10 * time.Millisecond)
	}
	state := runningState(t)
	h.respond = func(s string) string {
		if strings.Contains(s, "docker inspect $ids") {
			return state
		}
		return deployAnswers(s)
	}
	h.reset()
	return h
}

func gatePlan(t *testing.T, h remote.Host, compose string) []plan.Change {
	t.Helper()
	var cs []plan.Change
	if err := client(t, "alice", h).Call(context.Background(), VerbPlan, PlanArgs{Version: "v1", Compose: compose}, nil, io.Discard, &cs); err != nil {
		t.Fatalf("plan: %v", err)
	}
	return cs
}

// Issue #65: after a Gate deploy, a scoped plan of the same compose shows
// nothing to do (the Gate hardens what it compares, as deploy does), and
// it never pulls, loads, locks or writes.
func TestPlanVerbConvergesAfterGateDeploy(t *testing.T) {
	h := gateDeployed(t)
	appDir := release.AppDir("shop", "production")
	before := snapshot(t, appDir)
	cs := gatePlan(t, h, planCompose)
	for _, c := range cs {
		if c.Action != plan.ActionNoop {
			t.Errorf("unexpected change %+v", c)
		}
	}
	if len(cs) != 2 {
		t.Errorf("changes %+v", cs)
	}
	for _, bad := range []string{"docker pull", "docker load", "docker compose", "docker run", "docker tag", "docker volume create", "docker rm", "docker network create", "write "} {
		if strings.Contains(h.all(), bad) {
			t.Errorf("plan ran %q:\n%s", bad, h.all())
		}
	}
	if after := snapshot(t, appDir); after != before {
		t.Errorf("plan changed the App directory:\nbefore %s\nafter  %s", before, after)
	}
	if _, err := os.Stat(rolloutStatePath("shop", "production")); err != nil {
		t.Fatal(err) // the deploy's rollout state is still the deploy's
	}
	if st, _ := readRolloutState("shop", "production"); st == nil || st.Verb != VerbDeploy {
		t.Errorf("plan touched the rollout state: %+v", st)
	}
	if e := lastAudit(t); e.Verb != VerbPlan || e.Exit != ExitOK {
		t.Errorf("audit %+v", e)
	}
}

// An explicit limit the Member sets still shows as a change.
func TestPlanVerbShowsExplicitLimitChange(t *testing.T) {
	h := gateDeployed(t)
	changed := strings.Replace(planCompose, "    image: postgres:17\n", "    image: postgres:17\n    mem_limit: 256m\n", 1)
	cs := gatePlan(t, h, changed)
	m := map[string]plan.Change{}
	for _, c := range cs {
		m[c.Name] = c
	}
	if c := m["db"]; c.Action != plan.ActionReplace || strings.Join(c.Reasons, "|") != "config changed: mem_limit" {
		t.Errorf("db %+v", c)
	}
	if m["worker"].Action != plan.ActionNoop {
		t.Errorf("worker %+v", m["worker"])
	}
}

// plan needs the deploy grant, not a grant of its own.
func TestPlanVerbNeedsDeployGrant(t *testing.T) {
	setup(t, "members:\n  bob: {verbs: [logs]}\n  carol: {verbs: [deploy]}\n")
	writeSettings(t, Settings{Server: "srv"})
	err := client(t, "bob", &fakeHost{}).Call(context.Background(), VerbPlan, PlanArgs{Version: "v1", Compose: planCompose}, nil, io.Discard, nil)
	var ge *remote.GateError
	if !errors.As(err, &ge) || ge.Code != "denied" || !strings.Contains(ge.Message, "may not deploy") {
		t.Fatalf("bob: %v", err)
	}
	var h HelloResult
	if err := client(t, "carol", &fakeHost{}).Call(context.Background(), VerbHello, nil, nil, nil, &h); err != nil || !h.Plan {
		t.Fatalf("hello %+v %v", h, err)
	}
}

// The plan refuses what deploy refuses, before any docker call, and its
// errors never carry secret values.
func TestPlanVerbRefusesAndRedacts(t *testing.T) {
	setup(t, "")
	writeSettings(t, Settings{Server: "srv"})
	h := &fakeHost{}
	evil := "services:\n  web:\n    image: alpine\n    privileged: true\n"
	err := client(t, "alice", h).Call(context.Background(), VerbPlan, PlanArgs{Version: "v1", Compose: evil}, nil, io.Discard, nil)
	var ge *remote.GateError
	if !errors.As(err, &ge) || ge.Code != "denied" || !strings.Contains(ge.Message, "privileged is not allowed") {
		t.Fatalf("got %v", err)
	}
	if h.all() != "" {
		t.Fatalf("refused plan ran scripts: %s", h.all())
	}

	const secret = "pl4n-s3cret-VALUE"
	gen := filepath.Join(release.SecretsDir("shop", "production", "20260101T000000Z-push"))
	if err := os.MkdirAll(filepath.Join(gen, "web"), 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(gen, "web", "DB_URL"), []byte(secret), 0o600)
	os.WriteFile(release.AccessSecretsFile("shop", "production"), []byte("20260101T000000Z-push\n"), 0o600)
	old := composeDiff
	t.Cleanup(func() { composeDiff = old })
	composeDiff = func(_ context.Context, d *plan.Deploy) ([]plan.Change, error) {
		if d.Scoped == nil || d.Scoped.ProxyNetwork != "yoho-shop-production-proxy" {
			t.Errorf("plan is not scoped: %+v", d.Scoped)
		}
		if d.ServiceSecrets["web"]["DB_URL"] != secret {
			t.Errorf("secrets %v", d.ServiceSecrets)
		}
		return nil, errors.New("compile: bad value " + secret)
	}
	compose := "services:\n  web:\n    image: alpine:3\n    x-yoho: {secrets: [DB_URL]}\n"
	err = client(t, "alice", &fakeHost{}).Call(context.Background(), VerbPlan, PlanArgs{Version: "v1", Compose: compose}, nil, io.Discard, nil)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("got %v", err)
	}
}

// snapshot lists every file under dir (the audit log aside: every Gate
// request appends to it) with its mode, size, time and contents hash.
func snapshot(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if p == AuditPath("shop", "production") {
			return nil
		}
		fmt.Fprintf(&b, "%s %v %d %s", p, fi.Mode(), fi.Size(), fi.ModTime())
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			l, err := os.Readlink(p)
			if err != nil {
				return err
			}
			b.WriteString(" -> " + l)
		case !fi.IsDir():
			c, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			fmt.Fprintf(&b, " %x", sha256.Sum256(c))
		}
		b.WriteString("\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}
