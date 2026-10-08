# Landscape: compose-first Go deploy CLI (2026-10-08)

Facts from GitHub API and official docs on 2026-10-08. Items marked "(memory)" not re-verified.

## Kamal (basecamp/kamal) — v2.12.0, Ruby
- Config: `config/deploy.yml` + `deploy.<dest>.yml` (`-d dest`). One container per role per host (`docker run`), no compose.
- Builder: local default; `remote: ssh://host`; multi-arch via buildx; `builder.secrets` → buildx `--secret` (`RUN --mount=type=secret`).
- Registry-less since 2.8: `registry.server: localhost:5555` runs a local registry and SSH-forwards it to servers.
- kamal-proxy (Go, v0.10.2): `kamal-proxy deploy <svc> --target host:port` → health check, cutover, drain, Let's Encrypt TLS.
- Hooks (`.kamal/hooks/`): docker-setup, pre-connect, pre-build, pre-deploy, post-deploy, pre-app-boot, post-app-boot, pre-proxy-reboot, post-proxy-reboot. Env `KAMAL_*`. Non-zero exit aborts.
- Secrets: `.kamal/secrets` dotenv with `$(...)`; `kamal secrets fetch/extract`; adapters 1password, bitwarden, bitwarden-sm, lastpass, aws, gcp, doppler, passbolt. Runtime via `env.secret`.
- Lock, rollback (needs retained container, `retain_containers` default 5), aliases, `setup` installs Docker.
- Pain: no compose, accessories second-class, no backups, Ruby runtime.

## Basecamp ONCE (basecamp/once) — v0.3.4, Go
- Single-host installer + TUI + background service; not SSH-remote.
- App contract: HTTP :80, `/up` healthcheck, data in `/storage`, optional `/hooks/pre-backup`, `/hooks/post-restore`.
- Runs kamal-proxy standalone for zero-downtime + TLS.
- Backup: pre-backup hook, else `docker pause`; tar.gz of volume; retention trim; restore via helper container + post-restore hook.
- 0.3.4 fixed privilege escalation in backup dir (GHSA-jqwc-wmh4-9qw3) → backup dir permissions matter.

## Prior art
| Tool | Lang | Compose | Registry-less | Zero-downtime | Secrets | Backups |
|---|---|---|---|---|---|---|
| docker-rollout | sh | yes (plugin) | no | scale-up trick, needs dynamic proxy | – | – |
| Uncloud | Go | yes (primary) | yes (unregistry) | rolling | compose | – |
| unregistry / `docker pussh` | Go | n/a | yes, layer-diff over SSH; needs containerd image store | n/a | n/a | n/a |
| Sidekick | Go | no | yes (memory) | Traefik | age (memory) | – |
| Haloy | Go | no | ? | yes | ? | – |
| Coolify / Dokploy | PHP / TS | yes | no | partial / Swarm | UI | DB→S3 |
| Komodo / Portainer | Rust / Go | stacks | no | no | UI | – |
| Swarm stack | built-in | subset | no | start-first | docker secrets | – |
| `docker context` ssh | built-in | yes | no image transfer | no | local env | – |

## Go building blocks
- SSH: shell out to `ssh` + ControlMaster (honors ~/.ssh/config, agent, ProxyJump, 1Password agent) vs `golang.org/x/crypto/ssh` (pure Go, must reimplement config).
- Compose: `compose-spec/compose-go/v2` for parsing/validation and `x-` extensions; shell out to remote `docker compose` rather than embedding `docker/compose` (heavy, major-version churn).
- Build secrets: `buildx --secret id=X,env=X`; compose `build.secrets` + top-level `secrets: {X: {environment: X}}`.
- Runtime secrets: compose `secrets:` file → `/run/secrets/X` (non-Swarm needs file on host), or env.
- Providers: `op read/inject/run`, `bw get` (BW_SESSION), `bws secret get/run`, SOPS+age offline.

## Zero-downtime with compose
1. docker-rollout scale-up: scale 2N `--no-recreate`, wait healthy, remove old; no `container_name`/host `ports:`.
2. Label proxy (Traefik / caddy-docker-proxy).
3. kamal-proxy as a service, CLI calls `kamal-proxy deploy` per web service. Most attractive.
4. Swarm `start-first`.

## Backups
- Volume tar via helper container; ONCE pause/hook pattern; DB dumps via `compose exec`; restic (dedup, encrypted, S3/B2/SFTP).

## Gaps / opportunity
1. compose.yaml as source of truth + small deploy overlay (`x-deploy` or sibling file), plain Docker hosts over SSH, no cluster daemon.
2. Image transport ladder: pussh (layer diff) → tunneled local registry → save|load → real registry; plus build-on-server.
3. Zero-downtime via kamal-proxy for web services; stateful services recreate with stop+backup.
4. Kamal-style secrets adapters, compose-aware: build-time → buildx secrets, runtime → env or tmpfs `/run/secrets`.
5. First-class per-volume backup/restore with ONCE hook contract, DB-dump hooks, restic/S3, 0700 root-owned dirs.
6. Kamal polish in one static binary: lock, hooks, rollback by recorded release (compose digest + image digests on host), destinations, aliases, setup, audit log.

