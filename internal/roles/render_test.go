package roles

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/yoho-build/yoho/internal/remote"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/plan"
)

func fixture() (*plan.Deploy, config.Destination, map[string]string) {
	dest := config.Destination{Servers: []string{"a", "b"}, Roles: map[string]config.Role{"app": {Services: []string{"web", "worker"}, Servers: []string{"a"}}, "data": {Services: []string{"db"}, Servers: []string{"b"}}}, Connections: map[string]config.Connection{"database": {Service: "db", Port: 5432, Consumers: []string{"web", "worker"}}}}
	d := &plan.Deploy{App: "shop", Destination: "production", Servers: []plan.NamedHost{{Name: "a"}, {Name: "b"}}, Project: &types.Project{Services: types.Services{"web": {Name: "web", WorkloadSpec: types.WorkloadSpec{DependsOn: types.DependsOnConfig{"db": {Condition: types.ServiceConditionHealthy}, "worker": {}}}}, "worker": {Name: "worker"}, "db": {Name: "db"}}}, Ext: map[string]config.ServiceExt{}, ServiceSecrets: map[string]map[string]string{"db": {"PASSWORD": "secret"}}}
	return d, dest, map[string]string{"a": "10.0.0.1", "b": "10.0.0.2"}
}
func TestRolesRender(t *testing.T) {
	d, dest, addresses := fixture()
	a, err := Render(d, dest, "a", addresses)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Project.Services) != 2 || a.Project.Services["db"].Name != "" {
		t.Fatal(a.Project.Services)
	}
	web := a.Project.Services["web"]
	if !reflect.DeepEqual(web.ExtraHosts, types.HostsList{"db": {"10.0.0.2"}, "db.shop.yoho.internal": {"10.0.0.2"}}) {
		t.Fatal(web.ExtraHosts)
	}
	if *web.Environment["YOHO_CONNECTION_DATABASE_HOST"] != "10.0.0.2" || *web.Environment["YOHO_CONNECTION_DATABASE_PORT"] != "5432" {
		t.Fatal(web.Environment)
	}
	if _, ok := web.DependsOn["db"]; ok {
		t.Fatal("cross-Server depends_on retained")
	}
	if _, ok := web.DependsOn["worker"]; !ok {
		t.Fatal("local dependency removed")
	}
	b, err := Render(d, dest, "b", addresses)
	if err != nil {
		t.Fatal(err)
	}
	port := b.Project.Services["db"].Ports[0]
	if port.HostIP != "10.0.0.2" || port.Target != 5432 || port.Published != "5432" {
		t.Fatal(port)
	}
	if len(a.ServiceSecrets["db"]) > 0 {
		t.Fatal("unplaced secret leaked")
	}
	if len(d.Project.Services["web"].DependsOn) != 2 || len(d.Project.Services["db"].Ports) > 0 {
		t.Fatal("input mutated")
	}
	addresses["b"] = "10.0.0.3"
	updated, _ := Render(d, dest, "a", addresses)
	if updated.Project.Services["web"].ExtraHosts["db"][0] != "10.0.0.3" {
		t.Fatal("stale address")
	}
}
func TestRolesSameServerAndLegacy(t *testing.T) {
	d, dest, addresses := fixture()
	dest.Roles["data"] = config.Role{Services: []string{"db"}, Servers: []string{"a"}}
	p, e := Render(d, dest, "a", addresses)
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Project.Services["web"].ExtraHosts) != 0 || *p.Project.Services["web"].Environment["YOHO_CONNECTION_DATABASE_HOST"] != "db" {
		t.Fatal("local consumer changed")
	}
	legacy, e := Render(d, config.Destination{}, "a", nil)
	if e != nil || legacy != d {
		t.Fatal("legacy rendering changed")
	}
}
func TestRolesOrder(t *testing.T) {
	_, dest, _ := fixture()
	order, err := Order(dest)
	if err != nil || !reflect.DeepEqual(order, []string{"b", "a"}) {
		t.Fatal(order, err)
	}
	dest.Connections["reverse"] = config.Connection{Service: "web", Consumers: []string{"db"}, Port: 80}
	if _, err := Order(dest); err == nil {
		t.Fatal("cycle accepted")
	}
}
func TestRolesRenderGolden(t *testing.T) {
	d, dest, addresses := fixture()
	for _, server := range []string{"a", "b"} {
		p, e := Render(d, dest, server, addresses)
		if e != nil {
			t.Fatal(e)
		}
		got, e := p.Project.MarshalYAML()
		if e != nil {
			t.Fatal(e)
		}
		file := filepath.Join("testdata", "roles-"+server+".yaml.golden")
		if os.Getenv("UPDATE_GOLDEN") == "1" {
			if e = os.MkdirAll(filepath.Dir(file), 0755); e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(file, got, 0644); e != nil {
				t.Fatal(e)
			}
		}
		want, e := os.ReadFile(file)
		if e != nil {
			t.Fatal(e)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("Server %s Compose differs:\n%s", server, got)
		}
	}
}

