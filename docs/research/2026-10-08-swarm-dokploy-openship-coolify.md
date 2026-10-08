# Swarm, Dokploy, OpenShip, Coolify (2026-10-08)

Items marked "unverified" were not confirmed against a primary source.

## Docker Swarm in 2026
- Maintained, not deprecated: Engine 29.8.2 (2026-09-30) ships steady Swarm/overlay fixes; swarmkit activity is mostly vendor bumps. Mirantis committed to Swarm support through 2030 (MKE 3, announced 2025-07-01). nftables not yet usable on Swarm nodes.
- `stack deploy` ignores: build, devices, network_mode, privileged, security_opt, restart, shm_size, pid, ipc, links, container_name, expose. Errors: extends, volumes_from, volume_driver, cpu_quota/shares/cpuset. Supported: cap_add/drop, init, sysctls, ulimits, healthcheck, deploy.*, secrets, configs. depends_on/profiles/env_file: confirm by test. Does not read `.env` → render and interpolate ourselves.
- Zero-downtime: `update_config {order: start-first, parallelism: 1, failure_action: rollback}` + healthcheck. Whether a task leaves the VIP before SIGTERM is unverified (moby#38841 open: stopping task still gets traffic in VIP mode; dnsrr switches immediately). Apps must drain on SIGTERM; longer stop_grace_period.
- Secrets: encrypted in Raft, tmpfs `/run/secrets`, 500 KB, immutable → content-hashed names for rotation.
- Images: every node that may run a task needs the image. No registry → per-node `pussh` or `save|load` fan-out; `--resolve-image never`.
- Stateful: `placement.constraints` + `stop-first`; local volumes are per node.

## Dokploy (TypeScript)
- Apps are Swarm services via dockerode; compose has `docker compose up` and `docker stack deploy` modes; Traefik via `deploy.labels`.
- Multi-server requires a registry for image upload. Env merged project → environment → service; external vault refs (AWS, Azure, Doppler, HashiCorp, Infisical, ...).
- Backups: DB dumps + volume backups via rclone to S3. Zero-downtime: start-first + rollback; Bad Gateway without a healthcheck.

## OpenShip (oblien/openship, TypeScript, Apache-2.0)
- Desktop/web/CLI control plane driving servers over SSH; compose mode with OpenResty edge + certbot; clustering via K3s + Longhorn + managed WireGuard, not Swarm.
- Borrow: durable idempotent operation model with rollback timers; immutable digests per release; arch check; "release ready before traffic moves".

## Coolify env features
- Magic generated vars: `SERVICE_PASSWORD_<ID>` (32/64), `SERVICE_USER_`, `SERVICE_BASE64_`, `SERVICE_HEX_`, `SERVICE_FQDN_/URL_<ID>[_port]`; persisted across deploys; same name shared across services.
- Build vs runtime flag per variable; shared variables scoped team/project/environment/server via `{{environment.NAME}}`; vault refs; Literal, Multiline, Locked.
- Pitfall: all env injected into every container of a project (#7655).
