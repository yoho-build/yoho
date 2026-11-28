package config

import (
	"strings"
	"testing"
)

// TestSupportMatrixRefusals asserts that every config-validated "No" cell in
// docs/site/support-matrix.md fails Validate with the documented error string.
func TestSupportMatrixRefusals(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "compose multi-server",
			yaml: `
app: shop
servers:
  a: {ssh: yoho@10.0.0.1}
  b: {ssh: yoho@10.0.0.2}
destinations:
  production: {servers: [a, b]}
`,
			want: RolesMessage,
		},
		{
			name: "swarm with scoped access",
			yaml: `
app: shop
servers:
  a: {ssh: yoho@10.0.0.1}
destinations:
  production: {servers: [a], runtime: swarm, access: scoped}
`,
			want: "destinations.production: access: scoped supports the compose runtime only",
		},
		{
			name: "preview from swarm destination",
			yaml: `
app: shop
servers:
  a: {ssh: yoho@10.0.0.1}
destinations:
  staging: {servers: [a], runtime: swarm}
previews:
  from: staging
  host: "{name}.preview.example.com"
`,
			want: "previews.from requires a compose Destination with operator access",
		},
		{
			name: "preview from scoped destination",
			yaml: `
app: shop
servers:
  a: {ssh: yoho@10.0.0.1}
destinations:
  staging: {servers: [a], access: scoped}
previews:
  from: staging
  host: "{name}.preview.example.com"
`,
			want: "previews.from requires a compose Destination with operator access",
		},
		{
			name: "migrate on swarm destination",
			yaml: `
app: shop
servers:
  a: {ssh: yoho@10.0.0.1}
destinations:
  production:
    servers: [a]
    runtime: swarm
    migrate:
      stop: {compose_project: old}
`,
			want: "destinations.production.migrate supports the compose runtime only; cut over to a compose Destination first, or stop the old stack by hand before `yoho deploy`",
		},
		{
			name: "migrate on scoped destination",
			yaml: `
app: shop
servers:
  a: {ssh: yoho@10.0.0.1}
destinations:
  production:
    servers: [a]
    access: scoped
    migrate:
      stop: {compose_project: old}
`,
			want: "destinations.production.migrate needs access: operator (the cutover stops other containers on the Server)",
		},
		{
			name: "subnet on swarm destination",
			yaml: `
app: shop
servers:
  a: {ssh: yoho@10.0.0.1}
destinations:
  production: {servers: [a], runtime: swarm, subnet: 10.200.3.0/24}
`,
			want: "destinations.production.subnet supports the compose runtime only; remove it or pin the subnet in the compose file's networks.default.ipam",
		},
		{
			name: "connection provider on several Servers without instances",
			yaml: `
app: shop
servers:
  a: {ssh: yoho@10.0.0.1, private_address: 10.0.0.1}
  b: {ssh: yoho@10.0.0.2, private_address: 10.0.0.2}
destinations:
  production:
    servers: [a, b]
    roles:
      api: {services: [api], servers: [a, b]}
    connections:
      upstream: {service: api, port: 8000, consumers: [api]}
`,
			want: "connection upstream target Service api must resolve to exactly one Server, or set instances: true to address each Server as `<server>.m.api`",
		},
		{
			name: "instance connection provider consuming itself",
			yaml: `
app: shop
servers:
  a: {ssh: yoho@10.0.0.1, private_address: 10.0.0.1}
  b: {ssh: yoho@10.0.0.2, private_address: 10.0.0.2}
destinations:
  production:
    servers: [a, b]
    roles:
      api: {services: [api], servers: [a, b]}
    connections:
      upstream: {service: api, port: 8000, consumers: [api], instances: true}
`,
			want: "connection upstream provider api cannot consume its own instance Connection; split the consumer into another Service",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tc.yaml), "yoho.yml")
			if err != nil {
				t.Fatalf("Parse failed: %v", err)
			}
			var msgs []string
			for _, e := range Validate(cfg) {
				msgs = append(msgs, e.Error())
			}
			joined := strings.Join(msgs, "\n")
			if !strings.Contains(joined, tc.want) {
				t.Errorf("Validate errors:\n%s\nwant substring %q", joined, tc.want)
			}
		})
	}
}
