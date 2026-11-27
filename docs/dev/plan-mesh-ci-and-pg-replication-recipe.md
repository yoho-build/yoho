# Plan: mesh CI, and a tested Postgres primary/standby recipe on Roles

Status: revised after review (see "Review log").

## Problem

1. The opt-in WireGuard mesh (ADR 0018) has no automatic test. `TestE2EMesh`
   runs only when `YOHO_E2E_MESH_HOSTS` is set, and CI never sets it. Roles has a
   CI job (`test/integration/roles.sh`), but it uses `private_address`, not the
   mesh.
2. On a Roles Destination with `network.mesh: wireguard`, `yoho plan` and
   `yoho deploy` stop with "Server X has no mesh allocation; run yoho mesh up"
   until `yoho mesh up` has run once. This contradicts `docs/site/mesh.md`,
   which says that deploy reconciles the mesh and that plan reports mesh changes.
3. Users ask for "master/slave Postgres across Servers".
   `docs/site/high-availability.md` says it is out of scope and stops there.
   Kamal leaves it to a community article, and Uncloud says the app should
   replicate itself (`docs/research/2026-11-27-stateful-ha-kamal-uncloud.md`).
   Yoho already has the parts Kamal lacks: pinned Stateful Services, private-only
   Connections behind a firewall, provider-before-consumer readiness, and
   Backups. What is missing is a recipe that has been proven to work.

## Scope (one PR)

### A. Mesh CI: `test/integration/mesh.sh` and a `mesh` job

**Setup.** Three nested SSH+Docker Servers (a, b, c):

- They are reached by their outer Docker-network IPs, not `127.0.0.1` ports.
  This way, the default endpoints and the allowlist for the `endpoint: none`
  Server are real addresses.
- c is `endpoint: none`, and it drops unsolicited inbound UDP.
- The image pre-installs `wireguard-tools iproute2 iptables iputils-ping ufw procps`.
- The workflow runs `sudo modprobe wireguard` on the runner first.
- The `systemctl` shim no-ops only `daemon-reload/start/stop/restart`, and
  prints SKIP lines when it does.

**The script asserts:**

1. **Plan before `mesh up`.** Run against the Roles Destination itself; this
   needs fix B. It reports mesh changes and planned addresses, and it creates
   no `yoho0`.
2. **After `mesh up`:**
   - Addresses and peer counts are correct.
   - The NATed Server has no ListenPort and no inbound rule.
   - Hub-only state exists only on the hub: `99-yoho-mesh.conf`,
     `ip_forward=1`, the DOCKER-USER `yoho-mesh` rule, and `ufw route allow`.
   - No unrestricted UDP 51820 rule exists anywhere.
   - The hub allowlist names c's real egress IP.
   - Key files are mode 0600.
3. **Reachability.** Pings succeed between all Server pairs, including non-hub
   to NATed through the hub. `mesh status --json` shows handshakes and RTTs,
   and contains no private key material.
4. **Idempotence.** A second `mesh up` leaves the before/after snapshot
   unchanged: conf hash, ifindex, public keys, iptables and ufw rules. Then
   `plan --detailed-exitcode` exits 0.
5. **Roles over the mesh.**
   - `db` resolves to c's mesh address inside web, and a real query works
     through the Proxy.
   - A non-consumer mesh peer is refused at c's port (mangle PREROUTING on
     `yoho0`).
   - c's port is not reachable on its Docker-network address.
6. **`mesh rotate-keys`.** Public keys change, and pings still work afterwards.
7. **Teardown.**
   - `remove -y` cleans both Servers.
   - `mesh down` removes `yoho0`, the conf, key, owned marker, `state.json`,
     the sysctl file, the `yoho-mesh` iptables and ufw rules, and the enabled unit.
8. **Go test.** `TestE2EMesh` also runs against the same Servers.

### B. Fix: plan and deploy before the first `mesh up`

When `network.mesh: wireguard` is set and a Server has no allocation yet:

- **`plan`** uses the planned allocation (`mesh.Allocate` over the configured
  Servers and the existing state) for Roles addresses, and labels it as planned.
- **`deploy` and `apply`** reconcile the mesh first, then read the
  allocations. They never fall back to `private_address`; that rule from
  `TestReviewMeshMissingAllocationDoesNotFallback` stays.

The fix includes unit tests for both paths.

### C. Postgres primary/standby recipe

The files live in `examples/postgres-replication/`, and the CI job deploys
this exact directory. The guide is at `docs/site/postgres-replication.md`.

**Services.** `db` and `db-standby` share the same image and entrypoint, and
both are Stateful. Each has one Server: A and B. The Connection is
`primary: {service: db, port: 5432, consumers: [web, db-standby]}`.

**Secrets.**
- `POSTGRES_PASSWORD` and `REPLICATOR_PASSWORD` are **user secrets** from
  `.yoho/secrets`, delivered to both database Services. They are *not*
  `x-yoho.generate`.
- Reason: generated-secret owners are pinned (ADR 0019). After a switchover,
  the pinned owner would be a read-only standby, and rotation would fail.
- Rotating these two secrets is documented as a manual procedure.

**One symmetric entrypoint:**
- If `$PGDATA/PG_VERSION` exists, start Postgres.
- If it is missing and `YOHO_CONNECTION_PRIMARY_HOST` is set (a consumer of
  `primary`), the Service becomes a standby:
  1. Clear any partial `$PGDATA`.
  2. Wait with `pg_isready`.
  3. Run `pg_basebackup -R -X stream` using a passfile. The conninfo carries
     no password.
- Otherwise, run initdb as the primary. An init script creates the
  `replicator` role, reading its password from the secret file.
