# Yoho vision

**Deploy Docker Compose apps to your own servers the way Kamal deploys one container: one command, zero downtime, no platform to run.**

## Why

Self-hosting today means choosing between:

- **Kamal**: great CLI and zero-downtime proxy, but one container per role, no Compose, no backups, and it needs Ruby.
- **ONCE**: great backups and a simple app contract, but single-host and not for your own apps over SSH.
- **Coolify / Dokploy**: Compose support, but a web platform with its own database to run, secure, and back up.
- **Ansible playbooks**: flexible, but every project ends up with a copy of the same rsync + `docker compose up` with downtime and a plaintext `.env`.

Yoho is one static Go binary that covers all of that for people who already describe their apps in `compose.yaml`.

## Principles

1. **Compose is the source of truth.** Yoho reads your compose file and adds only an `x-yoho` block per Service. `docker compose up` still works locally.
2. **CLI first, agentless by default.** Nothing runs on the Server except Docker. Scheduled Backups and future dashboards are opt-in, per Server.
3. **Zero downtime is the default experience.** Proxied Services are health-gated and drained through kamal-proxy, including behind Cloudflare Tunnel.
4. **Secrets never touch the repo, the image, or `docker inspect`.** They're resolved on your machine from 1Password or Bitwarden, delivered as files, and audited by fingerprint.
5. **Data is protected by design.** Stateful Services are pinned, never duplicated, never `down -v`. Backups follow ONCE's contract and go to restic, S3, B2, SFTP, or any rclone remote.
6. **No registry required.** Build locally (Apple `container` on Apple silicon, Docker elsewhere), cross-build for x86 servers, ship over SSH.
7. **Feels like Kamal, does more.** Familiar commands, hook names, and secrets files; clearer output, actionable hints, `--json` for agents.
8. **AI-native.** JSON Schema for every config, NDJSON output, and a shipped agent skill so coding agents can operate Yoho correctly.

## Scope

**v1:** one Server per Destination on the compose runtime; Swarm runtime for multiple Servers; local, remote, or on-Server builds; kamal-proxy; secrets via op / bw / bws / any command; generated secrets; release commands; rollback; Hooks; Backups and restore; opt-in Scheduled Jobs; interactive `yoho setup` for Debian / Ubuntu; agent skill with Kamal, Coolify, and Dokploy migration guides.

**Later:** Roles (subsets of Services on subsets of Servers), managed Cloudflare Tunnel, a read-only dashboard API with health, metrics, and alerts, SOPS / age.

**Not planned:** a web control plane, a cluster daemon, Kubernetes.

## Success looks like

- Moving an existing Ansible / Coolify app takes minutes and an agent can do it from the skill.
- A deploy behind Cloudflare Tunnel drops zero requests.
- Losing a Server means `yoho setup`, `yoho deploy`, `yoho backup restore`, and nothing else.
