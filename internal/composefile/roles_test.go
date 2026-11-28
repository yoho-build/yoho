package composefile

import (
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/yoho-build/yoho/internal/config"
)

func TestRolesServiceValidation(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Result, *config.Destination, *config.ProxyConfig)
		want string
	}{
		{"valid", func(*Result, *config.Destination, *config.ProxyConfig) {}, ""},
		{"unassigned", func(r *Result, d *config.Destination, p *config.ProxyConfig) { delete(d.Roles, "app") }, "exactly one Role"},
		{"Stateful replicated", func(r *Result, d *config.Destination, p *config.ProxyConfig) {
			d.Roles["data"] = config.Role{Services: []string{"db"}, Servers: []string{"a", "b"}}
		}, "Stateful Service"},
		{"wildcard", func(r *Result, d *config.Destination, p *config.ProxyConfig) {
			s := r.Project.Services["db"]
			s.Ports = []types.ServicePortConfig{{Target: 5432, Published: "5432"}}
			r.Project.Services["db"] = s
		}, "wildcard"},
		{"host network", func(r *Result, d *config.Destination, p *config.ProxyConfig) {
			s := r.Project.Services["db"]
			s.NetworkMode = "host"
			r.Project.Services["db"] = s
		}, "Compose networking"},
		{"alias collision", func(r *Result, d *config.Destination, p *config.ProxyConfig) {
			s := r.Project.Services["web"]
			s.ExtraHosts = types.HostsList{"db": {"10.0.0.4"}}
			r.Project.Services["web"] = s
		}, "name collision"},
		{"multi ingress TLS refused", func(r *Result, d *config.Destination, p *config.ProxyConfig) {
			d.Roles["app"] = config.Role{Services: []string{"web"}, Servers: []string{"a", "b"}}
			r.Ext["web"] = config.ServiceExt{Proxy: &config.ServiceProxy{TLS: true}}
		}, "multi-Server Proxy TLS"},
		{"multi ingress external TLS", func(r *Result, d *config.Destination, p *config.ProxyConfig) {
			d.Roles["app"] = config.Role{Services: []string{"web"}, Servers: []string{"a", "b"}}
			r.Ext["web"] = config.ServiceExt{Proxy: &config.ServiceProxy{TLS: false, SSL: boolPointer(false)}}
		}, ""},
		{"multi ingress tunnel", func(r *Result, d *config.Destination, p *config.ProxyConfig) {
			d.Roles["app"] = config.Role{Services: []string{"web"}, Servers: []string{"a", "b"}}
			r.Ext["web"] = config.ServiceExt{Proxy: &config.ServiceProxy{TLS: true}}
			p.Tunnel = &config.TunnelConfig{TokenSecret: "TUNNEL"}
		}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Result{Project: &types.Project{Name: "shop", Services: types.Services{"web": {Name: "web"}, "db": {Name: "db"}}}, Ext: map[string]config.ServiceExt{"db": {Stateful: true}}}
			d := config.Destination{Servers: []string{"a", "b"}, Roles: map[string]config.Role{"app": {Services: []string{"web"}, Servers: []string{"a"}}, "data": {Services: []string{"db"}, Servers: []string{"b"}}}, Connections: map[string]config.Connection{"database": {Service: "db", Port: 5432, Consumers: []string{"web"}}}}
			p := config.ProxyConfig{}
			tt.edit(r, &d, &p)
			fs := CheckRoles(r, d, p)
			got := ""
			for _, f := range fs {
				got += f.Message + "\n"
			}
			if tt.want == "" && HasErrors(fs) || tt.want != "" && !strings.Contains(got, tt.want) {
				t.Fatalf("got %q; want %q", got, tt.want)
			}
		})
	}
}

func boolPointer(v bool) *bool { return &v }

// Connection ports are keyed by (Server, port): a1:234 and a:1234 differ.
func TestRolesConnectionPortKeyIsNotAmbiguous(t *testing.T) {
	r := &Result{Project: &types.Project{Name: "shop", Services: types.Services{"web": {Name: "web"}, "db": {Name: "db"}, "cache": {Name: "cache"}}}, Ext: map[string]config.ServiceExt{"db": {Stateful: true}}}
	d := config.Destination{Servers: []string{"a", "a1"}, Roles: map[string]config.Role{
		"app":   {Services: []string{"web"}, Servers: []string{"a"}},
		"data":  {Services: []string{"db"}, Servers: []string{"a1"}},
		"cache": {Services: []string{"cache"}, Servers: []string{"a"}},
	}, Connections: map[string]config.Connection{
		"database": {Service: "db", Port: 234, Consumers: []string{"web"}},
		"kv":       {Service: "cache", Port: 1234, Consumers: []string{"web"}},
	}}
	for _, f := range CheckRoles(r, d, config.ProxyConfig{}) {
		if strings.Contains(f.Message, "same port") {
			t.Fatalf("a1:234 and a:1234 reported as the same port: %s", f.Message)
		}
	}
	d.Connections["kv"] = config.Connection{Service: "cache", Port: 234, Consumers: []string{"web"}}
	d.Roles["cache"] = config.Role{Services: []string{"cache"}, Servers: []string{"a1"}}
	found := false
	for _, f := range CheckRoles(r, d, config.ProxyConfig{}) {
		found = found || strings.Contains(f.Message, "same port")
	}
	if !found {
		t.Fatal("same Server and port not reported")
	}
}
