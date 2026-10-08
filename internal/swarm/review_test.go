package swarm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
)

const simStack = "yoho-shop-production"

// liveAll makes every service of testCompose run in the simulated Swarm.
func liveAll(sim *swarmSim) {
	sim.mu.Lock()
	defer sim.mu.Unlock()
	for n, r := range map[string]string{"web": "1/1", "worker": "2/2", "db": "1/1", "cloudflared": "1/1"} {
		sim.services[simStack+"_"+n] = svcStatus{Replicas: r}
	}
}

// currentFixture records the compiled stack and plan as the current Release
// and returns the Deploy that produced them.
func currentFixture(t *testing.T) (*plan.Deploy, *swarmSim) {
	t.Helper()
	withRoot(t)
	sim := newSim()
	d := testDeploy(t, newFake(sim), nil)
	key := bytesKey()
	writeHMAC(t, key)
	writeGenerated(t, "POSTGRES_PASSWORD", "generated-db-password")
	svc, _, err := mergeSecrets(d, map[string]string{"POSTGRES_PASSWORD": "generated-db-password"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := compile(d, compileInput{
		PinHost: "node-1", GenerationDir: release.SecretsDir(d.App, d.Destination, "20260101T000000Z-v1"),
		SvcSecrets: svc, HMACKey: key,
	})
	if err != nil {
		t.Fatal(err)
	}
	putCurrent(t, c.YAML)
	b, _ := json.Marshal(c.Plan)
	if err := os.WriteFile(filepath.Join(release.Dir("shop", "production", "v1"), "plan.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	liveAll(sim)
	return d, sim
}

func findChange(changes []plan.Change, kind, name string) plan.Change {
	for _, c := range changes {
		if c.Kind == kind && c.Name == name {
			return c
		}
	}
	return plan.Change{}
}

// Finding 1: a user constraint must not drop the Stateful pin.
func TestStatefulPinSurvivesUserConstraints(t *testing.T) {
	d := testDeploy(t, nil, nil)
	d.Project = loadProject(t, strings.Replace(testCompose, "  db:\n    image: postgres:17\n", "  db:\n    image: postgres:17\n    deploy:\n      placement:\n        constraints: [node.platform.os == linux]\n", 1))
	c, err := compile(d, compileInput{PinHost: "node-1", HMACKey: testKey})
	if err != nil {
		t.Fatal(err)
	}
	got := get(t, c.Doc, "services", "db", "deploy", "placement", "constraints").([]any)
	if len(got) != 2 || got[0] != "node.platform.os == linux" || got[1] != "node.hostname == node-1" {
		t.Fatalf("constraints = %v", got)
	}
	// The same pin written by the user is not duplicated.
	d.Project = loadProject(t, strings.Replace(testCompose, "  db:\n    image: postgres:17\n", "  db:\n    image: postgres:17\n    deploy:\n      placement:\n        constraints: [node.hostname==node-1]\n", 1))
	c, err = compile(d, compileInput{PinHost: "node-1", HMACKey: testKey})
	if err != nil {
		t.Fatal(err)
	}
	if got := get(t, c.Doc, "services", "db", "deploy", "placement", "constraints").([]any); len(got) != 1 {
		t.Fatalf("constraints = %v", got)
	}
	// A different node is an error (Backup assumes the first Server).
	d.Project = loadProject(t, strings.Replace(testCompose, "  db:\n    image: postgres:17\n", "  db:\n    image: postgres:17\n    deploy:\n      placement:\n        constraints: [node.hostname == worker-2]\n", 1))
	if _, err = compile(d, compileInput{PinHost: "node-1", HMACKey: testKey}); err == nil || !strings.Contains(err.Error(), "contradicts") {
		t.Fatalf("err = %v", err)
	}
}

// Finding 2: rotating a secrets_as_env secret must show in the plan.
func TestDiffSecretsAsEnvRotation(t *testing.T) {
	d, _ := currentFixture(t)
	ctx := context.Background()
	changes, err := Runtime{}.Diff(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if ch := findChange(changes, "service", "cloudflared"); ch.Action != plan.ActionNoop {
		t.Fatalf("unchanged: %+v", ch)
	}
	d.ServiceSecrets["cloudflared"]["TUNNEL_TOKEN"] = "rotated-tunnel-token"
	changes, err = Runtime{}.Diff(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	ch := findChange(changes, "service", "cloudflared")
	if ch.Action != plan.ActionUpdate || !contains(ch.Reasons, "secrets changed") {
		t.Fatalf("rotated: %+v", ch)
	}
	for _, c := range changes {
		for _, r := range c.Reasons {
			if strings.Contains(r, "rotated-tunnel-token") {
				t.Fatal("secret in plan reason")
			}
		}
	}
}

// Finding 3: proxy options alone change the route.
func TestDiffProxyOptions(t *testing.T) {
	d, _ := currentFixture(t)
	ctx := context.Background()
	web := d.Ext["web"]
	p := *web.Proxy
	p.Hosts = []string{"shop.example.com", "www.example.com"}
	p.HealthPath = "/healthz"
	web.Proxy = &p
	d.Ext["web"] = web
	changes, err := Runtime{}.Diff(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if ch := findChange(changes, "service", "web"); ch.Action != plan.ActionNoop {
		t.Fatalf("service must not change: %+v", ch)
	}
	ch := findChange(changes, "route", "shop-production-web")
	if ch.Action != plan.ActionUpdate || len(ch.Reasons) != 2 {
		t.Fatalf("route: %+v", ch)
	}
}

// Finding 4: stack or Service removed or scaled out of band.
func TestDiffLiveDrift(t *testing.T) {
	d, sim := currentFixture(t)
	ctx := context.Background()
	changes, err := Runtime{}.Diff(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range changes {
		if c.Action != plan.ActionNoop {
			t.Fatalf("in sync but %+v", c)
		}
	}
	sim.mu.Lock()
	delete(sim.services, simStack+"_cloudflared")
	sim.services[simStack+"_worker"] = svcStatus{Replicas: "0/0"}
	sim.services[simStack+"_web"] = svcStatus{Replicas: "0/1"} // crashing, not drift
	sim.mu.Unlock()
	changes, err = Runtime{}.Diff(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if ch := findChange(changes, "service", "cloudflared"); ch.Action != plan.ActionCreate || !contains(ch.Reasons, "missing from the Swarm") {
		t.Errorf("cloudflared %+v", ch)
	}
	if ch := findChange(changes, "service", "worker"); ch.Action != plan.ActionUpdate || len(ch.Reasons) != 1 || !strings.Contains(ch.Reasons[0], "replicas 0") {
		t.Errorf("worker %+v", ch)
	}
	if ch := findChange(changes, "service", "web"); ch.Action != plan.ActionNoop {
		t.Errorf("web %+v", ch)
	}
	// Whole stack removed.
	sim.mu.Lock()
	sim.services = map[string]svcStatus{}
	sim.mu.Unlock()
	changes, err = Runtime{}.Diff(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if ch := findChange(changes, "service", "db"); ch.Action != plan.ActionCreate {
		t.Errorf("db %+v", ch)
	}
}

// Finding 5: configs referenced by a Service reach the release stack.
func TestReleaseJobStackConfigs(t *testing.T) {
	d := testDeploy(t, nil, nil)
	cfgDir := t.TempDir()
	os.WriteFile(filepath.Join(cfgDir, "app.conf"), []byte("x"), 0o600)
	src := strings.Replace(testCompose, "    depends_on: [db]\n", "    depends_on: [db]\n    configs: [appconf, shared]\n", 1) +
		"configs:\n  appconf:\n    file: " + filepath.Join(cfgDir, "app.conf") + "\n  shared:\n    external: true\n    name: shared-cfg\n"
	d.Project = loadProject(t, src)
	c, err := compile(d, compileInput{PinHost: "node-1", HMACKey: testKey})
	if err != nil {
		t.Fatal(err)
	}
	b, err := releaseJobStack(c.Doc, simStack, "web", []string{"true"})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if get(t, doc, "configs", "shared", "external") != true {
		t.Errorf("shared:\n%s", b)
	}
	app := get(t, doc, "configs", "appconf").(map[string]any)
	if _, ok := app["name"]; ok || app["file"] == nil {
		t.Errorf("appconf = %v", app)
	}
}

// Finding 6: explicit replicas 0 stays 0 for non-stateful Services.
func TestCompileExplicitZeroReplicas(t *testing.T) {
	d := testDeploy(t, nil, nil)
	d.Project = loadProject(t, strings.Replace(testCompose, "    scale: 2\n", "    deploy:\n      replicas: 0\n", 1))
	c, err := compile(d, compileInput{PinHost: "node-1", HMACKey: testKey})
	if err != nil {
		t.Fatal(err)
	}
	if got := get(t, c.Doc, "services", "worker", "deploy", "replicas"); got != 0 {
		t.Errorf("worker replicas = %v", got)
	}
	for _, sp := range c.Plan.Services {
		if sp.Name == "worker" && (sp.Replicas != 0 || sp.Global) {
			t.Errorf("plan %+v", sp)
		}
		if sp.Name == "web" && sp.Replicas != 1 {
			t.Errorf("unset must stay 1: %+v", sp)
		}
	}
	// Stateful Services still run exactly one.
	d.Project = loadProject(t, strings.Replace(testCompose, "  db:\n    image: postgres:17\n", "  db:\n    image: postgres:17\n    deploy:\n      replicas: 0\n", 1))
	c, err = compile(d, compileInput{PinHost: "node-1", HMACKey: testKey})
	if err != nil {
		t.Fatal(err)
	}
	if got := get(t, c.Doc, "services", "db", "deploy", "replicas"); got != 1 {
		t.Errorf("db replicas = %v", got)
	}
}

// Finding 7: release command logs and task errors are redacted in errors.
func TestReleaseFailureRedactsSecrets(t *testing.T) {
	withRoot(t)
	fastPolling(t)
	sim := newSim()
	sim.jobState = "failed|1|exit"
	h := newFake(sim)
	inner := h.respond
	h.respond = func(script string) (string, error) {
		if strings.Contains(script, "docker service logs") {
			return "boom password=" + webSecret + " tunnel=" + tunnelSecret, nil
		}
		return inner(script)
	}
	d := testDeploy(t, h, nil)
	_, err := Runtime{}.Deploy(context.Background(), d)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), webSecret) {
		t.Fatalf("secret leaked in error: %v", err)
	}
}

var _ = config.ServiceExt{}
