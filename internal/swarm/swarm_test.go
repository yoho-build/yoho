package swarm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// swarmSim answers the docker commands of the runtime like a one-node Swarm.
type swarmSim struct {
	mu        sync.Mutex
	services  map[string]svcStatus // stack service name -> status
	updates   int
	jobState  string
	failMain  bool // main stack update rolls back
	failOnce  bool // only the next main stack update rolls back
	notSwarm  bool
	secretsLs string
}

func newSim() *swarmSim {
	return &swarmSim{services: map[string]svcStatus{}, jobState: "complete||"}
}

func (s *swarmSim) respond(script string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	const stack = "yoho-shop-production"
	switch {
	case strings.Contains(script, "{{.Swarm.ControlAvailable}}"):
		if s.notSwarm {
			return "inactive|false|node-1", nil
		}
		return "active|true|node-1", nil
	case strings.Contains(script, "{{.Swarm.LocalNodeState}}"):
		if s.notSwarm {
			return "inactive", nil
		}
		return "active", nil
	case strings.Contains(script, "docker stack deploy"):
		switch {
		case strings.Contains(script, "dependencies.yaml"):
			s.services[stack+"_db"] = svcStatus{Replicas: "1/1"}
		case strings.Contains(script, "release-web.yaml"):
		default:
			s.updates++
			failing := s.failMain || s.failOnce
			s.failOnce = false
			for _, n := range []string{"web", "worker", "db", "cloudflared"} {
				st := svcStatus{Replicas: "1/1"}
				if n == "worker" {
					st.Replicas = "2/2"
				}
				if old, ok := s.services[stack+"_"+n]; ok {
					st.UpdateStarted = "t" + string(rune('0'+s.updates))
					st.UpdateState = "completed"
					if failing && n == "web" {
						st.UpdateState = "rollback_completed"
						st.UpdateMessage = "update rolled back due to failure"
					}
					_ = old
				}
				s.services[stack+"_"+n] = st
			}
		}
		return "", nil
	case strings.Contains(script, "docker service ls -q --filter"):
		var b strings.Builder
		for _, n := range sortedKeys(s.services) {
			st := s.services[n]
			b.WriteString("S|" + n + "|")
			if st.UpdateStarted != "" {
				b.WriteString(st.UpdateStarted + "|" + st.UpdateState + "|" + st.UpdateMessage)
			}
			b.WriteString("\n")
		}
		for _, n := range sortedKeys(s.services) {
			b.WriteString("R|" + n + "|" + s.services[n].Replicas + "\n")
		}
		return b.String(), nil
	case strings.Contains(script, "docker service ps -q"):
		return s.jobState, nil
	case strings.Contains(script, "docker service ps --no-trunc"):
		return "yoho-shop-production_web.1 on node-1: Failed 3 seconds ago task: non-zero exit (1)", nil
	case strings.Contains(script, "docker service logs"):
		return "migration exploded", nil
	case strings.Contains(script, "docker secret ls"):
		return s.secretsLs, nil
	}
	return "", nil
}

func newFake(sim *swarmSim) *fakeHost {
	return &fakeHost{name: "primary", local: &remote.Local{}, respond: sim.respond}
}

