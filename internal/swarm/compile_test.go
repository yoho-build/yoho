package swarm

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/yoho-build/yoho/internal/config"
)

var testKey = []byte("0123456789abcdef0123456789abcdef")

func compileTest(t *testing.T, mutate func(*compileInput)) (*compiled, map[string]any) {
	t.Helper()
	d := testDeploy(t, nil, nil)
	in := compileInput{
		PinHost:       "node-1",
		GenerationDir: "/srv/yoho/apps/shop/production/secrets/gen1",
		SvcSecrets: map[string]map[string]string{
			"web":         {"SECRET_KEY_BASE": webSecret, "DATABASE_PASSWORD": "pg-generated-PASS"},
			"db":          {"POSTGRES_PASSWORD": "pg-generated-PASS"},
			"cloudflared": {"TUNNEL_TOKEN": tunnelSecret},
		},
		HMACKey: testKey,
	}
	if mutate != nil {
		mutate(&in)
	}
	c, err := compile(d, in)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(c.YAML, &doc); err != nil {
		t.Fatal(err)
	}
	return c, doc
}

// get walks a decoded YAML document by keys.
func get(t *testing.T, v any, keys ...string) any {
	t.Helper()
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("at %q: not a map: %#v", k, v)
		}
		v, ok = m[k]
		if !ok {
			t.Fatalf("missing key %q in %v", k, keys)
		}
	}
	return v
}

func TestCompileUpdateConfig(t *testing.T) {
	_, doc := compileTest(t, nil)
	web := get(t, doc, "services", "web", "deploy")
	want := map[string]any{"order": "start-first", "parallelism": 1, "failure_action": "rollback", "monitor": "30s"}
	if got := get(t, web, "update_config"); !reflect.DeepEqual(got, want) {
		t.Errorf("web update_config = %v", got)
	}
	if got := get(t, web, "rollback_config"); !reflect.DeepEqual(got, map[string]any{"order": "start-first", "parallelism": 1}) {
		t.Errorf("web rollback_config = %v", got)
	}
	if got := get(t, doc, "services", "worker", "deploy", "replicas"); got != 2 {
		t.Errorf("worker replicas (from scale) = %v", got)
	}

	db := get(t, doc, "services", "db", "deploy")
	if got := get(t, db, "update_config", "order"); got != "stop-first" {
		t.Errorf("db order = %v", got)
	}
	if got := get(t, db, "rollback_config", "order"); got != "stop-first" {
		t.Errorf("db rollback order = %v", got)
	}
	if got := get(t, db, "replicas"); got != 1 {
		t.Errorf("db replicas = %v", got)
	}
	if got := get(t, db, "placement", "constraints"); !reflect.DeepEqual(got, []any{"node.hostname == node-1"}) {
		t.Errorf("db placement = %v", got)
	}
}

func TestCompileKeepsUserPlacement(t *testing.T) {
	d := testDeploy(t, nil, nil)
	src := strings.Replace(testCompose, "  db:\n    image: postgres:17\n", "  db:\n    image: postgres:17\n    deploy:\n      placement:\n        constraints: [node.labels.db == true]\n", 1)
	d.Project = loadProject(t, src)
	c, err := compile(d, compileInput{PinHost: "node-1", HMACKey: testKey})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(c.YAML), "node.labels.db == true") || strings.Contains(string(c.YAML), "node-1") {
		t.Errorf("user placement must win:\n%s", c.YAML)
	}
}

