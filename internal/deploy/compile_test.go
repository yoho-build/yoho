package deploy

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/plan"
)

func compileTest(t *testing.T, d *plan.Deploy) ([]byte, []servicePlan, map[string]map[string]string) {
	t.Helper()
	gen := map[string]string{"POSTGRES_PASSWORD": "generated-PG-password"}
	svc, err := serviceSecrets(d, gen, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	out, plans, err := compile(d, "primary", "/srv/yoho/apps/shop/production/secrets/G1", svc, []byte("0123456789abcdef0123456789abcdef"), "")
	if err != nil {
		t.Fatal(err)
	}
	return out, plans, svc
}

func TestCompileContainsNoSecretValues(t *testing.T) {
	d := testDeploy(t, nil, nil)
	out, _, svc := compileTest(t, d)
	s := string(out)
	for _, m := range svc {
		for name, v := range m {
			if strings.Contains(s, v) {
				t.Errorf("compiled compose contains the value of %s", name)
			}
		}
	}
	for _, v := range []string{webSecret, "generated-PG-password", "tunnel-TOKEN"} {
		if strings.Contains(s, v) {
			t.Errorf("compiled compose leaks %q", v)
		}
	}
}

type compiledDoc struct {
	Name     string `yaml:"name"`
	Services map[string]struct {
		Environment map[string]string `yaml:"environment"`
		Labels      map[string]string `yaml:"labels"`
		Networks    map[string]any    `yaml:"networks"`
		EnvFile     []struct {
			Path string `yaml:"path"`
		} `yaml:"env_file"`
		Secrets []struct {
			Source string `yaml:"source"`
			Target string `yaml:"target"`
		} `yaml:"secrets"`
	} `yaml:"services"`
	Secrets map[string]struct {
		File string `yaml:"file"`
	} `yaml:"secrets"`
	Networks map[string]struct {
		Name     string `yaml:"name"`
		External bool   `yaml:"external"`
	} `yaml:"networks"`
	Volumes map[string]struct {
		Name string `yaml:"name"`
	} `yaml:"volumes"`
}

func TestCompileLayout(t *testing.T) {
	d := testDeploy(t, nil, nil)
	out, plans, _ := compileTest(t, d)
	var doc compiledDoc
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if doc.Name != "yoho-shop-production" {
		t.Errorf("project name %q", doc.Name)
	}

	web := doc.Services["web"]
	if web.Environment["DATABASE_PASSWORD_FILE"] != "/run/secrets/DATABASE_PASSWORD" || web.Environment["SECRET_KEY_BASE_FILE"] != "/run/secrets/SECRET_KEY_BASE" {
		t.Errorf("web _FILE env: %v", web.Environment)
	}
	if len(web.Secrets) != 2 || web.Secrets[0].Source != "web.DATABASE_PASSWORD" || web.Secrets[0].Target != "DATABASE_PASSWORD" {
		t.Errorf("web secrets: %+v", web.Secrets)
	}
	if f := doc.Secrets["web.SECRET_KEY_BASE"].File; f != "/srv/yoho/apps/shop/production/secrets/G1/web/SECRET_KEY_BASE" {
		t.Errorf("secret file path %q", f)
	}
	if _, ok := web.Networks["yoho"]; !ok {
		t.Errorf("proxied web not on yoho network: %v", web.Networks)
	}
	if _, ok := web.Networks["default"]; !ok {
		t.Errorf("proxied web lost default network: %v", web.Networks)
	}
	if web.Labels["yoho.version"] != "v1" || web.Environment["YOHO_VERSION"] != "v1" || web.Labels["yoho.secrets"] == "" {
		t.Errorf("web version labels/env: %v %v", web.Labels, web.Environment)
	}
	if web.Environment["PRICE"] != "$$5" {
		t.Errorf("literal dollar must be escaped for compose, got %q", web.Environment["PRICE"])
	}
	for _, k := range []string{"YOHO_APP", "YOHO_DESTINATION", "YOHO_SERVER", "YOHO_SERVICE"} {
		if web.Environment[k] == "" {
			t.Errorf("web missing %s", k)
		}
	}

	db := doc.Services["db"]
	if db.Environment["POSTGRES_PASSWORD_FILE"] != "/run/secrets/POSTGRES_PASSWORD" {
		t.Errorf("db should receive its generated secret: %v", db.Environment)
	}
	if _, ok := db.Labels["yoho.version"]; ok {
		t.Error("third-party image must not get a per-version label (would recreate it every deploy)")
	}
	if _, ok := db.Environment["YOHO_VERSION"]; ok {
		t.Error("third-party image must not get YOHO_VERSION")
	}
	if db.Labels["yoho.app"] != "shop" || db.Labels["yoho.service"] != "db" {
		t.Errorf("db labels %v", db.Labels)
	}

	cf := doc.Services["cloudflared"]
	if len(cf.EnvFile) != 1 || cf.EnvFile[0].Path != "/srv/yoho/apps/shop/production/secrets/G1/cloudflared.env" || len(cf.Secrets) != 0 {
		t.Errorf("cloudflared env_file delivery: %+v", cf)
	}
	if n := doc.Networks["yoho"]; !n.External {
		t.Errorf("yoho network must stay external: %+v", doc.Networks)
	}
	if doc.Volumes["pgdata"].Name != "yoho-shop-production_pgdata" || doc.Networks["default"].Name != "yoho-shop-production_default" {
		t.Errorf("volumes/networks must be named per Destination: %+v %+v", doc.Volumes, doc.Networks)
	}

	byName := map[string]servicePlan{}
	for _, p := range plans {
		byName[p.Name] = p
	}
	wp := byName["web"].Proxy
	if wp == nil || wp.Port != 3000 || wp.HealthPath != "/up" || wp.DeployTimeout != 30 || wp.DrainTimeout != 30 {
		t.Errorf("web proxy plan %+v", wp)
	}
	if !byName["cloudflared"].ProxyNetwork || byName["db"].ProxyNetwork || !byName["db"].Stateful {
		t.Errorf("plans %+v", plans)
	}
	if got := byName["web"].DependsOn; len(got) != 1 || got[0] != "db" {
		t.Errorf("web depends_on %v", got)
	}
	// The caller's project must be untouched.
	if _, ok := d.Project.Services["web"].Labels["yoho.app"]; ok {
		t.Error("compile mutated d.Project")
	}
}

func TestCompileFastStartHealth(t *testing.T) {
	d := testDeploy(t, nil, nil)
	src := strings.Replace(testCompose, "  db:\n    image: postgres:17\n", "  db:\n    image: postgres:17\n    healthcheck:\n      test: [\"CMD\", \"pg_isready\"]\n", 1)
	d.Project = loadProject(t, src)
	gen := map[string]string{"POSTGRES_PASSWORD": "generated-PG-password"}
	svc, err := serviceSecrets(d, gen, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	key := []byte("0123456789abcdef0123456789abcdef")
	out, _, err := compile(d, "primary", "/srv/yoho/apps/shop/production/secrets/G1", svc, key, "25.0.3")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	web := doc["services"].(map[string]any)["web"].(map[string]any)["healthcheck"].(map[string]any)
	if web["start_period"] != "60s" || web["start_interval"] != "1s" {
		t.Errorf("proxied healthcheck = %v", web)
	}
	db := doc["services"].(map[string]any)["db"].(map[string]any)["healthcheck"].(map[string]any)
	if _, ok := db["start_interval"]; ok || db["start_period"] != nil {
		t.Errorf("non-proxied compose healthcheck must be unchanged: %v", db)
	}
	if d.Project.Services["web"].HealthCheck.StartPeriod != nil {
		t.Error("compile mutated the caller's healthcheck")
	}

	// User-set start_period is kept, and the missing start_interval is not filled in.
	src = strings.Replace(src, "http://localhost:3000/up\"]\n", "http://localhost:3000/up\"]\n      start_period: 10s\n", 1)
	d.Project = loadProject(t, src)
	out, _, err = compile(d, "primary", "/srv/yoho/apps/shop/production/secrets/G1", svc, key, "29.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	web = doc["services"].(map[string]any)["web"].(map[string]any)["healthcheck"].(map[string]any)
	if web["start_period"] != "10s" {
		t.Errorf("user start_period = %v", web["start_period"])
	}
	if _, ok := web["start_interval"]; ok {
		t.Errorf("user start_period must block the default start_interval: %v", web)
	}

	d.Project = loadProject(t, testCompose)
	out, _, err = compile(d, "primary", "/srv/yoho/apps/shop/production/secrets/G1", svc, key, "24.0.9")
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	web = doc["services"].(map[string]any)["web"].(map[string]any)["healthcheck"].(map[string]any)
	if _, ok := web["start_interval"]; ok || web["start_period"] != nil {
		t.Errorf("Docker 24 must not get start_interval: %v", web)
	}
}

func TestCompileAddsExternalNetwork(t *testing.T) {
	d := testDeploy(t, nil, nil)
	d.Project = loadProject(t, "services:\n  web:\n    image: shop-web:v1\n")
	d.Ext = map[string]config.ServiceExt{"web": {Proxy: &config.ServiceProxy{}}}
	d.ServiceSecrets = nil
	out, _, _ := compileTest(t, d)
	var doc compiledDoc
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if n := doc.Networks["yoho"]; !n.External || n.Name != "yoho" {
		t.Errorf("networks %+v\n%s", doc.Networks, out)
	}
}

func TestValidate(t *testing.T) {
	cases := map[string]struct {
		compose string
		ext     config.ServiceExt
		servers int
		want    string
	}{
		"stateful proxy": {"services:\n  web:\n    image: x\n", config.ServiceExt{Stateful: true, Proxy: &config.ServiceProxy{}}, 1, "stateful"},
		"ports":          {"services:\n  web:\n    image: x\n    ports: ['80:80']\n", config.ServiceExt{Proxy: &config.ServiceProxy{}}, 1, "publish ports"},
		"container_name": {"services:\n  web:\n    image: x\n    container_name: web\n", config.ServiceExt{Proxy: &config.ServiceProxy{}}, 1, "container_name"},
		"tls no hosts":   {"services:\n  web:\n    image: x\n", config.ServiceExt{Proxy: &config.ServiceProxy{TLS: true}}, 1, "tls requires hosts"},
		"two servers":    {"services:\n  web:\n    image: x\n", config.ServiceExt{}, 2, "exactly one Server"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			d := testDeploy(t, &dockerHost{}, nil)
			d.Project = loadProject(t, c.compose)
			d.Ext = map[string]config.ServiceExt{"web": c.ext}
			if c.servers == 2 {
				d.Servers = append(d.Servers, d.Servers[0])
			}
			err := validate(d)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want %q", err, c.want)
			}
		})
	}
	if err := validate(testDeploy(t, &dockerHost{}, nil)); err != nil {
		t.Errorf("example deploy should validate: %v", err)
	}
}