// Multi-connector tunnels terminate TLS before traffic reaches each Proxy.
func TestRolesTunnelPreservesTLS(t *testing.T) {
	d, dest, addresses := fixture()
	dest.Roles["app"] = config.Role{Services: []string{"web", "worker"}, Servers: []string{"a", "b"}}
	d.Proxy.Tunnel = &config.TunnelConfig{TokenSecret: "TUNNEL"}
	d.Ext["web"] = config.ServiceExt{Proxy: &config.ServiceProxy{TLS: true}}
	p, e := Render(d, dest, "a", addresses)
	if e != nil {
		t.Fatal(e)
	}
	if !p.Ext["web"].Proxy.TLS {
		t.Fatal("TLS silently changed")
	}
	if !d.Ext["web"].Proxy.TLS {
		t.Fatal("input mutated")
	}
}

type policyHost struct {
	remote.Host
	data []byte
}

func (h policyHost) ReadFile(context.Context, string, bool) ([]byte, error) { return h.data, nil }
func TestRolesExpandProtectionKeepsOldConsumersUntilCutover(t *testing.T) {
	_, dest, addresses := fixture()
	old, e := FirewallRules("shop", "production", "b", dest, addresses)
	if e != nil {
		t.Fatal(e)
	}
	data, _ := json.Marshal(old)
	addresses["a"] = "10.0.0.3"
	desired, e := FirewallRules("shop", "production", "b", dest, addresses)
	if e != nil {
		t.Fatal(e)
	}
	expanded, e := ExpandProtection(context.Background(), policyHost{data: data}, "shop", "production", desired)
	if e != nil {
		t.Fatal(e)
	}
	if len(expanded) != 3 {
		t.Fatal(expanded)
	}
	drop := false
	foundOld := false
	for _, r := range expanded {
		if r.Args[len(r.Args)-1] == "DROP" {
			drop = true
		} else if drop {
			t.Fatal("old consumer ACCEPT placed after DROP")
		}
		for _, a := range r.Args {
			foundOld = foundOld || a == "10.0.0.1"
		}
	}
	if !foundOld {
		t.Fatal("old consumer lost protection during rollout")
	}
}

func TestRolesFiltersResourcesAndSortsConnections(t *testing.T) {
	d, dest, addresses := fixture()
	svc := d.Project.Services["db"]
	svc.Volumes = []types.ServiceVolumeConfig{{Type: types.VolumeTypeVolume, Source: "data", Target: "/data"}}
	svc.Secrets = []types.ServiceSecretConfig{{Source: "db-only"}}
	d.Project.Services["db"] = svc
	d.Project.Volumes = types.Volumes{"data": {}}
	d.Project.Secrets = types.Secrets{"db-only": {File: "secret.txt"}}
	dest.Connections["metrics"] = config.Connection{Service: "db", Port: 9187, Consumers: []string{"web"}}
	p, e := Render(d, dest, "a", addresses)
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Project.Volumes) != 0 || len(p.Project.Secrets) != 0 {
		t.Fatal("resources leaked to consumer Server")
	}
	b, e := Render(d, dest, "b", addresses)
	if e != nil {
		t.Fatal(e)
	}
	if len(b.Project.Volumes) != 1 || len(b.Project.Secrets) != 1 {
		t.Fatal("provider resources dropped")
	}
	want, _ := b.Project.MarshalYAML()
	for i := 0; i < 20; i++ {
		b, e := Render(d, dest, "b", addresses)
		if e != nil {
			t.Fatal(e)
		}
		got, _ := b.Project.MarshalYAML()
		if !bytes.Equal(got, want) {
			t.Fatal("connection map changed rendered bytes")
		}
	}
}

func TestReviewLoopbackPublicationsAndColocatedEndpoint(t *testing.T) {
	d, dest, addresses := fixture()
	s := d.Project.Services["db"]
	s.Ports = []types.ServicePortConfig{{HostIP: "127.0.0.1", Published: "15432", Target: 5432, Protocol: "tcp"}}
	d.Project.Services["db"] = s
	p, e := Render(d, dest, "b", addresses)
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Project.Services["db"].Ports) != 2 {
		t.Fatal("explicit loopback publication dropped")
	}
	dest.Roles["data"] = config.Role{Services: []string{"db"}, Servers: []string{"a"}}
	p, e = Render(d, dest, "a", addresses)
	if e != nil {
		t.Fatal(e)
	}
	web := p.Project.Services["web"]
	if *web.Environment["YOHO_CONNECTION_DATABASE_HOST"] != "db" || len(web.ExtraHosts) > 0 {
		t.Fatal("colocated endpoint lost Compose DNS", web)
	}
	if !slices.Contains(p.Project.Services["db"].Networks["default"].Aliases, "db.shop.yoho.internal") {
		t.Fatal("qualified endpoint lost on colocation")
	}
}

func TestReviewUnprotectedAlternatePublicationRefused(t *testing.T) {
	d, dest, addresses := fixture()
	s := d.Project.Services["db"]
	s.Ports = []types.ServicePortConfig{{HostIP: "10.0.0.2", Published: "15432", Target: 5432, Protocol: "tcp"}}
	d.Project.Services["db"] = s
	if _, e := Render(d, dest, "b", addresses); e == nil {
		t.Fatal("unprotected alternate publication preserved")
	}
}
