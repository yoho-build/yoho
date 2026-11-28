package deploy

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/plan"
)

// Each provider Server of an instance Connection plans its own publication.
func TestRolesDiffInstanceConnectionFirewall(t *testing.T) {
	withRoot(t)
	dest := config.Destination{Servers: []string{"a", "b", "c"},
		Roles:       map[string]config.Role{"echo": {Services: []string{"echo"}, Servers: []string{"a", "b"}}, "lb": {Services: []string{"lb"}, Servers: []string{"c"}}},
		Connections: map[string]config.Connection{"upstream": {Service: "echo", Port: 8000, Consumers: []string{"lb"}, Instances: true}}}
	var servers []plan.NamedHost
	for _, n := range dest.Servers {
		servers = append(servers, plan.NamedHost{Name: n, Host: &rolesHost{name: n, files: map[string][]byte{}}})
	}
	d := &plan.Deploy{App: "shop", Destination: "production", Version: "v1", Roles: dest, Servers: servers,
		Project:          &types.Project{Services: types.Services{"echo": {Name: "echo"}, "lb": {Name: "lb"}}},
		Ext:              map[string]config.ServiceExt{},
		PrivateAddresses: map[string]string{"a": "10.0.0.1", "b": "10.0.0.2", "c": "10.0.0.3"}}
	changes, err := (Roles{}).Diff(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[string][]string{}
	for _, c := range changes {
		if c.Kind == "firewall" {
			reasons[c.Server] = c.Reasons
		}
	}
	for server, want := range map[string]string{
		"a": "connection upstream: publish 10.0.0.1:8000:8000/tcp (instance a.m.echo)",
		"b": "connection upstream: publish 10.0.0.2:8000:8000/tcp (instance b.m.echo)",
	} {
		if !slices.Contains(reasons[server], want) {
			t.Fatalf("%s firewall reasons %v, want %q", server, reasons[server], want)
		}
	}
	for _, r := range reasons["c"] {
		if strings.Contains(r, "publish") {
			t.Fatalf("consumer-only Server publishes: %v", reasons["c"])
		}
	}
}
