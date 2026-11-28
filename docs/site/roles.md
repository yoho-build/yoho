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
consumers retain normal Compose DNS, with environment endpoints pointing to the
Service name and the qualified name retained as a network alias. Names that collide with existing aliases or
environment variables are refused. Applying a new placement updates the aliases.

Docker-published ports bypass UFW, and a FORWARD ACCEPT (for example Tailscale)
can bypass DOCKER-USER. Yoho instead filters **new original-direction requests**
in `mangle PREROUTING`, before DNAT and every filter FORWARD rule. One tagged
jump enters `YOHO-ROLES`; each App/Destination has an owned `YOHO-R-<hash>`
sub-chain. Consumer sources RETURN; other sources DROP. Replies never match.
Apply fills a fresh sub-chain and swaps only that App's jump atomically. Other
Apps retain their relative rule order. Root/passwordless sudo, `iptables`,
`ip6tables`, `flock` and systemd are required. The oneshot
`yoho-firewall.service` restores all App policies **before Docker and network
setup**, attempts every policy despite individual failures and fails at the end
if any failed. `yoho doctor` checks relative policy content and actual bindings.
Explicit loopback publications are preserved; non-loopback connection traffic
must use the protected private endpoint.

## Rollouts and recovery

`yoho plan` shows Service changes per Server and connection firewall changes.
Apply locks every Server in sorted name order before building or shipping.
Providers deploy before consumers. Yoho probes provider readiness **from each
consumer Server**, with a bounded TCP check, before starting consumers. Cross-Server
`depends_on` entries are removed from the rendered Compose files. Server dependency
cycles are refused; colocate cyclic dependencies. The readiness probe requires
`timeout` and either `nc` or Bash's `/dev/tcp`.

Release commands run once on the first sorted Server hosting the declaring
Service in a separate phase: providers are readied before the command, and no
replica of that Service switches before it succeeds. All Servers pass read-only
ownership, hostname and subnet preflight before the first mutation. Destination deploy and App boot Hooks run once; Proxy reboot Hooks run
on each affected Server. Only Servers hosting proxied Services boot the Proxy.
Each Server keeps a Release. One operation tracks progress with `server/service`
keys. A mixed rollout reports partial (exit 13); lost SSH reports unknown (14).
Inspect `yoho status --operation ID`, restore connectivity and rerun apply to
converge. Rollback uses each Server's saved Release; it does not restore data or
reverse a Stateful Service move.

Each Server receives only its Services' secrets. A shared generated key is
created once, preferring the first Stateful consumer's Server, otherwise the
first sorted consumer Server. Its owner is pinned in Release metadata and cannot
shift on a placement edit. Missing owner data refuses apply instead of generating
a new value. Only names declared in `x-yoho.secrets` reach containers; unused
consumer copies are pruned. Apply distributes that value to the consumers;
rotation updates their Releases and secret generations.

## Multi-Server ingress

For a proxied Service on several Servers, use one of:

- A **named Cloudflare Tunnel** (`proxy.tunnel.token_secret`). Yoho runs a connector
  on each Server hosting proxied Services. Cloudflare provides TLS, balancing and
  connector failover. Proxy TLS metadata is preserved, including customer-domain
  routes; Yoho closes public Proxy HTTP/HTTPS ports for Tunnel ingress.
- An external load balancer with TLS termination and health checks, with
  explicit `x-yoho.proxy.ssl: false` on each Service. DNS round-robin alone does not provide
  health checks or failover.

Independent Let's Encrypt HTTP-01 issuance across Servers is refused without a
named Tunnel. DNS-01 and shared certificates are not implemented. `ssl` is an explicit TLS selection; existing `tls` configuration remains valid.
A Quick Tunnel is refused for multi-Server ingress.

## Stop a Service, promote a standby

`yoho app stop SERVICE` stops a Service on every Server hosting it (or the
one named with `--server`) and keeps it stopped through deploy, apply and
rollback until `yoho app start SERVICE`; volumes are kept. A consumer whose
Connection provider is stopped fails its deploy with a message naming the stopped
provider. For a Postgres primary and standby, `yoho db promote` fences the old
primary with `app stop`, promotes the standby and applies the edited
Connection; see [Postgres primary and standby](postgres-replication.md) and the
[CLI](cli.md#database).

## Move a Stateful Service

Configure `x-yoho.stateful`, a Docker healthcheck, and `x-yoho.backup.volumes` covering **every** named volume,
plus a Backup Target/job. A dump does not exempt unrelated named volumes.
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

A journal under the source App directory records each step. Before target startup,
a failed copy can restart the source. Once target startup may have accepted
writes, a later failure is **partial**: target data is retained and the source
stays stopped. Inspect the exact journal steps and converge with the printed
target Role placement; never restart a stale source over committed target writes.
Moves verify the deployed Version/config and target image before stopping data,
and never rerun release commands or deploy Hooks. Failed copies remain for
inspection and must be explicitly emptied before retrying. Source data is never
removed automatically. Cleanup requires a successful journal and verifies
ownership and absence of mounts.

## Integration tests

Disposable Linux Servers can run the guarded Roles test with `YOHO_E2E=1`,
`YOHO_E2E_ROLES_HOSTS="user@a,user@b[,user@c]"` and matching
`YOHO_E2E_ROLES_ADDRESSES="10.0.0.11,10.0.0.12[,10.0.0.13]"`.
It exercises Compose-name connections, redeploy requests, rollback, Backup/restore,
interruption/convergence and a data-preserving move. The optional third Server
checks that the database port is blocked. These tests are not wired into CI.

Set `YOHO_E2E_ROLES_PUBLIC_B` to check the provider’s public address. Optional `YOHO_E2E_ROLES_TUNNEL_TOKEN` and `YOHO_E2E_ROLES_TUNNEL_URL` enable the named Tunnel connector failover subtest.
