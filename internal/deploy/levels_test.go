package deploy

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/composefile"
	"github.com/yoho-build/yoho/internal/remote"
)

func levelNames(levels [][]servicePlan) string {
	var parts []string
	for _, level := range levels {
		var names []string
		for _, sp := range level {
			names = append(names, sp.Name)
		}
		parts = append(parts, strings.Join(names, ","))
	}
	return strings.Join(parts, " | ")
}

func TestDependencyLevels(t *testing.T) {
	cases := []struct {
		name  string
		plans []servicePlan
		want  string
		err   string
	}{
		{name: "independent", plans: []servicePlan{{Name: "pg1"}, {Name: "pg2"}, {Name: "redis"}}, want: "pg1,pg2,redis"},
		{name: "chain", plans: []servicePlan{{Name: "worker", DependsOn: []string{"api"}}, {Name: "api", DependsOn: []string{"db"}}, {Name: "db"}}, want: "db | api | worker"},
		{name: "diamond", plans: []servicePlan{
			{Name: "web", DependsOn: []string{"cache", "search"}},
			{Name: "cache", DependsOn: []string{"db"}},
			{Name: "search", DependsOn: []string{"db"}},
			{Name: "db"},
			{Name: "tunnel"},
		}, want: "db,tunnel | cache,search | web"},
		{name: "deepest dependency wins", plans: []servicePlan{{Name: "app", DependsOn: []string{"db", "migrate"}}, {Name: "migrate", DependsOn: []string{"db"}}, {Name: "db"}}, want: "db | migrate | app"},
		{name: "cycle", plans: []servicePlan{{Name: "a", DependsOn: []string{"b"}}, {Name: "b", DependsOn: []string{"a"}}}, err: "cycle"},
		{name: "missing", plans: []servicePlan{{Name: "a", DependsOn: []string{"gone"}}}, err: "missing from saved plan"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			levels, err := dependencyLevels(tc.plans)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err=%v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := levelNames(levels); got != tc.want {
				t.Fatalf("levels %q, want %q", got, tc.want)
			}
		})
	}
}

