package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func migrateCfg(m *Migrate, mutate func(*Config)) *Config {
	c := &Config{
		App:     "shop",
		Servers: map[string]Server{"a": {SSH: "u@h"}},
		Destinations: map[string]Destination{
			"production": {Servers: []string{"a"}, Migrate: m},
			"rehearsal":  {Servers: []string{"a"}},
		},
		Proxy: ProxyConfig{Tunnel: &TunnelConfig{TokenSecret: "CF_TUNNEL_TOKEN"}},
	}
	if mutate != nil {
		mutate(c)
	}
	return c
}

func validMigrate() *Migrate {
	return &Migrate{
		From:   "coolify",
		Stop:   MigrateStop{ComposeProject: "ab12cd", Labels: []string{"coolify.applicationId=ab12"}, Containers: []string{"old-web"}},
		Claim:  MigrateClaim{Paths: []string{"/data/app/postgres"}, Volumes: []string{"ab12_pgdata"}},
		Tunnel: &MigrateTunnel{APITokenSecret: "CF_API_TOKEN", Hostnames: []string{"app.example.com", "*.example.com"}},
	}
}

func TestValidateMigrate(t *testing.T) {
	if errs := Validate(migrateCfg(validMigrate(), nil)); len(errs) != 0 {
		t.Fatalf("valid config: %v", errs)
	}
	cases := []struct {
		name   string
		m      func(*Migrate)
		c      func(*Config)
		expect string
	}{
		{"from enum", func(m *Migrate) { m.From = "heroku" }, nil, "migrate.from"},
		{"on_failure enum", func(m *Migrate) { m.OnFailure = "retry" }, nil, "migrate.on_failure"},
		{"empty stop", func(m *Migrate) { m.Stop = MigrateStop{} }, nil, "set compose_project, labels or containers"},
		{"yoho project", func(m *Migrate) { m.Stop.ComposeProject = "yoho-shop-production" }, nil, "is a Yoho project"},
		{"yoho container", func(m *Migrate) { m.Stop.Containers = []string{"yoho-proxy"} }, nil, "is a Yoho container"},
		{"yoho label", func(m *Migrate) { m.Stop.Labels = []string{"yoho.app=shop"} }, nil, "selects Yoho containers"},
		{"relative path", func(m *Migrate) { m.Claim.Paths = []string{"data/pg"} }, nil, "absolute path"},
		{"root path", func(m *Migrate) { m.Claim.Paths = []string{"/"} }, nil, "absolute path other than /"},
		{"unclean path", func(m *Migrate) { m.Claim.Paths = []string{"/data//pg/"} }, nil, "clean path"},
		{"volume name", func(m *Migrate) { m.Claim.Volumes = []string{"a b"} }, nil, "not a volume name"},
		{"hostname", func(m *Migrate) { m.Tunnel.Hostnames = []string{"https://app.example.com"} }, nil, "must be a DNS hostname"},
		{"no hostnames", func(m *Migrate) { m.Tunnel.Hostnames = nil }, nil, "list the hostnames"},
		{"service url", func(m *Migrate) { m.Tunnel.Service = "yoho-proxy:80" }, nil, "http:// or https://"},
		{"api token key", func(m *Migrate) { m.Tunnel.APITokenSecret = "" }, nil, "api_token_secret"},
		{"no tunnel token", nil, func(c *Config) { c.Proxy.Tunnel = nil }, "needs proxy.tunnel.token_secret"},
		{"swarm", nil, func(c *Config) {
			d := c.Destinations["production"]
			d.Runtime = "swarm"
			c.Destinations["production"] = d
		}, "compose runtime only"},
		{"scoped", nil, func(c *Config) {
			d := c.Destinations["production"]
			d.Access = "scoped"
			c.Destinations["production"] = d
		}, "needs access: operator"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := validMigrate()
			if tc.m != nil {
				tc.m(m)
			}
			errs := Validate(migrateCfg(m, tc.c))
			var msgs []string
			for _, e := range errs {
				msgs = append(msgs, e.Error())
			}
			if !strings.Contains(strings.Join(msgs, "\n"), tc.expect) {
				t.Fatalf("want %q in %v", tc.expect, msgs)
			}
		})
	}
	// A Destination-level tunnel satisfies the token requirement.
	c := migrateCfg(validMigrate(), func(c *Config) {
		c.Proxy.Tunnel = nil
		d := c.Destinations["production"]
		d.Proxy = &ProxyConfig{Tunnel: &TunnelConfig{TokenSecret: "T"}}
		c.Destinations["production"] = d
	})
	if errs := Validate(c); len(errs) != 0 {
		t.Fatalf("destination tunnel: %v", errs)
	}
	if got := (Migrate{}).FailurePolicy(); got != "restart_old" {
		t.Errorf("default on_failure %q", got)
	}
	if got := (MigrateTunnel{}).TargetService(); got != "http://yoho-proxy:80" {
		t.Errorf("default service %q", got)
	}
}

func TestSchemaHasMigrate(t *testing.T) {
	b, err := Schema()
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Defs map[string]struct {
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
			Required []string `json:"required"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Defs["Destination"].Properties["migrate"]; !ok {
		t.Fatal("Destination.migrate missing from schema")
	}
	m := s.Defs["Migrate"]
	if strings.Join(m.Properties["from"].Enum, ",") != "coolify,dokploy,compose" || strings.Join(m.Properties["on_failure"].Enum, ",") != "restart_old,leave" {
		t.Errorf("enums: %+v", m.Properties)
	}
	if strings.Join(s.Defs["MigrateTunnel"].Required, ",") != "api_token_secret,hostnames" {
		t.Errorf("tunnel required: %v", s.Defs["MigrateTunnel"].Required)
	}
}