## Zero-downtime behind cloudflared (source-read, not tested)
- cloudflared: per-rule Go http.Transport, keepAliveTimeout 90s, connectTimeout 30s; DNS resolved per new dial, no cache; no origin retry → 502 on dial/RoundTrip error. `--grace-period` drains on SIGTERM. Replicas: 4 edge conns each, edge fails over between replicas.
- compose recreate: 502 gap. Scale-up/remove (shared alias): near zero, small 502 risk (no retry, stale pooled conns). Network-disconnect-first is worse (stalls established conns).
- Robust: cloudflared → long-lived kamal-proxy (HTTP, `--host`) → app. Stable origin for cloudflared; kamal-proxy health-gates `/up` and drains. Same as ONCE. 2 cloudflared replicas, serial upgrade, `stop_grace_period` ≥ grace period.
- Cloudflare API ingress blue/green: not recommended (API dependency, uneven propagation).

## ONCE background service
- `once background install` (root): systemd unit / launchd plist running `once background run`; 5-min tick; per app auto-update + auto-backup every 24h, failed tasks retried each tick; self-update unless `ONCE_NO_SELF_UPDATE`.
- State: app settings as JSON in container label `once`; task state `once-state.json` in proxy volume. Multi-app per host; one kamal-proxy per namespace; deploy via `docker exec proxy kamal-proxy deploy <app> --target <id> --deploy-timeout 120s [--host] [--tls]`; new container → proxy deploy → remove old.

## Kamal built-in env vars
- Hooks: KAMAL_RECORDED_AT, KAMAL_PERFORMER, KAMAL_DESTINATION, KAMAL_VERSION, KAMAL_SERVICE_VERSION, KAMAL_SERVICE, KAMAL_HOSTS, KAMAL_ROLES, KAMAL_LOCK, KAMAL_COMMAND, KAMAL_SUBCOMMAND, KAMAL_RUNTIME (post-deploy); secrets merged for pre-connect/pre-deploy/post-deploy.
- Hook names: docker-setup, pre-connect, pre-build, pre-deploy, post-deploy, pre-app-boot, post-app-boot, pre-app-remove, post-app-remove, pre-proxy-reboot, post-proxy-reboot.
- App container: KAMAL_CONTAINER_NAME, KAMAL_VERSION, KAMAL_HOST, KAMAL_DESTINATION.

## Name check
- DaoFlow: `DaoFlow-dev/DaoFlow` is the user's own org. daoflow.dev/.io/.com registered; daoflow.sh free (whois). nova: collides with OpenStack CLI, drop.
- Alternatives: davit (davit.sh free), zhou (zhou.sh free), berth (.sh taken).

## User's existing deploys (scan of ~/Developer, 2026-10-08)
- 4 small Ansible playbooks (auth-rockiestar, twilar-app-update-server, core-translator, leet-mgr): copy compose + plaintext `.env` (0644), rsync app dir with excludes, mkdir data dirs, `docker-compose up -d --pull always` or `up --build --remove-orphans -d` (build on server), `compose ps` check. Host e.g. Oracle Cloud ARM, Ubuntu.
- No provisioning, firewall, backups, cloudflared, secrets manager in Ansible. Sep 2026 moved to Coolify (`/Volumes/SandE/backup-prod-all-sep-7-2026/`, `docker-compose.coolify.yml` files).

## Roles / stateful / secrets / schedulers prior art (2026-10-08)
- Pinning: Kamal accessories `host:`; Uncloud `x-machines`; Swarm placement constraints. None guard against duplicated state. Coolify refuses multi-server for apps with persistent storage (only explicit guard found).
- Cross-server: Kamal = user-provided IPs; Uncloud = WireGuard mesh + DNS; Komodo = one stack per server.
- Kamal secrets: per-role env file on host, `docker run --env-file` → visible in `docker inspect`. Coolify injects all env into every container of a project (#7655).
- Compose non-Swarm `secrets:` file = read-only bind mount; `uid/gid/mode` ignored → chown to container UID. `${VAR}` interpolation leaks into `docker compose config` (#9160).
- 1Password service accounts: hourly + shared daily read caps; crash-looping `op` can lock all service accounts ~24h. Fetch client-side once per deploy.
- Schedulers: ofelia (one per host, needs socket; use socket proxy), offen/docker-volume-backup (per project, stop-during-backup labels, GPG/age), resticker, supercronic.
- Consults: `docs/consult/2026-10-08-sol.md`, `docs/consult/2026-10-08-grok.md`.
- Full scan (background, finished later): ~15 more playbooks, same pattern. twilar-services (auth-go, core-system, website-icon-go, uptime-kuma, otel-service prod+staging), rockie-star-services (community, minio-backup), blog-wordpress, legacy Rails/pomodoly. Invoked from Makefile `ansible-playbook -i deployment/hosts.ini deployment/prod.yml`.
- Two modes seen: rsync source (exclude .git/venv) + `up --build --remove-orphans -d` on server; or copy compose + .env, `pull`, down, up (not zero-downtime). otel staging swaps `.env.staging` → `.env`. No Vault.