// An inactive profile Service named as an optional dependency is pruned at
// compile time, so its dependant starts in the first level.
func TestDependencyLevelsSkipInactiveProfiles(t *testing.T) {
	dir := t.TempDir()
	src := `services:
  worker:
    image: worker
    depends_on:
      db: {condition: service_healthy}
      debug: {condition: service_started, required: false}
  db:
    image: postgres
  debug:
    image: debug
    profiles: [debug]
  cache:
    image: redis
`
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := composefile.Load(context.Background(), dir, nil, nil, "shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	d := testDeploy(t, &remote.Local{}, &bytes.Buffer{})
	d.Project = loaded.Project
	d.Ext = loaded.Ext
	_, plans, err := compile(d, "primary", "/secrets", nil, []byte("key"), "")
	if err != nil {
		t.Fatal(err)
	}
	levels, err := dependencyLevels(plans)
	if err != nil {
		t.Fatal(err)
	}
	if got := levelNames(levels); got != "cache,db | worker" {
		t.Fatalf("levels %q", got)
	}
}

// levelHost answers readiness scripts for plain Services. Each wait blocks
// until every Service of its level has started waiting, which proves the
// waits run concurrently (a sequential runner would time out here).
type levelHost struct {
	*dockerHost
	t       *testing.T
	mu      sync.Mutex
	waiting map[string]bool
	ready   map[string]bool
	all     chan struct{}
	level   []string
	fail    string
}

func newLevelHost(t *testing.T, level []string, fail string) *levelHost {
	h := &levelHost{dockerHost: &dockerHost{local: &remote.Local{}}, t: t, waiting: map[string]bool{}, ready: map[string]bool{}, all: make(chan struct{}), level: level, fail: fail}
	h.respond = h.answer
	return h
}

func (h *levelHost) answer(s string) (string, error) {
	for _, name := range h.level {
		if !strings.Contains(s, "'ps' '--all' '-q' '"+name+"'") || !strings.Contains(s, "deadline=") {
			continue
		}
		h.mu.Lock()
		h.waiting[name] = true
		if len(h.waiting) == len(h.level) {
			close(h.all)
		}
		h.mu.Unlock()
		select {
		case <-h.all:
		case <-time.After(5 * time.Second):
			h.t.Errorf("readiness of %s waited alone: waits are sequential", name)
		}
		if name == h.fail {
			return "", errors.New(name + " unhealthy")
		}
		h.mu.Lock()
		h.ready[name] = true
		h.mu.Unlock()
		return "", nil
	}
	if strings.Contains(s, "'up' '-d' '--no-deps' 'app'") {
		h.mu.Lock()
		defer h.mu.Unlock()
		if len(h.ready) != len(h.level) {
			h.t.Errorf("next level started before its dependencies were ready: %v", h.ready)
		}
	}
	return "", nil
}

func TestCutoverStartsIndependentServicesInOneBatch(t *testing.T) {
	h := newLevelHost(t, []string{"pg1", "pg2", "redis"}, "")
	var out bytes.Buffer
	r := runner{h: h, out: &out, server: "s1", app: "shop", dest: "prod", cc: composeCLI{project: "p", dir: "/app", file: "/c.yaml"}}
	plans := []servicePlan{
		{Name: "app", Replicas: 1, DependsOn: []string{"pg1", "pg2", "redis"}},
		{Name: "pg1", Replicas: 1, Stateful: true},
		{Name: "pg2", Replicas: 1, Stateful: true},
		{Name: "redis", Replicas: 1, Stateful: true},
	}
	if err := r.cutover(context.Background(), plans); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	all := h.dockerHost.all()
	if strings.Count(all, "'up' '-d' '--no-deps' 'pg1' 'pg2' 'redis'") != 1 {
		t.Fatalf("no single batch up:\n%s", all)
	}
	for _, name := range []string{"pg1", "pg2", "redis"} {
		if strings.Contains(all, "'up' '-d' '--no-deps' '"+name+"'\n") || strings.HasSuffix(all, "'up' '-d' '--no-deps' '"+name+"'") {
			t.Fatalf("per-Service up for %s:\n%s", name, all)
		}
	}
	if !strings.Contains(all, "'up' '-d' '--no-deps' 'app'") {
		t.Fatalf("second level not started:\n%s", all)
	}
	progress := out.String()
	for _, want := range []string{
		"[s1] starting level 1/2: pg1, pg2, redis",
		"[s1] level 1/2 ready in ",
		"[s1] starting app",
		"[s1] level 2/2 ready in ",
	} {
		if !strings.Contains(progress, want) {
			t.Fatalf("missing %q in progress:\n%s", want, progress)
		}
	}
	// Per-Service lines survive for --json consumers, in Service order.
	pg1, pg2, redis := strings.Index(progress, "waiting for Service pg1 readiness"), strings.Index(progress, "waiting for Service pg2 readiness"), strings.Index(progress, "waiting for Service redis readiness")
	if pg1 < 0 || pg2 < pg1 || redis < pg2 {
		t.Fatalf("per-Service readiness lines:\n%s", progress)
	}
}

func TestCutoverBatchMemberFailureNamesService(t *testing.T) {
	h := newLevelHost(t, []string{"pg1", "pg2", "redis"}, "pg2")
	var out bytes.Buffer
	r := runner{h: h, out: &out, server: "s1", app: "shop", dest: "prod", cc: composeCLI{project: "p", dir: "/app", file: "/c.yaml"}}
	plans := []servicePlan{
		{Name: "app", Replicas: 1, DependsOn: []string{"pg1", "pg2", "redis"}},
		{Name: "pg1", Replicas: 1},
		{Name: "pg2", Replicas: 1},
		{Name: "redis", Replicas: 1},
	}
	err := r.cutover(context.Background(), plans)
	var failure *Failure
	if !errors.As(err, &failure) || !strings.Contains(err.Error(), "Service pg2 did not become ready") {
		t.Fatalf("failure does not name pg2: %v", err)
	}
	if failure.Phase != "plain-readiness" {
		t.Fatalf("phase %s", failure.Phase)
	}
	want := map[string]string{"app": "not-attempted", "pg1": "complete", "pg2": "attempted", "redis": "complete"}
	for _, f := range failure.Services {
		if want[f.Service] != f.Convergence {
			t.Fatalf("facts %+v", failure.Services)
		}
	}
	if strings.Contains(h.dockerHost.all(), "'up' '-d' '--no-deps' 'app'") {
		t.Fatal("next level started after a failed level")
	}
	if !strings.Contains(out.String(), "diagnostics Service pg2") || strings.Contains(out.String(), "diagnostics Service pg1") {
		t.Fatalf("diagnostics not scoped to the failing Service:\n%s", out.String())
	}
}

func TestCutoverBatchUpFailureSamplesLevel(t *testing.T) {
	h := &dockerHost{local: &remote.Local{}, respond: func(s string) (string, error) {
		if strings.Contains(s, "'up' '-d' '--no-deps' 'a' 'b'") {
			return "", errors.New("pull failed")
		}
		return "", nil
	}}
	var out bytes.Buffer
	r := runner{h: h, out: &out, server: "s1", app: "shop", dest: "prod", cc: composeCLI{project: "p", dir: "/app", file: "/c.yaml"}}
	err := r.cutover(context.Background(), []servicePlan{{Name: "a", Replicas: 1}, {Name: "b", Replicas: 1}})
	var failure *Failure
	if !errors.As(err, &failure) || failure.Phase != "service-prepare" || !strings.Contains(err.Error(), "compose up a b") {
		t.Fatalf("batch up failure: %v", err)
	}
	for _, f := range failure.Services {
		if f.Convergence != "attempted" {
			t.Fatalf("facts %+v", failure.Services)
		}
	}
	if !strings.Contains(out.String(), "batch startup failed; sampling 2 of 2 Services") {
		t.Fatalf("batch not diagnosed:\n%s", out.String())
	}
	if strings.Count(out.String(), "deploy failed in phase") != 1 {
		t.Fatalf("failure reported twice:\n%s", out.String())
	}
}
