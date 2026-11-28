package roles

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/plan"
)

// echo runs on a and b; worker shares b with one instance, lb on c is remote
// to both.
func instanceFixture() (*plan.Deploy, config.Destination, map[string]string) {
	dest := config.Destination{
		Servers: []string{"a", "b", "c"},
		Roles: map[string]config.Role{
			"echo":   {Services: []string{"echo"}, Servers: []string{"a", "b"}},
			"worker": {Services: []string{"worker"}, Servers: []string{"b"}},
			"lb":     {Services: []string{"lb"}, Servers: []string{"c"}},
		},
		Connections: map[string]config.Connection{"upstream": {Service: "echo", Port: 8000, Consumers: []string{"lb", "worker"}, Instances: true}},
	}
	d := &plan.Deploy{App: "shop", Destination: "production", Servers: []plan.NamedHost{{Name: "a"}, {Name: "b"}, {Name: "c"}},
		Project: &types.Project{Services: types.Services{"echo": {Name: "echo"}, "worker": {Name: "worker"}, "lb": {Name: "lb"}}},
		Ext:     map[string]config.ServiceExt{}, ServiceSecrets: map[string]map[string]string{}}
	return d, dest, map[string]string{"a": "10.0.0.1", "b": "10.0.0.2", "c": "10.0.0.3"}
}

func TestInstanceConnectionRender(t *testing.T) {
	d, dest, addresses := instanceFixture()
	parts := map[string]*plan.Deploy{}
	for _, server := range dest.Servers {
		p, err := Render(d, dest, server, addresses)
		if err != nil {
			t.Fatal(server, err)
		}
		parts[server] = p
	}
	// Each provider Server publishes its own address.
	for server, addr := range map[string]string{"a": "10.0.0.1", "b": "10.0.0.2"} {
		ports := parts[server].Project.Services["echo"].Ports
		if len(ports) != 1 || ports[0].HostIP != addr || ports[0].Published != "8000" {
			t.Fatalf("%s echo ports: %+v", server, ports)
		}
	}
	// Remote consumer: both instances by extra_hosts, no bare name or HOST.
	lb := parts["c"].Project.Services["lb"]
	want := types.HostsList{
		"a.m.echo": {"10.0.0.1"}, "a.m.echo.shop.yoho.internal": {"10.0.0.1"},
		"b.m.echo": {"10.0.0.2"}, "b.m.echo.shop.yoho.internal": {"10.0.0.2"},
	}
	if !reflect.DeepEqual(lb.ExtraHosts, want) {
		t.Fatalf("lb extra_hosts: %v", lb.ExtraHosts)
	}
	if _, ok := lb.Environment["YOHO_CONNECTION_UPSTREAM_HOST"]; ok {
		t.Fatal("HOST set for a multi-Server provider")
	}
	if v := lb.Environment["YOHO_CONNECTION_UPSTREAM_PORT"]; v == nil || *v != "8000" {
		t.Fatal("PORT missing")
	}
	// Colocated consumer: the remote instance by extra_hosts, the local one
	// as a network alias on the provider.
	worker := parts["b"].Project.Services["worker"]
	if !reflect.DeepEqual(worker.ExtraHosts, types.HostsList{"a.m.echo": {"10.0.0.1"}, "a.m.echo.shop.yoho.internal": {"10.0.0.1"}}) {
		t.Fatalf("worker extra_hosts: %v", worker.ExtraHosts)
	}
	aliases := parts["b"].Project.Services["echo"].Networks["default"].Aliases
	if !slices.Equal(aliases, []string{"b.m.echo", "b.m.echo.shop.yoho.internal"}) {
		t.Fatalf("b echo aliases: %v", aliases)
	}
	// No consumer on a: no aliases there, and the bare qualified name is never added.
	if n := parts["a"].Project.Services["echo"].Networks; len(n) != 0 {
		t.Fatalf("a echo networks: %v", n)
	}
	if d.Project.Services["echo"].Networks != nil || d.Project.Services["lb"].ExtraHosts != nil {
		t.Fatal("input mutated")
	}
}

func TestInstanceConnectionSingleProviderKeepsServiceName(t *testing.T) {
	d, dest, addresses := instanceFixture()
	dest.Roles["echo"] = config.Role{Services: []string{"echo"}, Servers: []string{"a"}}
	p, err := Render(d, dest, "c", addresses)
	if err != nil {
		t.Fatal(err)
	}
	lb := p.Project.Services["lb"]
	for _, name := range []string{"echo", "echo.shop.yoho.internal", "a.m.echo", "a.m.echo.shop.yoho.internal"} {
		if got := lb.ExtraHosts[name]; len(got) != 1 || got[0] != "10.0.0.1" {
			t.Fatalf("%s: %v", name, lb.ExtraHosts)
		}
	}
	if v := lb.Environment["YOHO_CONNECTION_UPSTREAM_HOST"]; v == nil || *v != "10.0.0.1" {
		t.Fatal("HOST missing for one provider Server")
	}
}

func TestInstanceConnectionOrderFirewallAndRefusal(t *testing.T) {
	_, dest, addresses := instanceFixture()
	order, err := Order(dest)
	if err != nil || !slices.Equal(order, []string{"a", "b", "c"}) {
		t.Fatal(order, err)
	}
	for _, server := range []string{"a", "b"} {
		rules, err := FirewallRules("shop", "production", server, dest, addresses)
		if err != nil {
			t.Fatal(err)
		}
		var sources []string
		for _, r := range rules {
			if r.Args[0] == "-s" {
				sources = append(sources, r.Args[1])
			}
			if !slices.Contains(r.Args, addresses[server]) {
				t.Fatalf("%s rule for another address: %v", server, r.Args)
			}
		}
		if !slices.Equal(sources, []string{"10.0.0.2", "10.0.0.3"}) || rules[len(rules)-1].Args[len(rules[len(rules)-1].Args)-1] != "DROP" {
			t.Fatalf("%s rules: %+v", server, rules)
		}
	}
	if rules, _ := FirewallRules("shop", "production", "c", dest, addresses); len(rules) != 0 {
		t.Fatalf("consumer-only Server publishes: %v", rules)
	}
	// A consumer on every provider Server forms a cycle.
	dest.Roles["worker"] = config.Role{Services: []string{"worker"}, Servers: []string{"a", "b"}}
	if _, err := Order(dest); err == nil || !strings.Contains(err.Error(), "instance Connections order Servers provider-first") {
		t.Fatal(err)
	}
	// Without instances: true a multi-Server provider stays refused.
	c := dest.Connections["upstream"]
	c.Instances = false
	dest.Connections["upstream"] = c
	if _, err := FirewallRules("shop", "production", "a", dest, addresses); err == nil {
		t.Fatal("multi-Server provider accepted without instances")
	}
}

func TestInstanceConnectionRenderGolden(t *testing.T) {
	d, dest, addresses := instanceFixture()
	for _, server := range dest.Servers {
		p, e := Render(d, dest, server, addresses)
		if e != nil {
			t.Fatal(e)
		}
		got, e := p.Project.MarshalYAML()
		if e != nil {
			t.Fatal(e)
		}
		file := filepath.Join("testdata", "instances-"+server+".yaml.golden")
		if os.Getenv("UPDATE_GOLDEN") == "1" {
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
