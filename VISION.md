# Yoho vision

**Deploy Docker Compose apps to your own servers the way Kamal deploys one container: one command, zero downtime for proxied Services on normal deploys, no platform to run.**

## Why

Self-hosting today means choosing between:

- **Kamal**: great CLI and zero-downtime proxy, but one container per role, no Compose, no backups, and it needs Ruby.
- **ONCE**: great backups and a simple app contract, but single-host and not for your own apps over SSH.
- **Coolify / Dokploy**: Compose support, but a web platform with its own database to run, secure, and back up.
- **Ansible playbooks**: flexible, but every project ends up with a copy of the same rsync + `docker compose up` with downtime and a plaintext `.env`.

Yoho is one static Go binary that covers all of that for people who already describe their apps in `compose.yaml`.

## Principles

1. **Compose is the source of truth.** Yoho reads your compose file and adds only an `x-yoho` block per Service. `docker compose up` still works locally.
2. **CLI first, agentless by default.** Nothing runs on the Server except Docker (and kamal-proxy for proxied Services). Local read-only dashboards (`yoho tui` / `yoho ui`) run on your machine over SSH; Scheduled Jobs, the Cloudflare Tunnel connector, and a future per-Server alerting API are opt-in per Server.
3. **Zero downtime for proxied Services on normal deploys.** Proxied Services are health-gated and drained through kamal-proxy, including behind Cloudflare Tunnel. Workers restart stop-first on the compose runtime; Swarm defaults stateless Services to start-first rolling updates. Stateful Services restart stop-first on both runtimes. Upgrading the shared Proxy causes a brief (~1 s) interruption.
4. **File-based secrets by default.** They're resolved on your machine from 1Password or Bitwarden, delivered as files, and audited by fingerprint. Values stay out of the repo, image, and `docker inspect` in this mode. Opt-in `secrets_as_env: true` delivers environment variables, which `docker inspect` can expose.
5. **Data is protected by design.** Stateful Services are pinned, never duplicated, never `down -v`. Backups follow ONCE's contract and go to restic, S3, B2, SFTP, or any rclone remote.
6. **No registry required.** Build locally (Apple `container` on Apple silicon, Docker elsewhere), cross-build for x86 servers, ship over SSH.
7. **Feels like Kamal, does more.** Familiar commands, hook names, and secrets files; clearer output, actionable hints, `--json` for agents.
8. **AI-native.** JSON Schema for every config, NDJSON output, and a shipped agent skill so coding agents can operate Yoho correctly.

## Scope

**In scope (shipped in beta):** single-Server Compose and opt-in Roles for multiple Servers; the frozen Swarm runtime; local, remote, or on-Server builds; kamal-proxy (including on-demand TLS for customer domains); Cloudflare Tunnel connector (including Quick Tunnels and ingress routing); secrets via op / bw / bws / any command, delivered as files; generated secrets and generated-secret rotation; release commands and Service checks; rollback; Hooks; Backups, restore, and restore drills (`yoho backup drill`); opt-in Scheduled Jobs; interactive `yoho setup` for Debian / Ubuntu; local read-only dashboards (`yoho tui` and `yoho ui`, ADR 0010); scoped team access through the Gate (ADR 0011); one-shot Migration Cutover (ADR 0012); preview environments (ADR 0015); and an agent skill with Kamal, Coolify, Dokploy, and Railway migration guides.

**Multi-Server:** opt-in Roles place subsets of Services on subsets of Servers (ADR 0017). Swarm is frozen for bug/security fixes and CI. An optional WireGuard mesh is independent of Roles.

**Later:** managed Cloudflare Tunnel and DNS creation, a remote read-only dashboard API with health, metrics, and alerts, and SOPS / age.

**Not planned:** a web control plane, a cluster daemon, Kubernetes.

## Success looks like

- Moving an existing Ansible / Coolify app takes minutes and an agent can do it from the skill.
- A deploy behind Cloudflare Tunnel drops zero requests.
- Losing a Server means `yoho setup`, `yoho deploy`, `yoho backup restore`, and nothing else.
