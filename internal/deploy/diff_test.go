package deploy

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/yoho-build/yoho/internal/remote"

	"github.com/yoho-build/yoho/internal/plan"
)

// fakeState answers the read-only docker queries Diff makes.
type fakeState struct {
	containers string // docker inspect JSON
	routes     string // kamal-proxy list --json
	proxyState string // "<label>|<running>" or ""
}

func (f *fakeState) respond(s string) (string, error) {
	switch {
	case strings.Contains(s, "kamal-proxy list --json"):
		return f.routes, nil
	case strings.Contains(s, "docker container inspect"):
		return f.proxyState, nil
	case strings.Contains(s, "docker inspect $ids"):
		return f.containers, nil
	}
	return "", nil
}

// stateFromRelease builds `docker inspect` output for one running container
// per Service of the current Release's compose file.
func stateFromRelease(t *testing.T, root string, extra ...map[string]any) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "apps", "shop", "production", "current", "compose.yaml"))
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
	all = append(all, extra...)
	b, _ := json.Marshal(all)
	return string(b)
}

const goodRoutes = `{"shop-production-web":{"hosts":["shop.example.com"],"targets":["yoho-shop-production-web-1:3000"],"state":"running"}}`

// deployed runs a real Deploy against the temp root and returns the host and
// a state that matches it.
func deployed(t *testing.T) (*dockerHost, *fakeState, string) {
	t.Helper()
	root := withRoot(t)
	h := &dockerHost{local: &remote.Local{}, respond: respond(nil)}
	if _, err := (Compose{}).Deploy(context.Background(), testDeploy(t, h, io.Discard)); err != nil {
		t.Fatalf("deploy: %v\n%s", err, h.all())
	}
	m := regexp.MustCompile(`yoho\.proxy\.config=([0-9a-f]+)`).FindStringSubmatch(h.all())
	if m == nil {
		t.Fatal("proxy was not booted")
	}
	st := &fakeState{containers: stateFromRelease(t, root), routes: goodRoutes, proxyState: m[1] + "|true"}
	h.respond = st.respond
	return h, st, root
}

func byKey(cs []plan.Change) map[string]plan.Change {
	m := map[string]plan.Change{}
	for _, c := range cs {
		m[c.Kind+" "+c.Name] = c
	}
	return m
}

func TestDiffFirstDeployCreatesEverything(t *testing.T) {
	withRoot(t)
	h := &dockerHost{local: &remote.Local{}, respond: (&fakeState{}).respond}
	cs, err := Compose{}.Diff(context.Background(), testDeploy(t, h, io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	m := byKey(cs)
	for _, k := range []string{"service web", "service db", "service cloudflared", "route shop-production-web", "proxy yoho-proxy", "secret POSTGRES_PASSWORD"} {
		if m[k].Action != plan.ActionCreate {
			t.Errorf("%s: %+v", k, m[k])
		}
	}
	if m["service web"].Reasons[0] != "image shop-web:v1" {
		t.Errorf("reasons %v", m["service web"].Reasons)
	}
	if strings.Contains(h.all(), "docker run") || strings.Contains(h.all(), "docker rm") {
		t.Error("Diff must be read-only")
	}
}

func TestDiffNoChangesAfterDeploy(t *testing.T) {
	h, _, _ := deployed(t)
	cs, err := Compose{}.Diff(context.Background(), testDeploy(t, h, io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		if c.Action != plan.ActionNoop {
			t.Errorf("unexpected change %+v", c)
		}
	}
}

func TestDiffImageChange(t *testing.T) {
	h, _, _ := deployed(t)
	d := testDeploy(t, h, io.Discard)
	web := d.Project.Services["web"]
	web.Image = "shop-web:v2"
	d.Project.Services["web"] = web
	db := d.Project.Services["db"]
	db.Image = "postgres:18"
	d.Project.Services["db"] = db
	cs, err := Compose{}.Diff(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	m := byKey(cs)
	if c := m["service web"]; c.Action != plan.ActionUpdate || c.Downtime || c.Reasons[0] != "image shop-web:v1 → shop-web:v2" {
		t.Errorf("web %+v", c)
	}
	if c := m["service db"]; c.Action != plan.ActionReplace || !c.Downtime || c.Reasons[0] != "image postgres:17 → postgres:18" {
		t.Errorf("db %+v", c)
	}
	if m["service cloudflared"].Action != plan.ActionNoop {
		t.Errorf("cloudflared %+v", m["service cloudflared"])
	}
}

func TestDiffSecretsAndConfigChange(t *testing.T) {
	h, _, _ := deployed(t)
	d := testDeploy(t, h, io.Discard)
	d.ServiceSecrets["web"]["SECRET_KEY_BASE"] = "rotated-value"
	d.ServiceSecrets["cloudflared"]["TUNNEL_TOKEN"] = tunnelSecret
	cf := d.Project.Services["cloudflared"]
	cf.Restart = "always"
	d.Project.Services["cloudflared"] = cf
	cs, err := Compose{}.Diff(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	m := byKey(cs)
	if c := m["service web"]; c.Action != plan.ActionUpdate || len(c.Reasons) != 1 || c.Reasons[0] != "secrets changed" {
		t.Errorf("web %+v", c)
	}
	if c := m["service cloudflared"]; c.Action != plan.ActionReplace || len(c.Reasons) != 1 || c.Reasons[0] != "config changed: restart" {
		t.Errorf("cloudflared %+v", c)
	}
	if strings.Contains(m["service web"].Reasons[0], "rotated") {
		t.Error("secret value leaked")
	}
}

func TestDiffRemovedServiceAndStaleRoute(t *testing.T) {
	h, st, root := deployed(t)
	st.containers = stateFromRelease(t, root, map[string]any{
		"Name": "/yoho-shop-production-worker-1", "State": map[string]any{"Running": true},
		"Config": map[string]any{"Image": "shop-worker:v1", "Labels": map[string]any{"com.docker.compose.service": "worker"}},
	})
	st.routes = `{"shop-production-web":{"hosts":["shop.example.com"],"targets":["yoho-shop-production-web-1:3000"],"state":"running"},` +
		`"shop-production-old":{"hosts":["*"],"targets":["yoho-shop-production-old-1:80"],"state":"running"},` +
		`"shop-production-other":{"hosts":["*"],"targets":["yoho-shop-production-other-1:80"],"state":"running"},` +
		`"shop-staging-web":{"hosts":["*"],"targets":["yoho-shop-staging-web-1:80"],"state":"running"}}`
	d := testDeploy(t, h, io.Discard)
	cs, err := Compose{}.Diff(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	m := byKey(cs)
	if m["service worker"].Action != plan.ActionDelete {
		t.Errorf("worker %+v", m["service worker"])
	}
	if m["route shop-production-old"].Action != plan.ActionDelete || m["route shop-production-other"].Action != plan.ActionDelete {
		t.Errorf("stale routes: %+v", cs)
	}
	if _, ok := m["route shop-staging-web"]; ok {
		t.Error("another Destination's route must not be touched")
	}
}

func TestDiffRouteHostsChange(t *testing.T) {
	h, st, _ := deployed(t)
	st.routes = `{"shop-production-web":{"hosts":["old.example.com"],"targets":["yoho-shop-production-web-1:3000"],"state":"running"}}`
	cs, err := Compose{}.Diff(context.Background(), testDeploy(t, h, io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	c := byKey(cs)["route shop-production-web"]
	if c.Action != plan.ActionUpdate || c.Reasons[0] != "hosts old.example.com → shop.example.com" {
		t.Errorf("%+v", c)
	}
}
