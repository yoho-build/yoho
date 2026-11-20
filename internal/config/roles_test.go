package config

import (
	"strings"
	"testing"
)

func TestRolesConfigValidation(t *testing.T) {
	base := func() *Config {
		return &Config{App: "shop", Servers: map[string]Server{"a": {SSH: "a", PrivateAddress: "10.0.0.1"}, "b": {SSH: "b", PrivateAddress: "10.0.0.2"}}, Destinations: map[string]Destination{"production": {Servers: []string{"a", "b"}, Roles: map[string]Role{"app": {Services: []string{"web"}, Servers: []string{"a"}}, "data": {Services: []string{"db"}, Servers: []string{"b"}}}, Connections: map[string]Connection{"db": {Service: "db", Port: 5432, Consumers: []string{"web"}}}}}}
	}
	tests := []struct {
		name   string
		change func(*Config)
		want   string
	}{
		{"valid", func(*Config) {}, ""},
		{"duplicate Service", func(c *Config) {
			d := c.Destinations["production"]
			d.Roles["extra"] = Role{Services: []string{"web"}, Servers: []string{"b"}}
		}, "exactly one Role"},
		{"foreign Server", func(c *Config) {
			d := c.Destinations["production"]
			d.Roles["extra"] = Role{Services: []string{"worker"}, Servers: []string{"other"}}
		}, "not in Destination"},
		{"missing address", func(c *Config) { s := c.Servers["a"]; s.PrivateAddress = ""; c.Servers["a"] = s }, "private_address"},
		{"wildcard address", func(c *Config) { s := c.Servers["b"]; s.PrivateAddress = "::"; c.Servers["b"] = s }, "private_address"},
		{"nonsingleton", func(c *Config) {
			d := c.Destinations["production"]
			d.Roles["data"] = Role{Services: []string{"db"}, Servers: []string{"a", "b"}}
		}, "exactly one Server"},
		{"unknown consumer", func(c *Config) {
			d := c.Destinations["production"]
			d.Connections["db"] = Connection{Service: "db", Port: 5432, Consumers: []string{"missing"}}
		}, "unknown consumer"},
		{"Swarm", func(c *Config) {
			d := c.Destinations["production"]
			d.Runtime = "swarm"
			c.Destinations["production"] = d
		}, "operator access and Compose"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base()
			tt.change(c)
			errs := validateRoles(c, "production", c.Destinations["production"])
			var messages []string
			for _, e := range errs {
				messages = append(messages, e.Error())
			}
			got := strings.Join(messages, "\n")
			if tt.want == "" && len(errs) > 0 || tt.want != "" && !strings.Contains(got, tt.want) {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func TestReviewRcloneIndependentRoleOwnersWarn(t *testing.T) {
	c := &Config{Destinations: map[string]Destination{"production": {Roles: map[string]Role{"one": {Services: []string{"db"}, Servers: []string{"a"}}, "two": {Services: []string{"files"}, Servers: []string{"b"}}}}}, Backups: BackupsConfig{Targets: map[string]BackupTarget{"same": {RcloneConfigSecret: "OAUTH"}}, Jobs: map[string]BackupJob{"db": {Destination: "production", Target: "same", Services: []string{"db"}}, "files": {Destination: "production", Target: "same", Services: []string{"files"}}}}}
	if got := strings.Join(RcloneWarnings(c), "\n"); !strings.Contains(got, "independent Roles Servers a, b") {
		t.Fatal(got)
	}
}