- On every start, write a 0600 `.pgpass` from the current secret files.
  `primary_conninfo` uses `passfile=`, so rotating a password does not break
  replication.

**`pg_hba`.** One mounted `hba_file` for both nodes, with `scram-sha-256` and
replication allowed from any source address. The Yoho Connection firewall
restricts access to consumer Servers, so the file can stay the same after a
promotion.

**Promotion (manual, documented, tested in CI):**

1. Fence the old primary:
   - If Server A is alive, run
     `psql -c "ALTER SYSTEM SET default_transaction_read_only=on"`, reload,
     then terminate the other backends.
   - If A is lost, it is already fenced. Remove A from the Destination and
     remove `db` from its Role/compose.
2. Promote with `psql -U postgres -c "select pg_promote()"`. Do not use
   `pg_ctl` as root.
3. Edit the Connection: set `service: db-standby` and `consumers: [web, db]`.
   web uses `YOHO_CONNECTION_PRIMARY_HOST/PORT`, not the alias `db`.
4. Run `yoho apply`.
5. Rebuild the old `db` as a standby:
   - Remove its volume on Server A by hand. Yoho never deletes volumes.
   - Run `yoho apply` again. The entrypoint sees an empty data directory as a
     consumer and runs a base backup from `db-standby`.

### D. CI for the recipe: `test/integration/pg-replication.sh` and a job

**Setup.** Two Servers plus an outsider container, and the permissive
`FORWARD ACCEPT` injection from `roles.sh`.

**Before promotion, assert:**
- A row written through web appears on the standby.
- The standby rejects writes (`read-only transaction`).
- `pg_stat_replication` shows the standby in `streaming` state.
- The outsider cannot reach 5432 on A.
- After a standby restart, replication resumes without a new base backup.
- After rotating `REPLICATOR_PASSWORD` by the documented steps, replication
  still streams.

**Then run the full promotion (steps 1 to 5), and assert:**
- web writes through `db-standby`.
- The firewall now protects B.
- The rebuilt `db` streams from `db-standby`.
- The new primary is not demoted after a restart.

**Finally,** `remove -y` cleans both Servers.

### E. Shared harness

Move the nested-Server setup into `test/integration/servers.sh`. Run
`shellcheck -x` on all scripts, including `servers.sh`, in every job.

### F. Docs

- `high-availability.md` links to the recipe and still says "Yoho does not
  fail over".
- `mesh.md` documents that `plan` works before the first `mesh up`.
- Add the zh research report.
- Add deferred ideas to `docs/backlog.md`.

## Not in scope (backlog)

- `yoho db promote` (switchover helper).
- Per-Server instance aliases (`<server>.m.<service>`).
- Kamal-style per-Server tags/env.
- Automatic failover of any kind (ADR 0003).
- MySQL recipes.
- `yoho app stop` for fencing.

## Decisions

- **Recipe, not a feature.** Each Service keeps one owner (ADR 0005/0017), and
  the standby is a different Service. This matches Uncloud's stance. It is
  better than Kamal's community recipe, because role creation, `pg_hba` and
  passwords survive recreation.
- **No automatic failover.** It needs a resident leader election (repmgrd,
  Patroni, Orchestrator), which ADR 0003 rules out. Split-brain is worse than
  downtime for a small team. The documented promotion fences before it promotes.
- **User secrets, not generated secrets, for database passwords.** Generated
  owners are pinned per ADR 0019, and that does not survive a switchover.
- **pg_hba allows replication from any address, and the Yoho firewall is the
  network control.** The Connection firewall already allows only consumer
  Servers, and the mesh or private addresses are not routable from outside. A
  per-IP `pg_hba` would be cloned to the standby and break the rebuild.
- **Async streaming by default.** Sync replication is mentioned only as an
  option.
- **One PR**, per the repo owner's rule. All jobs run on every PR, because the
  proof is the CI run itself: nested dind with WireGuard does not run on macOS.

## Proof

- `go build ./... && go vet ./... && go test ./...`, and `shellcheck -x` on
  every script.
- The `mesh`, `pg-replication` and `roles` jobs pass on the PR.
- Known local-only failure: `internal/backup` staging tests fail on macOS and
  pass on Linux CI. This was already true on main.

## Review log

agy-review (gemini-4-neon-xhigh), 2026-11-27, on the first draft. Accepted:

- Generated-secret owner pinning breaks promotion and rotation. Switched to
  user secrets.
- A per-IP `pg_hba` breaks the rebuild. Switched to one symmetric `hba_file`
  behind the firewall.
- `pg_ctl` cannot run as root, and promoting without fencing causes
  split-brain. Changed to fence first, then `pg_promote()`, then switch the
  Connection and consumers, and web uses the Connection env.
- `mesh down` and the systemctl shim; `procps` missing for `sysctl`. Fixed in
  the harness.
- `plan` before `mesh up` fails on Roles. Fixed in code (B) instead of being
  worked around.
- `mesh up` has no "no change" output. Idempotence is asserted with snapshots
  plus `plan --detailed-exitcode`.
- `pg_basebackup -R` stores the password. Switched to a passfile.
- Partial base backup. Guard on `PG_VERSION`.
- Missing assertions: the example dir is deployed as-is, the full promotion,
  restarts, rotation, cleanup, `mesh status`, hub-only invariants, non-consumer
  rejection, `mesh down`, and `rotate-keys`.
- 127.0.0.1 SSH targets corrupt the endpoint allowlist. Switched to real IPs.
- `shellcheck -x` for the sourced harness.

Not accepted: "split into several PRs" (the repo owner requires one PR).
