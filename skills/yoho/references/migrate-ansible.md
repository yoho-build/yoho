# Migrate from Ansible rsync + compose

Typical playbook: install Docker, rsync the repo to `/opt/app`, template a `.env`, run `docker compose up -d --build`. Yoho replaces all of it.

| Playbook step | Yoho |
|---|---|
| Docker install, users, ufw, unattended-upgrades, swap | `yoho setup` and `setup:` in `yoho.yml` |
| rsync source + `compose up --build` on the Server | `builder.location: server` (syncs source, builds on the Server) |
| Build locally and `docker save \| ssh docker load` | `builder.location: local` (default); `transport.mode: auto` |
| Templated `.env` with secrets (plaintext on disk) | `.yoho/secrets` + `x-yoho.secrets`, delivered as files |
| `docker compose up -d` (downtime) | `yoho deploy` (health-gated kamal-proxy cutover) |
| cron for backups | `backups:` + `yoho schedule install` |
| group_vars per environment | `destinations.<name>.env` and `.yoho/secrets.<destination>` |

Steps:

1. Keep the project's `compose.yaml`; remove host `ports:` and `container_name:` for proxied Services (the Proxy publishes ports and routes by host).
2. `yoho init`; set `servers.primary.ssh` from the inventory, `destinations` from inventory groups.
3. Choose the Builder: `server` if the Server has the CPU and you built there before, `local` otherwise (cross-builds for `linux/amd64` via Apple `container` on Apple silicon or Docker buildx).
4. Convert the `.env` template: secrets to the password manager and `.yoho/secrets`; the rest to `environment:` or `destinations.<name>.env`.
5. Databases: `x-yoho.stateful: true`, `x-yoho.backup`. Reuse the existing volume names (check `docker compose config` project name: Yoho uses `yoho-<app>-<destination>`, so set explicit `name:` on volumes to keep the old ones).
6. Migrations: `x-yoho.release_command`.
7. Stop the old compose project (`docker compose stop`, not `down -v`) right before the first `yoho deploy` if ports collide.
