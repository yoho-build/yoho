# Yoho

**Deploy Docker Compose apps to your own servers the way Kamal deploys one container: one command, zero downtime, no platform to run.**

Yoho is one static Go binary. Your `compose.yaml` stays the source of truth; Yoho adds an `x-yoho` block per Service and a small `yoho.yml`. It builds images on your machine, ships them over SSH (no registry needed), switches traffic through kamal-proxy, keeps secrets in your password manager, and backs up volumes.

Status: **alpha**. Expect breaking changes. License: MIT.

## Install

```sh
curl -fsSL https://yohobuild.com | sh
```

The installer downloads the binary for this OS and architecture from GitHub Releases, verifies the SHA-256 in `checksums.txt`, and installs to `/usr/local/bin` when that directory is writable, otherwise `~/.local/bin`. It does not use sudo. Pin a release with `YOHO_VERSION=v0.1.0` or choose a directory with `YOHO_INSTALL_DIR`. If the repository is private, set `GITHUB_TOKEN` or `GH_TOKEN` before running the installer.

From source (Go 1.26): `go install github.com/yoho-build/yoho/cmd/yoho@latest`.

## 60-second quickstart

```sh
yoho init                 # yoho.yml, .yoho/secrets, sample hooks
$EDITOR yoho.yml compose.yaml
yoho config check         # validate config, compose and secret refs
yoho setup                # interactive: Docker, deploy user, firewall (Debian/Ubuntu)
yoho plan                 # show what would change on the Server (read-only)
yoho apply                # confirm, then build, ship, health-gated zero-downtime cutover
```

Then `yoho app logs -f`, `yoho releases`, `yoho rollback <version>`.

## Features

- Compose-native: `docker compose up` still works locally; Yoho reads `x-yoho` and ignores nothing else.
- Zero downtime: health-gated, drained cutover via kamal-proxy, also behind Cloudflare Tunnel.
- Registry-less: local build (Apple `container` on Apple silicon, Docker elsewhere), amd64 cross-builds, layer push over SSH.
- Secrets never in the repo, image, or `docker inspect`: resolved locally from 1Password, Bitwarden, or any command; delivered as files; generated secrets on the Server.
- Stateful Services are pinned and never `down -v`.
- Backups (restic, S3, B2, SFTP, rclone, or encrypted archives) and restore; opt-in scheduled jobs via systemd timers.
- Cloudflare Tunnel, including account-less Quick Tunnels.
- Compose (default) or Swarm runtime.
- Kamal-compatible hooks and secrets-file style.
- `yoho setup` provisions a Debian/Ubuntu Server.
- AI-native: JSON Schema, `--json` NDJSON output, and a built-in agent skill (`yoho skill install`).

## Compared

| | Kamal | ONCE | Coolify | Yoho |
|---|---|---|---|---|
| Unit of deploy | container per role | single app | compose / apps | compose project |
| Runs where | CLI (Ruby) over SSH | on the host | web platform + DB | CLI (Go) over SSH |
| Zero downtime | kamal-proxy | kamal-proxy | partial | kamal-proxy |
| Registry required | no (since 2.8) | n/a | yes | no |
| Secrets | `.kamal/secrets` | env | UI, injected into every container | `.yoho/secrets`, per-Service files |
| Backups | no | yes | DB to S3 | volumes + dumps, restic/S3/B2/rclone |
| Server provisioning | Docker only | installer | installer | `yoho setup` |
| Multi-server | yes (roles) | no | yes | Swarm runtime; Roles later |
| Platform to run | no | service | yes | no |

## Config example

```yaml
# yoho.yml
app: shop
servers:
  primary: { ssh: yoho@203.0.113.10 }
destinations:
  production: { servers: [primary] }
builder: { location: local, secrets: [NPM_TOKEN] }
backups:
  targets:
    offsite: { type: restic, repository: "s3:s3.amazonaws.com/my-backups/shop", password_secret: BACKUP_PASSWORD, env_secrets: [AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY], keep_last: 14 }
  jobs:
    nightly: { destination: production, target: offsite, schedule: "*-*-* 03:00:00" }
```

```yaml
# compose.yaml
services:
  web:
    build: .
    healthcheck: {test: ["CMD", "wget", "-qO-", "http://127.0.0.1:3000/up"], interval: 5s}
    x-yoho:
      proxy: { hosts: [shop.example.com], port: 3000 }
      secrets: [DATABASE_PASSWORD:POSTGRES_PASSWORD, SECRET_KEY_BASE]
      release_command: ["bin/rails", "db:migrate"]
  db:
    image: postgres:17
    environment: { POSTGRES_PASSWORD_FILE: /run/secrets/POSTGRES_PASSWORD }
    volumes: [pgdata:/var/lib/postgresql/data]
    x-yoho:
      stateful: true
      generate: { POSTGRES_PASSWORD: password32 }
      backup: { dump: [pg_dump, -U, postgres, postgres], volumes: [pgdata] }
volumes: { pgdata: {} }
```

A proxied Service needs that compose `healthcheck` (an error under swarm, a warning under compose). The image needs wget or curl.

See `examples/basic` for a fuller file.

## Commands

| Command | Purpose |
|---|---|
| `init`, `config check\|show`, `schema [--ext]` | create and validate config |
| `setup` | provision a Server |
| `plan`, `apply`, `deploy`, `releases`, `rollback VERSION` | review and release lifecycle |
| `app ps\|logs\|exec` | inspect the running App |
| `proxy boot\|status\|logs`, `tunnel up\|status\|down` | Proxy and Cloudflare Tunnel |
| `secrets list\|print` | inspect secrets |
| `backup run\|list\|restore`, `schedule install\|status\|remove` | Backups and Scheduled Jobs |
| `swarm init\|join\|status` | Swarm runtime |
| `builder status\|setup` | local Builder |
| `skill install\|print` | agent skill |

Global flags: `-c/--config`, `-d/--destination` (default: `$YOHO_DESTINATION`, then `production`, then the only Destination), `--json`, `-v/--verbose`.

## Docs

Source in `docs/site` (MkDocs Material, `mkdocs serve`). Design: `VISION.md`, `GLOSSARY.md`, `docs/adr/`.