// indexOf returns the index of the first recorded script containing all subs.
func indexOf(h *fakeHost, subs ...string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, s := range h.scripts {
		ok := true
		for _, sub := range subs {
			if !strings.Contains(s, sub) {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}

func TestDeploySequence(t *testing.T) {
	withRoot(t)
	fastPolling(t)
	sim := newSim()
	sim.secretsLs = "yoho-shop-production_OLD_deadbeef\n"
	h := newFake(sim)
	var out bytes.Buffer
	d := testDeploy(t, h, &out)
	rel, err := Runtime{}.Deploy(context.Background(), d)
	if err != nil {
		t.Fatalf("%v\n%s\n%s", err, out.String(), h.all())
	}
	if rel.Status != "deployed" || rel.Runtime != "swarm" || rel.Server != "primary" {
		t.Errorf("release = %+v", rel)
	}

	steps := []int{
		indexOf(h, "{{.Swarm.ControlAvailable}}"),
		indexOf(h, "docker secret create"),
		indexOf(h, "docker network create -d overlay --attachable yoho"),
		indexOf(h, "docker stack deploy", "dependencies.yaml", "'--detach=false'"),
		indexOf(h, "docker stack deploy", "release-web.yaml", "'yoho-shop-production-release'"),
		indexOf(h, "docker service ps -q 'yoho-shop-production-release_web'"),
		indexOf(h, "docker stack deploy", "/compose.yaml", "'--prune'", "'--resolve-image' 'never'", "'--detach=false'", "'yoho-shop-production'"),
		indexOf(h, "kamal-proxy' 'deploy' 'shop-production-web' '--target' 'yoho-shop-production_web:3000'"),
		indexOf(h, "docker secret rm"),
	}
	t.Logf("steps %v", steps)
	for i, s := range steps {
		if s < 0 || (i > 0 && s < steps[i-1]) {
			t.Fatalf("step %d out of order: %v\n%s", i, steps, h.all())
		}
	}
	if i := indexOf(h, "docker stack deploy", "dependencies.yaml", "--prune"); i >= 0 {
		t.Error("dependencies must not prune the stack")
	}
	if indexOf(h, "--with-registry-auth") >= 0 {
		t.Error("no registry images, no --with-registry-auth")
	}
	if indexOf(h, "docker stack rm 'yoho-shop-production-release'") < 0 {
		t.Error("release stack not removed")
	}
	if i := indexOf(h, "docker secret rm"); !strings.Contains(h.scripts[i], "yoho-shop-production_OLD_deadbeef") {
		t.Errorf("prune: %s", h.scripts[i])
	}

	// Secret values only ever travel on stdin.
	gen, _ := os.ReadFile(filepath.Join(release.AppDir("shop", "production"), "generated", "POSTGRES_PASSWORD"))
	all := h.all()
	for _, v := range []string{webSecret, tunnelSecret, string(gen)} {
		if strings.Contains(all, v) {
			t.Fatalf("secret value %q in a script", v)
		}
	}
	stdins := strings.Join(h.stdins, "\n")
	if !strings.Contains(stdins, webSecret) || !strings.Contains(stdins, string(gen)) {
		t.Error("secret values not created via stdin")
	}
	var creates int
	for _, s := range h.scripts {
		if strings.Contains(s, "docker secret create") {
			creates++
			if !strings.Contains(s, "'--label' 'yoho.app=shop'") {
				t.Errorf("secret without labels: %s", s)
			}
		}
	}
	if creates != 3 { // web: SECRET_KEY_BASE, DATABASE_PASSWORD; db: POSTGRES_PASSWORD (same value, different NAME)
		t.Errorf("secret creates = %d", creates)
	}

	envFile := filepath.Join(release.SecretsDir("shop", "production", rel.SecretsGeneration), "cloudflared.env")
	fi, err := os.Stat(envFile)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("env file: %v %v", fi, err)
	}
	stackFile, err := os.ReadFile(filepath.Join(release.Dir("shop", "production", "v1"), "compose.yaml"))
	if err != nil || strings.Contains(string(stackFile), webSecret) {
		t.Errorf("stack file: %v", err)
	}
	cur, _ := os.Readlink(filepath.Join(release.AppDir("shop", "production"), "current"))
	if cur != "releases/v1" {
		t.Errorf("current -> %s", cur)
	}
}

func TestProxyOnEveryNode(t *testing.T) {
	withRoot(t)
	fastPolling(t)
	sim := newSim()
	mgr := newFake(sim)
	edge := &fakeHost{name: "edge", local: &remote.Local{}, respond: sim.respond}
	d := testDeploy(t, mgr, nil)
	d.Servers = append(d.Servers, plan.NamedHost{Name: "edge", Server: config.Server{SSH: "yoho@203.0.113.11"}, Host: edge})
	if _, err := (Runtime{}).Deploy(context.Background(), d); err != nil {
		t.Fatalf("%v\nmanager:\n%s\nedge:\n%s", err, mgr.all(), edge.all())
	}
	if indexOf(mgr, "docker network create -d overlay --attachable yoho") < 0 {
		t.Fatal("manager did not create the overlay")
	}
	if indexOf(edge, "docker network create -d overlay") >= 0 {
		t.Fatal("worker must not create the overlay")
	}
	for _, h := range []*fakeHost{mgr, edge} {
		if indexOf(h, "kamal-proxy' 'deploy' 'shop-production-web'") < 0 || indexOf(h, "yoho-proxy") < 0 {
			t.Fatalf("%s missing proxy boot or route\n%s", h.name, h.all())
		}
	}

	d.Version = "v2"
	web := d.Ext["web"]
	web.Proxy = nil
	d.Ext["web"] = web
	mgr.scripts, edge.scripts = nil, nil
	if _, err := (Runtime{}).Deploy(context.Background(), d); err != nil {
		t.Fatalf("%v\n%s\n%s", err, mgr.all(), edge.all())
	}
	for _, h := range []*fakeHost{mgr, edge} {
		if indexOf(h, "kamal-proxy' 'remove' 'shop-production-web'") < 0 {
			t.Fatalf("%s did not remove the route\n%s", h.name, h.all())
		}
	}
}

func TestDeployWithRegistryAuth(t *testing.T) {
	withRoot(t)
	fastPolling(t)
	h := newFake(newSim())
	d := testDeploy(t, h, nil)
	d.Registry = &config.Registry{Server: "ghcr.io", Username: "me"}
	if _, err := (Runtime{}).Deploy(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if indexOf(h, "docker stack deploy", "/compose.yaml", "--with-registry-auth") < 0 {
		t.Fatalf("flag missing\n%s", h.all())
	}
}

func TestDeployNotASwarm(t *testing.T) {
	withRoot(t)
	sim := newSim()
	sim.notSwarm = true
	_, err := Runtime{}.Deploy(context.Background(), testDeploy(t, newFake(sim), nil))
	if err == nil || !strings.Contains(err.Error(), "yoho swarm init") {
		t.Errorf("err = %v", err)
	}
}

func TestDeployRolledBack(t *testing.T) {
	withRoot(t)
	fastPolling(t)
	sim := newSim()
	h := newFake(sim)
	d := testDeploy(t, h, nil)
	if _, err := (Runtime{}).Deploy(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	sim.failMain = true
	d.Version = "v2"
	d.Project.Services["web"] = func() (s types.ServiceConfig) { s = d.Project.Services["web"]; s.Image = "shop-web:v2"; return }()
	_, err := Runtime{}.Deploy(context.Background(), d)
	if err == nil || !strings.Contains(err.Error(), "rolled back") || !strings.Contains(err.Error(), "non-zero exit") {
		t.Fatalf("err = %v", err)
	}
	r, rerr := Runtime{}.Releases(context.Background(), d)
	if rerr != nil || len(r) != 2 || r[0].Version != "v2" || r[0].Status != "failed" {
		t.Errorf("releases = %+v %v", r, rerr)
	}
}

func TestReleaseCommandFailureAborts(t *testing.T) {
	withRoot(t)
	fastPolling(t)
	sim := newSim()
	sim.jobState = "failed|1|task: non-zero exit (1)"
	h := newFake(sim)
	_, err := Runtime{}.Deploy(context.Background(), testDeploy(t, h, nil))
	if err == nil || !strings.Contains(err.Error(), "migration exploded") {
		t.Fatalf("err = %v", err)
	}
	if indexOf(h, "docker stack deploy", "/compose.yaml") >= 0 {
		t.Error("main stack deployed after a failed release command")
	}
	if indexOf(h, "docker stack rm 'yoho-shop-production-release'") < 0 {
		t.Error("release stack not cleaned up")
	}
}

func TestRollback(t *testing.T) {
	withRoot(t)
	fastPolling(t)
	sim := newSim()
	h := newFake(sim)
	d := testDeploy(t, h, nil)
	ctx := context.Background()
	if _, err := (Runtime{}).Deploy(ctx, d); err != nil {
		t.Fatal(err)
	}
	d.Version = "v2"
	if _, err := (Runtime{}).Deploy(ctx, d); err != nil {
		t.Fatal(err)
	}
	h.scripts, h.stdins = nil, nil
	rel, err := Runtime{}.Rollback(ctx, d, "v1")
	if err != nil {
		t.Fatalf("%v\n%s", err, h.all())
	}
	if rel.Version != "v1" || rel.Status != "deployed" {
		t.Errorf("rel = %+v", rel)
	}
	if indexOf(h, "docker secret inspect") < 0 || indexOf(h, "docker stack deploy", "releases/v1/compose.yaml", "'--prune'") < 0 {
		t.Errorf("rollback scripts:\n%s", h.all())
	}
	if indexOf(h, "release-web.yaml") >= 0 {
		t.Error("rollback must not run release commands")
	}
	rels, _ := Runtime{}.Releases(ctx, d)
	if rels[0].Version != "v1" || rels[1].Status != "rolled_back" {
		t.Errorf("releases = %+v", rels)
	}
}

func TestConverged(t *testing.T) {
	before := map[string]svcStatus{
		"s_old":   {UpdateStarted: "t1", UpdateState: "rollback_completed", Replicas: "1/1"},
		"s_web":   {UpdateStarted: "t1", UpdateState: "completed", Replicas: "1/1"},
		"s_flaky": {Replicas: "0/1"},
	}
	after := map[string]svcStatus{
		"s_old":   {UpdateStarted: "t1", UpdateState: "rollback_completed", Replicas: "1/1"}, // stale: not ours
		"s_web":   {UpdateStarted: "t2", UpdateState: "updating", Replicas: "1/1"},
		"s_flaky": {Replicas: "0/1"}, // untouched by this deploy
		"s_new":   {Replicas: "0/2"},
		"s_job":   {Replicas: "0/1 (1/1 completed)"},
	}
	pending, failed := converged(before, after)
	if !reflect.DeepEqual(pending, []string{"s_new", "s_web"}) || len(failed) != 0 {
		t.Errorf("pending=%v failed=%v", pending, failed)
	}
	after["s_web"] = svcStatus{UpdateStarted: "t2", UpdateState: "rollback_completed", Replicas: "1/1"}
	after["s_new"] = svcStatus{Replicas: "2/2"}
	pending, failed = converged(before, after)
	if len(pending) != 0 || !reflect.DeepEqual(failed, []string{"s_web"}) {
		t.Errorf("pending=%v failed=%v", pending, failed)
	}
}

func TestParseStatus(t *testing.T) {
	m := parseStatus("S|a|2026-10-08 10:00:00 +0000 UTC|completed|update completed\nS|b|\nR|a|2/2\nR|b|1/1 (max 1 per node)\n")
	if m["a"].UpdateState != "completed" || m["a"].Replicas != "2/2" || m["b"].UpdateStarted != "" || !replicasReady(m["b"].Replicas) {
		t.Errorf("%+v", m)
	}
	for in, want := range map[string]bool{"0/1 (1/1 completed)": true, "0/1 (0/1 completed)": false, "0/2 (1/2 completed)": false, "1/1": true, "": false} {
		if replicasReady(in) != want {
			t.Errorf("replicasReady(%q) != %v", in, want)
		}
	}
	if replicasReady("1/2") || !replicasReady("3/3") {
		t.Error("replicasReady")
	}
}

func TestUnusedSecrets(t *testing.T) {
	got := unusedSecrets([]string{"s_B_2", "s_A_1", "s_A_0"}, map[string]bool{"s_A_1": true})
	if !reflect.DeepEqual(got, []string{"s_A_0", "s_B_2"}) {
		t.Errorf("%v", got)
	}
}

func TestPruneKeepsRetainedSecrets(t *testing.T) {
	withRoot(t)
	fastPolling(t)
	sim := newSim()
	h := newFake(sim)
	d := testDeploy(t, h, nil)
	d.RetainReleases = 1
	ctx := context.Background()
	if _, err := (Runtime{}).Deploy(ctx, d); err != nil {
		t.Fatal(err)
	}
	p1, _ := readPlan(ctx, h, release.Dir("shop", "production", "v1"))
	d.Version = "v2"
	d.ServiceSecrets["web"]["SECRET_KEY_BASE"] = "rotated-value"
	sim.secretsLs = strings.Join(p1.Secrets, "\n")
	h.scripts = nil
	if _, err := (Runtime{}).Deploy(ctx, d); err != nil {
		t.Fatal(err)
	}
	p2, _ := readPlan(ctx, h, release.Dir("shop", "production", "v2"))
	i := indexOf(h, "docker secret rm")
	if i < 0 {
		t.Fatal("no secret rm")
	}
	rm := h.scripts[i]
	for _, s := range p1.Secrets {
		kept := false
		for _, s2 := range p2.Secrets {
			kept = kept || s == s2
		}
		if kept == strings.Contains(rm, s) {
			t.Errorf("secret %s kept=%v but rm script: %s", s, kept, rm)
		}
	}
	if _, err := os.Stat(release.Dir("shop", "production", "v1")); !errors.Is(err, os.ErrNotExist) {
		t.Error("v1 release dir should be pruned with retain 1")
	}
	if indexOf(h, "docker image ls -a", "--filter 'label=yoho.app=shop'", "--no-trunc") < 0 {
		t.Error("swarm image cleanup did not list this App's images")
	}
	if strings.Contains(h.all(), "docker image rm") && strings.Contains(h.all(), "--force") {
		t.Error("image removal must not be forced")
	}
}

// releaseGuard fails closed if a worker tries to read Release records.
// Those exist only on the manager; image pruning must use the passed keep-set.
type releaseGuard struct {
	*fakeHost
	readRelease bool
}

func (g *releaseGuard) Run(ctx context.Context, c remote.Cmd) error {
	_, err := g.Output(ctx, c)
	return err
}

func (g *releaseGuard) Output(ctx context.Context, c remote.Cmd) (string, error) {
	if strings.Contains(c.Script, "release.json") {
		g.readRelease = true
		return "", errors.New("release records are only on the manager")
	}
	return g.fakeHost.Output(ctx, c)
}

func TestWorkerPrunesUnusedImages(t *testing.T) {
	withRoot(t)
	fastPolling(t)
	sim := newSim()
	mgr := newFake(sim)
	edge := &releaseGuard{fakeHost: &fakeHost{name: "edge", local: &remote.Local{}, respond: func(script string) (string, error) {
		switch {
		case strings.Contains(script, "docker image ls"):
			return strings.Join([]string{
				"sha256:stale|shop-web|old|0",
				"sha256:used|shop-web|live|0",
				"sha256:kept|shop-web|v1|0",
			}, "\n"), nil
		case strings.Contains(script, "docker ps -a"):
			return "shop-web:live\n", nil
		default:
			return sim.respond(script)
		}
	}}}
	d := testDeploy(t, mgr, nil)
	d.Servers = append(d.Servers, plan.NamedHost{Name: "edge", Server: config.Server{SSH: "yoho@203.0.113.11"}, Host: edge})
	if _, err := (Runtime{}).Deploy(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if edge.readRelease {
		t.Fatal("worker read Release records")
	}
	all := edge.all()
	if indexOf(edge.fakeHost, "docker image ls -a", "--filter 'label=yoho.app=shop'") < 0 {
		t.Fatalf("worker did not list images:\n%s", all)
	}
	if indexOf(edge.fakeHost, "docker image rm 'sha256:stale'") < 0 {
		t.Fatalf("worker did not remove the unused image:\n%s", all)
	}
	for _, id := range []string{"sha256:used", "sha256:kept"} {
		if indexOf(edge.fakeHost, "docker image rm '"+id+"'") >= 0 {
			t.Errorf("worker removed %s:\n%s", id, all)
		}
	}
	if strings.Contains(all, "--force") {
		t.Error("image removal must not be forced")
	}
}

func TestWorkerNeverCreatesLocalNetwork(t *testing.T) {
	withRoot(t)
	fastPolling(t)
	sim := newSim()
	mgr := newFake(sim)
	edge := &fakeHost{name: "edge", local: &remote.Local{}, respond: sim.respond}
	d := testDeploy(t, mgr, nil)
	d.Servers = append(d.Servers, plan.NamedHost{Name: "edge", Server: config.Server{SSH: "yoho@203.0.113.11"}, Host: edge})
	if _, err := (Runtime{}).Deploy(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	for _, h := range []*fakeHost{mgr, edge} {
		if indexOf(h, "docker network create yoho") >= 0 || indexOf(h, "docker network create "+"'yoho'") >= 0 {
			t.Errorf("%s created a plain yoho network:\n%s", h.name, h.all())
		}
		if indexOf(h, "docker run -d", "yoho-proxy") < 0 {
			t.Errorf("%s did not boot the proxy", h.name)
		}
	}
}

func TestStaleBridgeRemoval(t *testing.T) {
	var out bytes.Buffer
	r := &runner{out: &out}
	edge := &fakeHost{name: "edge", local: &remote.Local{}, respond: func(string) (string, error) { return "bridge|local\n", nil }}
	if err := r.removeStaleBridge(context.Background(), plan.NamedHost{Name: "edge", Host: edge}); err != nil {
		t.Fatal(err)
	}
	all := edge.all()
	for _, want := range []string{"docker rm -f yoho-proxy", "docker network rm yoho"} {
		if !strings.Contains(all, want) {
			t.Errorf("script missing %q:\n%s", want, all)
		}
	}
	if !strings.Contains(out.String(), "warning: removed node-local docker network yoho (bridge|local)") {
		t.Errorf("no warning: %q", out.String())
	}

	// Other containers attached: the script exits 3 and we explain the migration.
	busy := &fakeHost{name: "edge", local: &remote.Local{}, respond: func(string) (string, error) {
		return "bridge|local: web-1", &remote.ExitError{Code: 3}
	}}
	err := r.removeStaleBridge(context.Background(), plan.NamedHost{Name: "edge", Host: busy})
	if err == nil || !strings.Contains(err.Error(), "needs downtime") || !strings.Contains(err.Error(), "web-1") {
		t.Errorf("err = %v", err)
	}
}

func TestRoutesSkipZeroReplicaServicesAndRemoveTheirRoute(t *testing.T) {
	withRoot(t)
	h := &fakeHost{name: "primary", local: &remote.Local{}, respond: func(string) (string, error) { return "", nil }}
	d := testDeploy(t, h, nil)
	px := &config.ServiceProxy{Hosts: []string{"x.example.com"}, Port: 80, HealthPath: "/up"}
	// Previous Release served web through the Proxy.
	dir := release.Dir("shop", "production", "v1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	prev := stackPlan{Stack: "yoho-shop-production", Services: []servicePlan{{Name: "web", Replicas: 2, Proxy: px}}}
	b, _ := json.Marshal(prev)
	if err := os.WriteFile(filepath.Join(dir, "plan.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/v1", filepath.Join(release.AppDir("shop", "production"), "current")); err != nil {
		t.Fatal(err)
	}
	next := stackPlan{Stack: "yoho-shop-production", Services: []servicePlan{
		{Name: "web", Mode: "replicated", Replicas: 0, Proxy: px},
		{Name: "api", Mode: "replicated", Replicas: 2, Proxy: px},
		{Name: "edge", Mode: "global", Proxy: px},
	}}
	var out bytes.Buffer
	r := &runner{h: h, out: &out, app: "shop", dest: "production"}
	if err := r.routes(context.Background(), d, next); err != nil {
		t.Fatal(err)
	}
	all := h.all()
	if strings.Contains(all, "'deploy' '"+deploy.RouteName("shop", "production", "web")+"'") {
		t.Errorf("zero-replica Service was deployed to the Proxy:\n%s", all)
	}
	for _, n := range []string{"api", "edge"} {
		if !strings.Contains(all, "'deploy' '"+deploy.RouteName("shop", "production", n)+"'") {
			t.Errorf("route %s not deployed:\n%s", n, all)
		}
	}
	if !strings.Contains(all, "'remove' '"+deploy.RouteName("shop", "production", "web")+"'") {
		t.Errorf("zero-replica route not removed:\n%s", all)
	}
}