func TestCompileSecrets(t *testing.T) {
	c, doc := compileTest(t, nil)
	out := string(c.YAML)
	for _, v := range []string{webSecret, "pg-generated-PASS", "tunnel-TOKEN"} {
		if strings.Contains(out, v) {
			t.Fatalf("stack file contains a secret value %q:\n%s", v, out)
		}
	}
	skb := SecretName("yoho-shop-production", "SECRET_KEY_BASE", webSecret, testKey)
	if !strings.HasPrefix(skb, "yoho-shop-production_SECRET_KEY_BASE_") || len(skb) != len("yoho-shop-production_SECRET_KEY_BASE_")+8 {
		t.Errorf("secret name %q", skb)
	}
	if got := get(t, doc, "secrets", skb); !reflect.DeepEqual(got, map[string]any{"external": true, "name": skb}) {
		t.Errorf("top-level secret = %v", got)
	}
	webSecrets := get(t, doc, "services", "web", "secrets").([]any)
	found := false
	for _, s := range webSecrets {
		if reflect.DeepEqual(s, map[string]any{"source": skb, "target": "SECRET_KEY_BASE"}) {
			found = true
		}
	}
	if !found || len(webSecrets) != 2 {
		t.Errorf("web secrets = %v", webSecrets)
	}
	if got := get(t, doc, "services", "web", "environment", "SECRET_KEY_BASE_FILE"); got != "/run/secrets/SECRET_KEY_BASE" {
		t.Errorf("_FILE env = %v", got)
	}
	// Same NAME and value in two Services share one secret; plan lists each once.
	if len(c.Plan.Secrets) != 3 {
		t.Errorf("plan secrets = %v", c.Plan.Secrets)
	}
	// secrets_as_env: env_file path in the generation, nothing in Swarm.
	if got := get(t, doc, "services", "cloudflared", "env_file"); !reflect.DeepEqual(got, []any{"/srv/yoho/apps/shop/production/secrets/gen1/cloudflared.env"}) {
		t.Errorf("cloudflared env_file = %v", got)
	}
	if !c.Plan.EnvFiles {
		t.Error("plan.EnvFiles")
	}
}

func TestSecretName(t *testing.T) {
	a := SecretName("s", "K", "v1", testKey)
	if a != SecretName("s", "K", "v1", testKey) {
		t.Error("not deterministic")
	}
	if a == SecretName("s", "K", "v2", testKey) || a == SecretName("s", "K", "v1", []byte("other-key-other-key")) {
		t.Error("must change with value and key")
	}
	long := SecretName(strings.Repeat("a", 50), "VERY_LONG_SECRET_NAME", "v", testKey)
	if len(long) > 64 {
		t.Errorf("len %d > 64: %s", len(long), long)
	}
}

func TestCompileSanitizes(t *testing.T) {
	c, doc := compileTest(t, nil)
	if _, ok := doc["name"]; ok {
		t.Error("top-level name must be dropped")
	}
	for _, k := range []string{"depends_on", "scale", "pull_policy"} {
		for svc, s := range doc["services"].(map[string]any) {
			if _, ok := s.(map[string]any)[k]; ok {
				t.Errorf("service %s still has %s", svc, k)
			}
		}
	}
	port := get(t, doc, "services", "cloudflared", "ports").([]any)[0].(map[string]any)
	if port["published"] != 9000 || port["host_ip"] != nil {
		t.Errorf("port = %v", port)
	}
	if !strings.Contains(strings.Join(c.Warnings, "\n"), "host_ip") {
		t.Errorf("warnings = %v", c.Warnings)
	}
	// Stack-namespaced volume (no loader-generated name).
	if v := get(t, doc, "volumes", "pgdata"); v != nil {
		t.Errorf("volume pgdata = %v", v)
	}
	if got := get(t, doc, "services", "web", "environment", "PRICE"); got != "$$5" {
		t.Errorf("dollar not escaped: %v", got)
	}
	env := get(t, doc, "services", "web", "environment").(map[string]any)
	if env["YOHO_VERSION"] != "v1" || env["YOHO_SERVER"] != "{{.Node.Hostname}}" || env["YOHO_SERVICE"] != "web" {
		t.Errorf("YOHO env = %v", env)
	}
	if _, ok := get(t, doc, "services", "db", "environment").(map[string]any)["YOHO_VERSION"]; ok {
		t.Error("YOHO_VERSION on a third-party image")
	}
	if got := get(t, doc, "services", "web", "deploy", "labels", "yoho.service"); got != "web" {
		t.Errorf("deploy label = %v", got)
	}
}

