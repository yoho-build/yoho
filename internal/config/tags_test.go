package config

import (
	"maps"
	"strings"
	"testing"
)

func TestServerEnvMergeOrder(t *testing.T) {
	ext := ServiceExt{Tags: map[string]ServiceTag{
		"eu":     {Env: map[string]string{"REGION": "eu", "LEVEL": "eu"}},
		"canary": {Env: map[string]string{"FEATURE_X": "1", "LEVEL": "canary"}},
		"a":      {Env: map[string]string{"NODE_NAME": "alpha", "REGION": "eu-a"}},
		"other":  {Env: map[string]string{"UNUSED": "1"}},
	}}
	got := ServerEnv(ext, "a", Server{Tags: []string{"eu", "canary"}})
	want := map[string]string{"REGION": "eu-a", "LEVEL": "canary", "FEATURE_X": "1", "NODE_NAME": "alpha"}
	if !maps.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	// Tag order decides between tags.
	got = ServerEnv(ext, "b", Server{Tags: []string{"canary", "eu"}})
	if got["LEVEL"] != "eu" || got["REGION"] != "eu" {
		t.Fatalf("tag order: %v", got)
	}
	if ServerEnv(ext, "c", Server{}) != nil || ServerEnv(ServiceExt{}, "a", Server{Tags: []string{"eu"}}) != nil {
		t.Fatal("unmatched Server got environment")
	}
}

func TestServerTagValidation(t *testing.T) {
	for name, tt := range map[string]struct {
		tags []string
		want string
	}{
		"valid":       {[]string{"eu", "canary"}, ""},
		"pattern":     {[]string{"EU"}, `tag "EU" must match`},
		"duplicate":   {[]string{"eu", "eu"}, "tag eu is repeated"},
		"server name": {[]string{"b"}, "tag b is also a Server name"},
	} {
		t.Run(name, func(t *testing.T) {
			c := &Config{App: "shop", Servers: map[string]Server{"a": {SSH: "a", Tags: tt.tags}, "b": {SSH: "b"}}, Destinations: map[string]Destination{"production": {Servers: []string{"a"}}}}
			var msgs []string
			for _, e := range Validate(c) {
				msgs = append(msgs, e.Error())
			}
			got := strings.Join(msgs, "\n")
			if tt.want == "" && got != "" || tt.want != "" && !strings.Contains(got, tt.want) {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
	if got := TagSelectors(map[string]Server{"a": {Tags: []string{"eu"}}, "b": {Tags: []string{"eu", "canary"}}}); strings.Join(got, ",") != "a,b,canary,eu" {
		t.Fatal(got)
	}
}

func TestInstanceConnectionValidation(t *testing.T) {
	base := func() *Config {
		return &Config{App: "shop", Servers: map[string]Server{"a": {SSH: "a", PrivateAddress: "10.0.0.1"}, "b": {SSH: "b", PrivateAddress: "10.0.0.2"}, "c": {SSH: "c", PrivateAddress: "10.0.0.3"}},
			Destinations: map[string]Destination{"production": {Servers: []string{"a", "b", "c"},
				Roles:       map[string]Role{"echo": {Services: []string{"echo"}, Servers: []string{"a", "b"}}, "lb": {Services: []string{"lb"}, Servers: []string{"c"}}},
				Connections: map[string]Connection{"upstream": {Service: "echo", Port: 8000, Consumers: []string{"lb"}, Instances: true}}}}}
	}
	run := func(c *Config) string {
		var msgs []string
		for _, e := range validateRoles(c, "production", c.Destinations["production"]) {
			msgs = append(msgs, e.Error())
		}
		return strings.Join(msgs, "\n")
	}
	if got := run(base()); got != "" {
		t.Fatal(got)
	}
	c := base()
	conn := c.Destinations["production"].Connections["upstream"]
	conn.Instances = false
	c.Destinations["production"].Connections["upstream"] = conn
	if got := run(c); !strings.Contains(got, "or set instances: true to address each Server as `<server>.m.echo`") {
		t.Fatal(got)
	}
	c = base()
	conn = c.Destinations["production"].Connections["upstream"]
	conn.Consumers = []string{"lb", "echo"}
	c.Destinations["production"].Connections["upstream"] = conn
	if got := run(c); !strings.Contains(got, "provider echo cannot consume its own instance Connection") {
		t.Fatal(got)
	}
	c = base()
	c.Servers["B_2"] = Server{SSH: "x", PrivateAddress: "10.0.0.4"}
	d := c.Destinations["production"]
	d.Servers = append(d.Servers, "B_2")
	d.Roles["echo"] = Role{Services: []string{"echo"}, Servers: []string{"a", "B_2"}}
	c.Destinations["production"] = d
	if got := run(c); !strings.Contains(got, `instance name needs Server name "B_2"`) {
		t.Fatal(got)
	}
}
