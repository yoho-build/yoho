package composefile

import (
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/yoho-build/yoho/internal/config"
	"strings"
	"testing"
)

func TestReviewRolesDependenciesIngressAndPersistentData(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Result, *config.Destination, *config.ProxyConfig)
		want string
	}{
		{"missing connection", func(r *Result, d *config.Destination, p *config.ProxyConfig) { d.Connections = nil }, "cross-Server depends_on"},
		{"consumer omitted", func(r *Result, d *config.Destination, p *config.ProxyConfig) {
			c := d.Connections["db"]
			c.Consumers = []string{"db"}
			d.Connections["db"] = c
		}, "cross-Server depends_on"},
		{"completion cross Server", func(r *Result, d *config.Destination, p *config.ProxyConfig) {
			s := r.Project.Services["web"]
			s.DependsOn["db"] = types.ServiceDependency{Condition: types.ServiceConditionCompletedSuccessfully}
			r.Project.Services["web"] = s
		}, "completion jobs must be colocated"},
		{"consumer network namespace", func(r *Result, d *config.Destination, p *config.ProxyConfig) {
			s := r.Project.Services["web"]
			s.NetworkMode = "service:other"
			r.Project.Services["web"] = s
		}, "consumer must use Compose networking"},
		{"unmarked volume", func(r *Result, d *config.Destination, p *config.ProxyConfig) { r.Ext["db"] = config.ServiceExt{} }, "persistent data requires"},
		{"implicit TLS choice", func(r *Result, d *config.Destination, p *config.ProxyConfig) {
			d.Roles["app"] = config.Role{Services: []string{"web"}, Servers: []string{"a", "b"}}
			r.Ext["web"] = config.ServiceExt{Proxy: &config.ServiceProxy{TLS: false}}
		}, "explicit x-yoho.proxy.ssl: false"},
		{"Quick Tunnel even SSL off", func(r *Result, d *config.Destination, p *config.ProxyConfig) {
			d.Roles["app"] = config.Role{Services: []string{"web"}, Servers: []string{"a", "b"}}
			r.Ext["web"] = config.ServiceExt{Proxy: &config.ServiceProxy{SSL: boolPointer(false)}}
			p.Tunnel = &config.TunnelConfig{}
		}, "Quick Tunnels are unsupported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &Result{Project: &types.Project{Services: types.Services{"web": {Name: "web", WorkloadSpec: types.WorkloadSpec{DependsOn: types.DependsOnConfig{"db": {Condition: types.ServiceConditionStarted}}}}, "db": {Name: "db", ContainerSpec: types.ContainerSpec{Volumes: []types.ServiceVolumeConfig{{Type: types.VolumeTypeVolume, Source: "data", Target: "/data"}}}}}}, Ext: map[string]config.ServiceExt{"db": {Stateful: true}}}
			d := config.Destination{Servers: []string{"a", "b"}, Roles: map[string]config.Role{"app": {Services: []string{"web"}, Servers: []string{"a"}}, "data": {Services: []string{"db"}, Servers: []string{"b"}}}, Connections: map[string]config.Connection{"db": {Service: "db", Port: 5432, Consumers: []string{"web"}}}}
			p := config.ProxyConfig{}
			tc.edit(r, &d, &p)
			var messages string
			for _, f := range CheckRoles(r, d, p) {
				messages += f.Message
			}
			if !strings.Contains(messages, tc.want) {
				t.Fatal(messages)
			}
		})
	}
}
