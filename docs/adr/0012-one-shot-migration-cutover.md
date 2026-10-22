# One-shot migration cutover

Status: Proposed. Refs [#8](https://github.com/yoho-build/yoho/issues/8).

## Context

Moving a live App from Coolify, Dokploy or plain compose to Yoho with little downtime needs a one-time cutover. Today every operator writes it by hand as a `pre-deploy` Hook (see the `yoho-migrate-coolify` skill):

- stop the old stack after the image is built and shipped;
- prove that no running container still mounts the data directories or volumes Yoho will reuse, because two Postgres servers on one data directory corrupt it;
- move Cloudflare Tunnel ingress (or DNS) to `yoho-proxy`;
- guard all of it so only the production Destination runs it, and delete the Hook afterwards;
- have a rollback ready, written and tested separately.

The first hand-written Hooks needed several iterations. A Cloudflare API token expired between stopping the old stack and switching routes. The Hook had to skip itself on a rehearsal Destination through `YOHO_DESTINATION`. Mistakes here are outages.

Yoho runs the `cloudflared` connector (`internal/proxy/tunnel.go`) but never calls the Cloudflare API; ingress rules are edited by hand or by an agent. `internal/deploy/cutover.go` is the zero-downtime Proxy switch inside a deploy and is unrelated.

## Decision

1. **A `migrate:` block on a Destination, run once by `apply`.** Config lives on the Destination, so a rehearsal Destination without the block never touches the old stack or ingress. This replaces the `YOHO_DESTINATION` guard.

   ```yaml
   destinations:
     production:
       servers: [main]
       migrate:
         from: coolify              # coolify | dokploy | compose; picks label defaults only
         stop:                      # old workloads, matched on the Server
           compose_project: old-stack
           labels: ["coolify.applicationId=ab12"]
           containers: [old-web]
         claim:                     # must not be mounted by any running container once `stop` ran
           paths: [/data/app/postgres]
           volumes: [old_pgdata]
         tunnel:
           api_token_secret: CF_API_TOKEN   # secret key, resolved locally
           hostnames: [app.example.com]     # ingress rules to repoint; others are kept
           service: http://yoho-proxy:80    # default
         on_failure: restart_old            # default; or "leave"
   ```

2. **Operator machine only, one-shot, never resident.** The cutover runs inside `yoho apply` on the operator's machine (ADR 0008) and leaves nothing running (ADR 0003). The existing `pre-deploy` Hook stays the escape hatch for anything the block cannot express. `pre-cutover` and `post-cutover` Hooks are added.

3. **Order inside `apply`, between `buildAndShip` and the rollout.** The build stays outside the downtime window, as in the hand-written Hook.
   1. *Preflight*, read-only, before anything changes: resolve `api_token_secret`, GET the tunnel configuration (account and tunnel IDs are decoded from the existing `proxy.tunnel.token_secret`), confirm every `hostnames` entry has an ingress rule, confirm SSH and Docker, list the containers `stop` matches (zero matches is an error unless `--allow-empty`).
   2. *Stop* the matched containers by saved ID (`docker stop`, never `rm`) and write the Journal.
   3. *Claim check*: every running container not labelled `yoho.app=<this app>` is inspected; any mount of a claimed path (prefix match on cleaned absolute paths) or volume aborts and runs `on_failure`.
   4. *Rollout*: the normal deploy. On failure with `on_failure: restart_old`, `docker start` the stopped IDs.
   5. *Switch ingress* after the Services are healthy, which shortens the outage compared with switching first. The previous configuration is written to the Journal before the PUT; only the `service` of the listed hostnames changes and the final catch-all rule is kept.
   6. Mark the Journal `done` and report the downtime window.

4. **The Journal is the durable state.** `<AppDir>/cutover.json`, mode 0600, written through `Host.WriteFile`: `{started_at, stopped:[id,name], tunnel_before, tunnel_after_hash, state: started|done|rolled_back}`. It holds no secret values. `release.Release` gains `Cutover string` (omitted when empty) so `yoho releases` shows which Release performed it. A `done` marker makes a rerun a no-op. A `started` marker makes `apply` refuse and point at `yoho rollback --cutover` (undo) or `yoho deploy --cutover-resume` (continue from the first unfinished step; every step is idempotent).

5. **A Cloudflare API client in `internal/proxy`**, next to `tunnel.go`, on stdlib `net/http` with no new dependency. It runs on the operator machine for both Local and SSH Hosts. This is Yoho's first call to a third-party control-plane API, so it is scoped narrowly: read and write the configuration of one tunnel, nothing else. The token is read from the secrets store, sent only in the `Authorization` header, never in argv, a script or the Journal, and client errors pass through the redactor. The base URL is overridable by `YOHO_CLOUDFLARE_API` for tests.

6. **Rollback is a mode of `rollback`.** `yoho rollback --cutover` stops this App's Yoho Services (by project and `yoho.*` labels, never `down -v`), restarts the saved container IDs, and PUTs `tunnel_before`. It refuses when the live ingress hash differs from `tunnel_after_hash` (someone changed it since) unless `--force`. It does not revert volumes or migrations, like `rollback VERSION`.

7. **New commands:** `yoho migrate plan` (preflight only, prints findings) and `yoho migrate status` (reads the Journal). `--json` adds one `cutover` event per phase (`preflight`, `stopped`, `claims_clear`, `ingress_switched`, `done`, `rolled_back`) with container names, hostnames and timings, no values.

8. **Compose runtime only in v1.** The old platforms run plain containers. `config check` rejects `migrate` on a Swarm Destination with a hint. A new package `internal/migrate` holds the logic (`Preflight`, `Plan.Stop`, `Plan.CheckClaims`, `Plan.SwitchIngress`, `Rollback`) so `internal/deploy/cutover.go` keeps its meaning.

9. **The rehearsal Destination pattern is documentation, not code.** A second Destination with its own `servers` (for example a LAN box), overlays that point data at a restored copy, side-effect Services (schedulers, off-Server Backups, the tunnel) replaced with `sleep`, test-mode payment keys, and a cold-start rehearsal (stop everything, then `yoho deploy`) to reproduce cutover ordering. It has no `migrate` block. It ships in the `yoho-migrate-coolify` skill reference and the migration guides with this ADR.

## Consequences

- A Coolify or Dokploy migration needs no hand-written Hook. Validation of the token and ingress before the first `docker stop` removes the expired-token failure mode.
- New schema surface (`Destination.migrate`), a new state file, a third-party API dependency at runtime for tunnel users, and a new rollback mode. All of it is opt-in per Destination and inert when the block is absent.
- The Cloudflare token needs Tunnel edit permission and lives in the operator's secrets store. A leaked token can edit that tunnel's routes; the Journal and logs never contain it.
- Ingress switches after health checks, so there is a window where the old stack is stopped and the tunnel still points at it. This is the same window the hand-written Hook has; the ADR accepts it rather than switching first.
- Only HTTP ingress through Cloudflare Tunnel is automated. DNS-based cutovers (A records, other tunnels) stay a Hook or a manual step.
- A cutover that dies mid-way is recovered from the Journal, not from memory of the operator; every step must therefore be idempotent (stopping a stopped container, an already-correct ingress).
- Tests: `internal/migrate` with a fake `Host` and an `httptest` Cloudflare server (expired token stops before any `docker stop`, claim check refuses a stray mount, PUT changes only listed hostnames, rollback refuses on drift, rerun after `done` is a no-op, no token in output); config validation; `--json` event order; an e2e under `YOHO_E2E=1`.
