package config

import (
	"strings"
	"testing"
)

func TestValidateScopedAccess(t *testing.T) {
	c, err := Parse([]byte(`
app: a
servers: {a: {ssh: deb@home}, b: {ssh: y}}
destinations:
  ok: {servers: [a], access: scoped, gate_user: team-gate, ssh: {via: cloudflare, hostname: ssh.example.com}}
  bad: {servers: [a, b], runtime: swarm, access: open, gate_user: "Root!", ssh: {via: tailscale, hostname: "x.com;id"}}
  swarm: {servers: [a], runtime: swarm, access: scoped}
`), "t")
	if err != nil {
		t.Fatal(err)
	}
	var all []string
	for _, e := range Validate(c) {
		all = append(all, e.Error())
	}
	j := strings.Join(all, "\n")
	for _, want := range []string{
		`destinations.bad.access "open" must be operator or scoped`,
		`destinations.bad.gate_user "Root!"`,
		`destinations.bad.ssh.via "tailscale" must be cloudflare`,
		`destinations.bad.ssh.hostname "x.com;id"`,
		"destinations.bad.ssh: one hostname reaches one Server",
		"destinations.swarm: access: scoped supports the compose runtime only",
	} {
		if !strings.Contains(j, want) {
			t.Errorf("missing %q in:\n%s", want, j)
		}
	}
	if strings.Contains(j, "destinations.ok") {
		t.Errorf("ok Destination rejected:\n%s", j)
	}
	b, _ := Schema()
	for _, want := range []string{`"access"`, `"gate_user"`, `"hostname"`, `"cloudflare"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("schema lacks %s", want)
		}
	}
}
