package composefile

import (
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/yoho-build/yoho/internal/config"
)

func messages(fs []Finding) string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Message)
	}
	return strings.Join(out, "\n")
}

func TestCheckTags(t *testing.T) {
	servers := map[string]config.Server{"a": {Tags: []string{"eu", "canary"}}, "b": {}}
	tests := []struct {
		name string
		tags map[string]config.ServiceTag
		dest config.Destination
		want string
	}{
		{"valid", map[string]config.ServiceTag{"eu": {Env: map[string]string{"REGION": "eu"}}, "b": {Env: map[string]string{"NODE": "b"}}}, config.Destination{}, ""},
		{"unknown selector", map[string]config.ServiceTag{"us": {}}, config.Destination{}, "x-yoho.tags.us matches no Server name or tag. Hint: known names and tags: a, b, canary, eu"},
		{"bad key", map[string]config.ServiceTag{"eu": {Env: map[string]string{"1X": "v"}}}, config.Destination{}, `x-yoho.tags.eu.env key "1X" must match`},
		{"reserved key", map[string]config.ServiceTag{"eu": {Env: map[string]string{"YOHO_SERVER": "v"}}}, config.Destination{}, "reserved YOHO_ prefix"},
		{"swarm", map[string]config.ServiceTag{"eu": {}}, config.Destination{Runtime: "swarm"}, "x-yoho.tags supports the compose runtime only; set the environment in the Compose file instead"},
		{"scoped", map[string]config.ServiceTag{"eu": {}}, config.Destination{Access: "scoped"}, "x-yoho.tags requires operator access; the Gate refuses per-Server environment"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Result{Project: &types.Project{Services: types.Services{"web": {Name: "web"}}}, Ext: map[string]config.ServiceExt{"web": {Tags: tt.tags}}}
			got := messages(CheckTags(r, servers, tt.dest))
			if tt.want == "" && got != "" || tt.want != "" && !strings.Contains(got, tt.want) {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestInstanceConnectionChecks(t *testing.T) {
	one := 1
	two := 2
	tests := []struct {
		name string
		edit func(*Result, *config.Destination)
		want string
	}{
		{"valid", func(*Result, *config.Destination) {}, ""},
		{"proxied provider", func(r *Result, _ *config.Destination) {
			r.Ext["echo"] = config.ServiceExt{Proxy: &config.ServiceProxy{Hosts: []string{"echo.example.com"}}}
		}, "instance connection upstream provider cannot be proxied"},
		{"replicas", func(r *Result, _ *config.Destination) {
			s := r.Project.Services["echo"]
			s.Deploy = &types.DeployConfig{Replicas: &two}
			r.Project.Services["echo"] = s
		}, "must run exactly one container per Server"},
		{"one replica", func(r *Result, _ *config.Destination) {
			s := r.Project.Services["echo"]
			s.Deploy = &types.DeployConfig{Replicas: &one}
			r.Project.Services["echo"] = s
		}, ""},
		{"instance extra_hosts collision", func(r *Result, _ *config.Destination) {
			s := r.Project.Services["lb"]
			s.ExtraHosts = types.HostsList{"b.m.echo": {"10.0.0.9"}}
			r.Project.Services["lb"] = s
		}, "name collision in extra_hosts: b.m.echo"},
		{"instance alias collision", func(r *Result, _ *config.Destination) {
			s := r.Project.Services["lb"]
			s.Networks = map[string]*types.ServiceNetworkConfig{"default": {Aliases: []string{"a.m.echo.shop.yoho.internal"}}}
			r.Project.Services["lb"] = s
		}, "name collides with local Service lb network alias"},
		{"cycle", func(_ *Result, d *config.Destination) {
			d.Roles["lb"] = config.Role{Services: []string{"lb"}, Servers: []string{"a", "b"}}
		}, "a consumer on every provider Server forms a cycle"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Result{Project: &types.Project{Name: "shop", Services: types.Services{"echo": {Name: "echo"}, "lb": {Name: "lb"}}}, Ext: map[string]config.ServiceExt{}}
			d := config.Destination{Servers: []string{"a", "b", "c"},
				Roles:       map[string]config.Role{"echo": {Services: []string{"echo"}, Servers: []string{"a", "b"}}, "lb": {Services: []string{"lb"}, Servers: []string{"c"}}},
				Connections: map[string]config.Connection{"upstream": {Service: "echo", Port: 8000, Consumers: []string{"lb"}, Instances: true}}}
			tt.edit(r, &d)
			got := messages(CheckRoles(r, d, config.ProxyConfig{}))
			if tt.want == "" && got != "" || tt.want != "" && !strings.Contains(got, tt.want) {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}
