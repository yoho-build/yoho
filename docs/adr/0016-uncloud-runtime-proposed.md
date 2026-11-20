# Experimental Uncloud runtime (`runtime: uncloud`)

Status: Proposed (experimental, default off, not scheduled). Refs [#82](https://github.com/yoho-build/yoho/issues/82).

## Context

[Uncloud](https://github.com/psviderski/uncloud) provides multi-machine Docker Compose deployment over a WireGuard mesh with peer discovery and NAT traversal, built-in DNS service discovery, Caddy ingress with Let's Encrypt and cluster-shared certificates, Unregistry layer-diff image shipping, and managed `*.uncld.dev` DNS — with no central control plane, running an `uncloudd` daemon and Corrosion (CRDT) store on every machine (as of 2026-11; [Uncloud docs](https://uncloud.run/docs)).

Some users want cross-cloud, multi-region Compose operation, which Kamal-style Roles ([#81](https://github.com/yoho-build/yoho/issues/81), using private networking the operator supplies) deliberately does not cover. Building a bespoke mesh or CRDT state layer inside Yoho is out of scope ([`VISION.md`](../../VISION.md), [ADR 0003](0003-no-resident-daemon-by-default.md)).

This spike evaluates whether Yoho should offer Uncloud as an **optional, experimental** runtime backend (`runtime: uncloud`), alongside `compose` (default) and `swarm`, by answering four questions against Uncloud's architecture and Yoho's existing ADRs ([0003](0003-no-resident-daemon-by-default.md), [0004](0004-kamal-proxy-for-zero-downtime.md), [0006](0006-secrets-resolved-locally-delivered-as-files.md), [0011](0011-scoped-access-through-a-gate.md), [0012](0012-one-shot-migration-cutover.md)).

## 1. Contract reuse

Can `runtime: uncloud` reuse Yoho's existing runtime contracts?

| Contract | Reuse? | Details / Adapter required |
| --- | --- | --- |
| **Plan (`yoho plan` / `plan.Deploy`)** | **With adapter** | Yoho's `plan.Deploy` can drive an Uncloud adapter that translates compiled Compose services and `x-yoho` metadata into Uncloud's Compose schema and diffs against live Corrosion state via `uc` (or its API) instead of Docker Compose or Swarm. Drift detection for Caddy routes and mesh placement would be Uncloud-specific. |
| **Secrets (file delivery, [ADR 0006](0006-secrets-resolved-locally-delivered-as-files.md))** | **No** | Uncloud delivers secrets as environment variables only and does not support file mounts. Supporting file-delivered secrets would either break [ADR 0006](0006-secrets-resolved-locally-delivered-as-files.md) (forcing `secrets_as_env: true`, exposing values in `docker inspect`) or require Yoho to stage secret files over SSH on every node outside Uncloud's orchestration, defeating Uncloud's scheduling model. |
| **Release record (`release.Release`)** | **With adapter** | Uncloud has no Release history. Yoho would have to persist `release.json` and compiled Compose files itself (on a designated primary Server or locally) and map Uncloud's deployed image digests back into the Release record. |
| **Rollback (`yoho rollback`)** | **Partial / with adapter** | Uncloud performs per-container rollback only when a health check fails during a deploy and keeps no Release history. Yoho's `yoho rollback VERSION` would have to re-deploy a stored Release's compiled Compose and digests through the Uncloud adapter, without atomic multi-Service parity or Gate vetting. |
| **Backups, restore, and drills** | **With adapter (limited)** | Uncloud has no built-in volume backup or restore. Yoho could run its existing SSH Backup and restore scripts against the single node pinned to a Stateful Service (similar to Swarm's first-Server pinning), but `yoho backup drill` would remain refused on Uncloud (requiring a separate Compose Destination via `restore --from`). |
| **Hooks** | **Yes (mostly)** | Hooks run locally on the operator's machine around CLI phases, so `pre-connect`, `pre-build`, `pre-deploy`, `pre-app-boot`, `post-app-boot`, `post-deploy`, and Backup Hooks work unchanged. `pre-proxy-reboot` and `post-proxy-reboot` do not apply because Caddy replaces kamal-proxy. |

## 2. Features refused under `runtime: uncloud`

If `runtime: uncloud` were added, it would form a new column in the [support matrix](../site/support-matrix.md) with substantial refusals:

| Capability | `runtime: uncloud` (proposed, experimental) | Reason |
| --- | --- | --- |
| **Server-side daemon** | Required (`uncloudd` + Corrosion on every Server) | Explicit opt-in exception to [ADR 0003](0003-no-resident-daemon-by-default.md). |
| **Ingress & TLS** | Caddy (Uncloud-managed); kamal-proxy disabled | Replaces [ADR 0004](0004-kamal-proxy-for-zero-downtime.md). `x-yoho.proxy.custom_domains` on-demand TLS check, `proxy upgrade`, `proxy.metrics`, and Cloudflare Tunnel-to-kamal-proxy cutover are refused. |
| **File-delivered secrets** | **Refused** (env vars only) | Conflicts with [ADR 0006](0006-secrets-resolved-locally-delivered-as-files.md); Uncloud has no secret file mounts. |
| **Scoped team access (Gate)** | **Refused** | Uncloud has no equivalent of the Gate ([ADR 0011](0011-scoped-access-through-a-gate.md)). |
| **One-shot Migration Cutover** | **Refused** | Compose + operator only ([ADR 0012](0012-one-shot-migration-cutover.md)). |
| **Preview environments** | **Refused** | Compose + operator only ([ADR 0015](0015-preview-destinations.md)). |
| **Restore drills (`backup drill`)** | **Refused** | Compose only; drill into a Compose Destination with `restore --from`. |
| **Generated-secret rotation (`--generated`)** | **Refused** | Compose only; requires direct container execution on the pinned Stateful Service. |
| **Service checks (`x-yoho.check`)** | **Refused** | Requires dialing container IPs over the local Docker project network. |
| **Destination `subnet`, `app_dir`, App-dir bind mounts** | **Refused** | Managed by Uncloud's mesh and remote node placement. |

## 3. Maintenance cost

- **Test matrix growth:** Adding a third runtime (`compose`, `swarm`, `uncloud`) expands config validation, Compose checks, plan/diff, deploy/rollback, and E2E suites. Testing Uncloud meaningfully requires multi-node WireGuard + `uncloudd` + Corrosion fixtures in CI.
- **Upstream API stability:** Uncloud is actively evolving. Its Compose extensions, `uc` CLI flags, and `uncloudd` / Corrosion internals are not a frozen wire contract, so Yoho would absorb breaking changes on every upstream upgrade.
- **Version pinning and daemon lifecycle:** Because `uncloudd` and Corrosion run permanently on every Server, Yoho would need to pin, check, and coordinate upgrades of an external cluster daemon (an ongoing operational surface [ADR 0003](0003-no-resident-daemon-by-default.md) explicitly avoids). Users would also report WireGuard, NAT traversal, and CRDT convergence issues to Yoho that Yoho cannot fix directly.

## 4. Recommendation: No-go for now

**Decision:** Do **not** implement `runtime: uncloud` now. Keep this ADR in `Proposed` status (not scheduled).

- **Why no-go:** Too many of Yoho's core guarantees — agentless Servers ([ADR 0003](0003-no-resident-daemon-by-default.md)), kamal-proxy cutover including behind Cloudflare Tunnel ([ADR 0004](0004-kamal-proxy-for-zero-downtime.md)), file-delivered secrets ([ADR 0006](0006-secrets-resolved-locally-delivered-as-files.md)), scoped Gate access ([ADR 0011](0011-scoped-access-through-a-gate.md)), and Migration Cutover ([ADR 0012](0012-one-shot-migration-cutover.md)) — would be disabled or degraded under Uncloud. Maintaining a third runtime that turns off half of Yoho's feature set is a poor trade-off while Yoho is still in beta.
- **What to tell users today:** If you need a cross-cloud, multi-machine WireGuard mesh today, use [Uncloud](https://github.com/psviderski/uncloud) directly (documented in [Compare](../site/compare.md)).
- **When to revisit:** Re-evaluate only after multi-Server Roles ([#81](https://github.com/yoho-build/yoho/issues/81)) and an opt-in WireGuard mesh ([#86](https://github.com/yoho-build/yoho/issues/86)) exist and real user demand appears for Uncloud-specific capabilities (CRDT state, Caddy cluster-shared certificates, `*.uncld.dev`) that Yoho's own Roles + mesh do not satisfy.
- **Guardrail if revisited:** If ever accepted in the future, `runtime: uncloud` must stay **off by default**, be marked experimental, and require an explicit `runtime: uncloud` opt-in on the Destination.
