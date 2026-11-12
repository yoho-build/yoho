package config

import (
	"strings"
	"testing"
)

func TestPreviewName(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"PR-123", "pr-123"}, {" feat/foo__bar--- ", "feat-foo-bar"}, {"123", "123"},
		{strings.Repeat("a", 50), strings.Repeat("a", 40)},
		{strings.Repeat("a", 39) + "---suffix", strings.Repeat("a", 39)},
		{"!!!", ""}, {"", ""}, {"你好", ""},
	} {
		got, err := PreviewName(tt.in)
		if got != tt.want || (err != nil) != (tt.want == "") {
			t.Errorf("%q: %q, %v; want %q", tt.in, got, err, tt.want)
		}
	}
}

func TestPreviewHosts(t *testing.T) {
	for _, tt := range []struct{ pattern, name, service, want string }{
		{"{name}.preview.example.com", "pr-1", "web", "pr-1.preview.example.com"},
		{"{service}-{name}.example.com", "pr-1", "web", "web-pr-1.example.com"},
		{"{name}.EXAMPLE.com", "pr-1", "web", "pr-1.example.com"},
		{"app.example.com", "pr-1", "web", ""},
		{"{name}.{other}.example.com", "pr-1", "web", ""},
		{"{name}.example.com/path", "pr-1", "web", ""},
		{"prefix-{name}.example.com", strings.Repeat("a", 60), "web", ""},
		{"{name}.example.com", "-bad", "web", ""},
		{"{name}." + strings.Repeat("abcd.", 50) + strings.Repeat("a", 60), "pr-1", "web", ""},
	} {
		got, err := (PreviewsConfig{Host: tt.pattern}).RenderHost(tt.name, tt.service)
		if got != tt.want || (err != nil) != (tt.want == "") {
			t.Errorf("%q: %q %v; want %q", tt.pattern, got, err, tt.want)
		}
	}
}

func TestValidatePreviews(t *testing.T) {
	c := &Config{Destinations: map[string]Destination{"staging": {}}, Previews: &PreviewsConfig{From: "staging", Host: "{name}.example.com"}}
	if errs := ValidatePreviews(c); len(errs) != 0 {
		t.Fatal(errs)
	}
	c.Previews.From = "missing"
	if len(ValidatePreviews(c)) == 0 {
		t.Fatal("unknown template accepted")
	}
	c.Previews.From = "staging"
	for _, d := range []Destination{{Runtime: "swarm"}, {Access: "scoped"}} {
		c.Destinations["staging"] = d
		if len(ValidatePreviews(c)) == 0 {
			t.Fatal("unsupported template accepted")
		}
	}
}

func TestPreviewDestination(t *testing.T) {
	d := Destination{Servers: []string{"s"}, Env: map[string]string{"X": "y"}, SSH: &DestinationSSH{Via: "cloudflare"}, AppDir: "/data/prod", Subnet: "10.0.0.0/24", Migrate: &Migrate{}, Proxy: &ProxyConfig{Tunnel: &TunnelConfig{}}}
	p := PreviewDestination(d)
	if p.AppDir != "" || p.Subnet != "" || p.Migrate != nil || p.Proxy.Tunnel != nil || p.Servers[0] != "s" || p.Env["X"] != "y" || p.SSH != d.SSH {
		t.Fatalf("bad derived Destination: %+v", p)
	}
	if d.Proxy.Tunnel == nil || d.Migrate == nil {
		t.Fatal("template was mutated")
	}
}