func TestCompileProxy(t *testing.T) {
	_, doc := compileTest(t, nil)
	nets := get(t, doc, "services", "web", "networks").(map[string]any)
	if _, ok := nets["yoho"]; !ok {
		t.Errorf("web networks = %v", nets)
	}
	if _, ok := nets["default"]; !ok {
		t.Errorf("web networks = %v", nets)
	}
	if got := get(t, doc, "networks", "yoho"); !reflect.DeepEqual(got, map[string]any{"external": true, "name": "yoho"}) {
		t.Errorf("yoho network = %v", got)
	}
	if _, ok := get(t, doc, "services", "web", "deploy").(map[string]any)["endpoint_mode"]; ok {
		t.Error("endpoint_mode without strict_drain")
	}

	d := testDeploy(t, nil, nil)
	e := d.Ext["web"]
	e.StrictDrain = true
	d.Ext["web"] = e
	c, err := compile(d, compileInput{PinHost: "n", HMACKey: testKey})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(c.YAML), "endpoint_mode: dnsrr") || !c.Plan.Services[2].StrictDrain {
		t.Errorf("strict_drain:\n%s", c.YAML)
	}
}

func TestValidate(t *testing.T) {
	d := testDeploy(t, &fakeHost{}, nil)
	if err := validate(d); err != nil {
		t.Fatal(err)
	}
	e := d.Ext["db"]
	e.Proxy = &config.ServiceProxy{}
	d.Ext["db"] = e
	if err := validate(d); err == nil || !strings.Contains(err.Error(), "stateful") {
		t.Errorf("err = %v", err)
	}
}

func TestReleaseJobStack(t *testing.T) {
	c, _ := compileTest(t, nil)
	b, err := releaseJobStack(c.Doc, "yoho-shop-production", "web", []string{"bin/rails", "db:migrate"})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	s := get(t, doc, "services", "web").(map[string]any)
	if !reflect.DeepEqual(s["command"], []any{"bin/rails", "db:migrate"}) {
		t.Errorf("command = %v", s["command"])
	}
	if get(t, s, "deploy", "mode") != "replicated-job" || get(t, s, "deploy", "restart_policy", "condition") != "none" {
		t.Errorf("deploy = %v", s["deploy"])
	}
	if get(t, s, "healthcheck", "disable") != true {
		t.Error("healthcheck must be disabled")
	}
	if _, ok := s["ports"]; ok {
		t.Error("ports must be dropped")
	}
	if got := get(t, doc, "networks", "default"); !reflect.DeepEqual(got, map[string]any{"external": true, "name": "yoho-shop-production_default"}) {
		t.Errorf("default network = %v", got)
	}
	skb := SecretName("yoho-shop-production", "SECRET_KEY_BASE", webSecret, testKey)
	if get(t, doc, "secrets", skb, "external") != true {
		t.Error("secret must be external")
	}
	if strings.Contains(string(b), webSecret) {
		t.Error("secret value in release stack")
	}

	// Volumes of the main stack are joined by real name.
	b, err = releaseJobStack(c.Doc, "yoho-shop-production", "db", []string{"true"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "name: yoho-shop-production_pgdata") || !strings.Contains(string(b), "node.hostname == node-1") {
		t.Errorf("db job:\n%s", b)
	}
}

// TestCompileValidDockerStack checks the output against the real docker
// CLI schema (`docker stack config` needs no Swarm).
func TestCompileValidDockerStack(t *testing.T) {
	if os.Getenv("YOHO_E2E") != "1" {
		t.Skip("YOHO_E2E=1 not set")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker CLI")
	}
	gen := t.TempDir()
	os.WriteFile(filepath.Join(gen, "cloudflared.env"), []byte("TUNNEL_TOKEN=\"x\"\n"), 0o600)
	c, _ := compileTest(t, func(in *compileInput) { in.GenerationDir = gen })
	job, err := releaseJobStack(c.Doc, "yoho-shop-production", "web", []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string][]byte{"stack": c.YAML, "job": job} {
		f := filepath.Join(t.TempDir(), "stack.yaml")
		os.WriteFile(f, b, 0o600)
		out, err := exec.Command("docker", "stack", "config", "-c", f).CombinedOutput()
		if err != nil {
			t.Errorf("%s: docker stack config: %v\n%s\n---\n%s", name, err, out, b)
		}
	}
}
