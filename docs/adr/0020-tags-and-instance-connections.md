# Per-Server differences: tags and instance Connections

Status: Accepted. Extends the singleton-provider rule of ADR 0017.

Roles place one Service on several Servers, but every copy got the same
environment, and a Connection provider had to be singleton. Two lighter tools
cover most per-Server needs without a second Service or a scheduler (research:
docs/research/2026-11-27-stateful-ha-kamal-uncloud.md). Kamal selects hosts by
`tags` and applies `env.tags` per host. Uncloud's internal DNS answers
`<machine>.m.<service>.internal` with the containers on one machine.

Tags: `servers.<name>.tags` lists lowercase tags, unique per Server, never equal
to a Server name. Each Server's name is an implicit tag. `x-yoho.tags.<selector>.env`
sets environment on Servers that match the selector. On each Server the Compose
environment comes first, then every matching tag in the Server's tags order,
then the Server's name; a later value wins. The merge happens when Yoho compiles
the Server's Compose file, so plan, deploy and Roles share it, and the plan shows
`config changed: environment` only on Servers whose value changes. A selector that
matches no Server name or tag, a key that is not an environment name, and a
`YOHO_` key are errors. Tags work with and without Roles.

Instance Connections: `instances: true` on a Connection lifts the singleton
rule for its provider. An Instance is the containers of one Service on one
Server. Consumers get two names per provider Server, `<server>.m.<service>` and
`<server>.m.<service>.<app>.yoho.internal`. A remote Instance is an extra_hosts
entry for that Server's effective private address (the mesh address when the
mesh is on). An Instance on the consumer's own Server is a Compose network alias
on the provider, because mangle PREROUTING drops container traffic to the
Server's own private address. The bare Service name,
`<service>.<app>.yoho.internal` and `YOHO_CONNECTION_<NAME>_HOST` exist only
while the provider has one Server; `_PORT` is always set. There is no DNS server
and no resident process (ADR 0003): names change only when apply rewrites the
Compose files. Each provider Server publishes the port on its own private
address with the same consumer allowlist, firewall, ordering (providers before
consumers), readiness probes and binding checks as other Connections.

Refusals: a proxied provider (the Proxy already balances), more than one
container per Server (an Instance name must mean one container), the provider as
its own consumer (it would order a Server before itself), Server names that are
not DNS labels, and a consumer on every provider Server (a cycle). Tags refuse
the Swarm runtime and scoped access; the Gate refuses `x-yoho.tags`, and Roles
were already operator-only.

Not decided: per-tag secrets and labels, peer clusters whose Instances consume
each other, and a `nearest.<service>` alias. ADR 0005 is unchanged: a Stateful
Service still has exactly one Server, so instance Connections never replicate
data; each Instance is independent.
