# Roles: multi-Server Compose

Roles place subsets of an App's Services on subsets of a Destination's Servers.
They are opt-in: omit `roles` to keep the existing single-Server behavior.
Roles requires operator access and the Compose runtime.

```yaml
servers:
  app1: {ssh: deploy@app1, private_address: 10.0.0.11, sudo: true}
  data1: {ssh: deploy@data1, private_address: 10.0.0.12, sudo: true}
destinations:
  production:
    servers: [app1, data1]
    roles:
      app: {services: [web, worker], servers: [app1]}
      data: {services: [db], servers: [data1]}
    connections:
      database: {service: db, port: 5432, consumers: [web, worker]}
```

Every Service belongs to exactly one Role. Stateful Services (and Services with
persistent data) must have exactly one Server. Colocated Roles share a Compose
project. Backups must select Services with the same volume owner; use separate
jobs for data on different Servers. The Backup manifest records its Server.
Scheduled Jobs run on the owning Server.

## Connections and private networking

A connection provider must be singleton. Yoho publishes its TCP port only on the
Server's effective private address: the WireGuard allocation when the optional
[mesh](mesh.md) is enabled, otherwise `private_address`. Every participating
Server needs a reachable address, and provider addresses must exist on an interface.
Consumer and provider addresses must use the same IP family. Wildcard Compose
port bindings and host networking on providers are refused.

Remote consumers receive `YOHO_CONNECTION_DATABASE_HOST` and
`YOHO_CONNECTION_DATABASE_PORT`. They also receive `extra_hosts` entries for `db`
and `db.<app>.yoho.internal`, so `postgres://db:5432` keeps working. Same-Server
consumers retain normal Compose DNS. Names that collide with existing aliases or
environment variables are refused. Applying a new placement updates the aliases.

Docker-published ports bypass UFW. Yoho installs consumer allowlists followed by
DROP rules in `DOCKER-USER`, matching the original destination before Docker's
DNAT. Rules carry `yoho-roles <app>/<destination>/<connection>` comments. Apply
inserts the desired protection before deleting stale owned rules; unrelated
rules remain untouched. Root or passwordless sudo, `iptables`, `ip6tables`, Docker
forwarding chains and systemd are required. A Yoho-owned `yoho-firewall.service`
replays the rules after Docker starts. `yoho doctor` checks actual bindings and
firewall rules. Check private network routing before the first deployment.

## Rollouts and recovery

`yoho plan` shows Service changes per Server and connection firewall changes.
Apply locks every Server in sorted name order before building or shipping.
Providers deploy before consumers. Yoho probes provider readiness **from each
consumer Server**, with a bounded TCP check, before starting consumers. Cross-Server
`depends_on` entries are removed from the rendered Compose files. Server dependency
cycles are refused; colocate cyclic dependencies. The readiness probe requires
`timeout` and either `nc` or Bash's `/dev/tcp`.

Release commands run once on the first sorted Server hosting the declaring
Service. Destination deploy and App boot Hooks run once; Proxy reboot Hooks run
on each affected Server. Only Servers hosting proxied Services boot the Proxy.
Each Server keeps a Release. One operation tracks progress with `server/service`
keys. A mixed rollout reports partial (exit 13); lost SSH reports unknown (14).
Inspect `yoho status --operation ID`, restore connectivity and rerun apply to
converge. Rollback uses each Server's saved Release; it does not restore data or
reverse a Stateful Service move.

Each Server receives only its Services' secrets. A shared generated key is
created once, preferring the first Stateful consumer's Server, otherwise the
first sorted consumer Server. Apply distributes that value to the consumers;
rotation updates their Releases and secret generations.

## Multi-Server ingress

For a proxied Service on several Servers, use one of:

- A **named Cloudflare Tunnel** (`proxy.tunnel.token_secret`). Yoho runs a connector
  on each Server hosting proxied Services. Cloudflare provides TLS, balancing and
  connector failover. Disable Proxy TLS (`x-yoho.proxy.tls: false`).
- An external load balancer with TLS termination and health checks, with
  `x-yoho.proxy.tls: false` on each Service. DNS round-robin alone does not provide
  health checks or failover.

Independent Let's Encrypt HTTP-01 issuance across Servers is refused without a
named Tunnel. DNS-01 and shared certificates are not implemented. The config key
is `tls`, matching the existing Proxy configuration (`ssl` is not an alias).

## Move a Stateful Service

Configure `x-yoho.stateful`, a Docker healthcheck, and either
`x-yoho.backup.volumes` covering every named volume or a consistent Backup dump,
plus a Backup Target/job.
Volumes must be owned plain local volumes, without shared mounts or driver
options. Bind mounts and Services without owned named volumes require a manual migration.

```sh
yoho service move db --to app1 --plan
yoho service move db --to app1 --auto-approve
# Review, edit and commit the printed Role placement before the next apply.
# Later, explicitly remove only the retained source volumes:
yoho service move db --to app1 --cleanup-source --auto-approve
```

The operation takes a fresh Backup, stops the source, streams the stopped volume
contents over SSH into empty target volumes, starts and health-checks the target,
and redeploys consumers with the new connection address. This is a **stop-first
operation with downtime**. Existing target data or mounted target volumes refuse
the move. The user's config file is never rewritten.

A journal under the source App directory records each step. On failure Yoho
stops the target before restarting the source and reconciling consumers. If the
target cannot be stopped, source restart is refused to prevent uncertain writes;
restore SSH access and inspect the journal before recovery. A failed copy leaves
target data in place for inspection; empty the owned target volume explicitly
before retrying. Source data is never removed automatically. Cleanup requires a
successful journal and verifies ownership and absence of mounts.

## Integration tests

Disposable Linux Servers can run the guarded Roles test with `YOHO_E2E=1`,
`YOHO_E2E_ROLES_HOSTS="user@a,user@b[,user@c]"` and matching
`YOHO_E2E_ROLES_ADDRESSES="10.0.0.11,10.0.0.12[,10.0.0.13]"`.
It exercises Compose-name connections, redeploy requests, rollback, Backup/restore,
interruption/convergence and a data-preserving move. The optional third Server
checks that the database port is blocked. These tests are not wired into CI.

Set `YOHO_E2E_ROLES_PUBLIC_B` to check the provider’s public address. Optional `YOHO_E2E_ROLES_TUNNEL_TOKEN` and `YOHO_E2E_ROLES_TUNNEL_URL` enable the named Tunnel connector failover subtest.
