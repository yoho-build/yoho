# Roles: multi-Server Compose

Status: Accepted. Supersedes the multi-Server requirement of ADR 0007.

Roles are the opt-in multi-Server path for the Compose runtime. Without
`destinations.<name>.roles`, existing plans and rendering remain unchanged.
Each Service belongs to exactly one Role; colocated Roles share one Compose
project per App/Destination/Server. Stateful Services have exactly one owner.

Connections publish singleton providers on effective private addresses (mesh
addresses when enabled), with consumer environment variables and extra_hosts
aliases for Compose names and `<service>.<app>.yoho.internal`. Same-Server
consumers retain Compose DNS. Providers precede consumers; readiness is checked
from each consumer Server. Cross-Server depends_on is removed from Compose.
Release commands execute on the first sorted Server hosting their Service;
Destination Hooks execute once. Locks cover all Servers in sorted order.
Each Server retains its own Release; one operation records rollout progress.
Interrupted rollouts must report partial or unknown outcomes and converge on rerun.

Docker-aware firewall rules in DOCKER-USER allow only consumer private addresses
and drop other sources for connection ports. Rules carry `yoho-roles
<app>/<destination>/<connection>` comments. Reconciliation touches only owned
rules and persists them with a Yoho-owned systemd oneshot after Docker. Missing
interfaces, tools, reachability or unverified protection refuse deployment.

Secrets are delivered only to their consuming Services. Shared generated values
are generated once, preferring the first Stateful consumer's Server, and delivered
to every consumer. Backups and restores follow volume ownership and record Server
placement. Service moves use a fresh Backup, stop-first cutover, an empty target
volume and health checks; source volumes survive until explicit cleanup. Failure
restarts the source and records the steps. Config edits are printed for the user.

Multi-Server proxied Services require a named Cloudflare Tunnel (a connector on
each ingress Server), or TLS disabled at the Proxy behind an external load
balancer. Independent HTTP-01 issuance is refused. DNS-01 is not implemented.

Roles adds no mesh, scheduler or resident daemon. The separate opt-in mesh of
ADR 0018 remains independent. Swarm is frozen: bug/security fixes and CI continue,
but no new features. Deprecation requires proven Swarm migration, interrupted
rollout recovery and blank-Server restore with Roles.
