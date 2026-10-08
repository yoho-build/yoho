# Decision log (grilling session 2026-10-08)

Small decisions that don't warrant an ADR. ADRs live in `docs/adr/`.

- Name: **yoho**. Binary `yoho`, config dir `.yoho/`, env prefix `YOHO_*`. `yoho.sh` unregistered (whois), `yoho.dev` taken.
- Config split: per-Service settings in compose `x-yoho`; where/how settings (Servers, Destinations, Builders, transport, secrets, provisioning, Backup Targets) in the Yoho file. Revisit later if needed.
- Config formats: YAML, TOML, JSON, JSONC, detected by content. JSON Schema generated from Go structs, published for editors and AI agents.
- Docs site built from the same repo (GitHub Pages).
- Provisioning v1: interactive `setup` that installs Docker + compose plugin and basics. Debian/Ubuntu first.
- One Server per Destination in v1. Roles later (see backlog).
- Builders: local, dedicated build Server, or the Destination's Server.
- Image transport: SSH layer push (unregistry-style) → `docker save | docker load` → registry.
- Proxy: kamal-proxy, ports configurable, see ADR 0004.
- Backups: ONCE contract (pre-backup hook or pause, post-restore). Targets: local, S3, B2, SFTP, rclone remotes (OneDrive, Google Drive). Archive formats incl. password-protected zip / 7z; password readable from a password manager.
- Agent skill ships in v1: "Yoho is like Kamal", built-in env vars, hook names. Coolify and Kamal migration is an agent-skill guide (Coolify CLI or SSH), not a command.
- Kamal-compatible hook names and secrets-file style, but under `.yoho/`.
- Scheduler: opt-in per Server, systemd timer per job, CLI-only by default (ADR 0003).
- Stateful Services pinned, `servers` always a list (ADR 0005).
- On-Server layout `/var/lib/yoho/<app>/<destination>/`, compose project `yoho-<app>-<destination>`, per-Server compiled compose, never `down -v`.
- Zero-downtime in one compose project: `--scale <svc>=2 --no-recreate` → kamal-proxy cutover → remove old.
- Secrets as files (ADR 0006); env_file fallback; `${VAR}` secret interpolation rejected by `yoho config check`; audit via ref + HMAC; `--reveal` TTY only.
- Backups (decided by Claude, user delegated): restic default (encrypted, dedup; S3/B2/SFTP/rclone); optional archive mode tar.gz / AES-256 zip / 7z, never ZipCrypto; passwords from password manager at schedule-install time, stored on Server.
- `yoho setup` v1 (delegated): interactive, shows each change before applying: Docker + compose plugin, deploy user + SSH key, ufw (SSH + Proxy ports), unattended-upgrades, optional swap/timezone/Proxy.
- License MIT (delegated). GitHub repo `yoho-build/yoho`; domains yohodev.com + yoho.sh available, user to register.
- Generated secrets (Coolify-style): `x-yoho.generate`, created once on the Server, `yoho secrets backup` to password manager, included in encrypted Backups.
- Each Service gets only secrets it declares in `x-yoho.secrets`, with aliasing (container name ← secrets key). Common `.yoho/secrets` + `.yoho/secrets.<destination>`.
- Version = git SHA, `_uncommitted_<rand>` suffix with warning; image tag = version; Release stores digests.
- Migrations: `x-yoho.release_command` in a one-off container from the new image before cutover; failure aborts. Pre-migration Backup off by default.
- Rollback: redeploy a previous Release (digests + compose), zero-downtime; volumes and migrations untouched, warned. Keep last 5 Releases per Server.
- Swarm (`docker stack deploy`) supported from the start, not compose-only. Details pending research.
- Reference projects to study: Coolify, Dokploy, OpenShip.
- Runtime: compose default, swarm opt-in, switch = downtime migration (ADR 0007). Swarm zero-downtime via start-first + VIP; `strict_drain` option.
- Swarm images: built locally, pushed to each node over SSH in parallel; registry optional.
- Secret refs: map `{NAME: KEY}` and Kamal-style list `["KEY", "NAME:KEY"]`.
- Fast health probes: when Yoho waits on a healthcheck (proxied compose cutover, or any Swarm service healthcheck) and the user set neither `start_period` nor `start_interval`, and the server Docker is 25 or newer, the compiled file sets `start_period: 60s` and `start_interval: 1s` so the first probes run every second while the container starts. User values are kept. Older engines omit both, because they reject `start_interval`.
- Swarm update width: when `deploy.update_config.parallelism` is unset, it defaults to the replica count (all new tasks start together, still start-first) and `monitor: 5s` unless set; the same defaults apply to `rollback_config`. `failure_action` stays `rollback`. One-at-a-time plus a 30s monitor was most of a two-replica deploy.
