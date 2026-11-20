<p align="center"><img src="docs/assets/logo.svg" alt="Yoho" width="120"></p>

# Yoho

[English](README.md) · [繁體中文](README-ZH-Hant.md) · [Français](README-FR.md) · [Deutsch](README-DE.md) · [Español](README-ES.md)

**Deploy Docker Compose apps to your own servers over SSH: one binary, health-gated cutovers for proxied Services, file-delivered secrets, verified restore drills, no platform to run.**

Yoho is one static Go binary. Your `compose.yaml` stays the source of truth; Yoho adds an `x-yoho` block per Service and a small `yoho.yml`. It builds images on your machine, ships them over SSH (no registry needed), switches traffic through kamal-proxy, keeps secrets in your password manager, and backs up volumes.

Status: **beta**. Expect breaking changes. License: MIT.

## Set up with your AI agent

No manual install needed. Your coding agent (Claude Code, Codex, Cursor, ...) reads the Yoho skill, installs the CLI, writes the config, and asks before it touches any Server.

Paste this into your agent, in the repo you want to deploy:

```text
Read https://raw.githubusercontent.com/yoho-build/yoho/main/skills/yoho/SKILL.md with curl (not a summarizing fetch tool) and follow its "First-time setup" section to deploy this repo with Yoho. Ask me before installing anything, editing files, or connecting to a server.
```

The agent will:

1. Ask what you want (new deploy, migrate from Kamal, Coolify, Dokploy, Railway or Ansible, or operate an existing App) and which Server (`user@host`).
2. Install `yoho` if missing, after asking. No sudo.
3. Run `yoho init` and write `yoho.yml`, `x-yoho` and `.yoho/secrets` references. No secret values in the repo.
4. Run `yoho config check`, then `yoho setup --plan`. `yoho setup` is interactive, so you run it (or approve `-y`).
5. Run `yoho plan` and show it. Nothing is deployed until you approve `yoho apply`.
6. Verify the health URL, `yoho app ps` and `yoho releases`, and tell you the rollback command.

Keep the skill installed so later sessions know Yoho without the prompt:

```sh
npx skills add yoho-build/yoho    # skills CLI; the skill lives at skills/yoho/SKILL.md
yoho skill install                # after installing the CLI; --agent claude|codex|all
```

## Install

```sh
curl -fsSL https://yohobuild.com/install.sh | sh
```

The installer downloads the binary for this OS and architecture from GitHub Releases, verifies the SHA-256 in `checksums.txt`, and installs to `/usr/local/bin` when that directory is writable, otherwise `~/.local/bin`. It does not use sudo. Pin a release with `YOHO_VERSION=v0.1.0` or choose a directory with `YOHO_INSTALL_DIR`.

For optional signed binary provenance verification, install a current GitHub CLI (`gh`) and use `curl -fsSL https://yohobuild.com/install.sh | YOHO_VERIFY_PROVENANCE=1 sh`. This requires a matching release attestation and fails closed; older unattested releases still work in the default checksum-only mode. See [installation verification and authentication](docs/site/getting-started.md#signed-binary-provenance).

From source (Go 1.26): `go install github.com/yoho-build/yoho/cmd/yoho@latest`.

## 5-minute quickstart

From a blank Debian/Ubuntu Server to a live App (web + worker + Postgres), then a zero-downtime redeploy and a secret change: [docs/site/quickstart.md](docs/site/quickstart.md). The matching App is [`examples/quickstart`](examples/quickstart). Moving that App off Railway: [docs/site/migrating-from-railway.md](docs/site/migrating-from-railway.md).

## 60-second quickstart

```sh
yoho init                 # yoho.yml, .yoho/secrets, sample hooks
$EDITOR yoho.yml compose.yaml
yoho config check         # validate config, compose and secret refs
yoho setup                # interactive: Docker, deploy user, firewall (Debian/Ubuntu)
yoho plan                 # show what would change on the Server (read-only)
yoho apply                # confirm, then build, ship, health-gated cutover for proxied Services
```

Then `yoho app logs web -f`, `yoho app stats`, `yoho releases`, `yoho rollback <version>`.

## Features

- Compose-native: `docker compose up` still works locally, and plain `docker compose` keeps working on the Server if you stop using Yoho.
- Health-gated cutover: proxied Services switch without downtime through kamal-proxy (including behind Cloudflare Tunnel); workers restart stop-first on the compose runtime (Swarm defaults stateless Services to start-first rolling updates), Stateful Services restart stop-first on both runtimes, and upgrading the shared Proxy causes a brief (~1 s) interruption. See the [support matrix](docs/site/support-matrix.md).
- Registry-less SSH shipping: local build (Apple `container` on Apple silicon, Docker elsewhere), remote or on-Server builds, amd64 cross-builds. Default `transport.mode: auto` uses layer-diff push (`docker pussh`) when both the `docker pussh` plugin and the Server's containerd image store are present, and otherwise falls back to a full `docker save` / `docker load` archive (`builder.direct`, Gate uploads and Apple `container` images always ship full archives; every fallback prints its reason).
- File-based secrets by default: resolved locally from 1Password, Bitwarden, or any command; delivered as files; generated secrets on the Server, rotatable with `yoho secrets rotate --generated` (Postgres, MySQL/MariaDB or your own command). Values stay out of the repo, image, and `docker inspect` in this mode. Opt-in `secrets_as_env: true` delivers environment variables, which `docker inspect` can expose.
- Stateful Services are pinned and never `down -v`.
- Backups (restic, S3, B2, SFTP, rclone, or encrypted archives) and restore; `yoho backup drill` restores into a throwaway project and reports a recovery time estimate; opt-in scheduled jobs via systemd timers.
- Service templates: `yoho init --template postgres,redis,clickhouse,rustfs` adds ready-to-deploy Stateful Services with generated secrets and Backups.
- Cloudflare Tunnel, including account-less Quick Tunnels.
- Compose (default) or Swarm runtime. See the [support matrix](docs/site/support-matrix.md) for what each mode supports and refuses.
- Scoped team access through the Gate (`access: scoped`): scoped authorization per App Destination (fixed verbs, no shell, no Docker socket, audit log). The Gate is not mandatory human approval (`yoho deploy` / `apply --auto-approve` skip prompts, and code a Member deploys runs with that App's secrets).
- Kamal-compatible hooks and secrets-file style.
- `yoho setup` provisions a Debian/Ubuntu Server.
- Read-only dashboards: `yoho tui` in the terminal, `yoho ui` in a local browser; nothing runs on the Servers.
- AI-native: JSON Schema, `--json` NDJSON output, and a built-in agent skill (`yoho skill install`).

## Compared

Full comparison (sourced and dated as of 2026-11): [docs/site/compare.md](docs/site/compare.md).

| | Yoho | Uncloud | Kamal | Coolify | Dokploy |
|---|---|---|---|---|---|
| Unit of deploy | Compose project | Compose project | container per role | apps / Compose | apps / Compose |
| Runs where | CLI (Go) over SSH | CLI + `uncloudd` / Corrosion per machine | CLI (Ruby) over SSH | web platform + DB | web platform + DB |
| Ingress / TLS | kamal-proxy + Tunnel | Caddy (cluster-shared certs, `*.uncld.dev`) | kamal-proxy | Traefik / Caddy | Traefik |
| Registry required | no (SSH `pussh` or archive) | no (Unregistry layer push) | no (since 2.8) | server build or registry | server build or registry |
| Secrets | `.yoho/secrets`, per-Service files | env vars only | `.kamal/secrets` (env) | UI (env) | UI (env) |
| Backups & drills | volumes + dumps, `backup drill` | no | no | DB to S3 | DB to S3 |
| Multi-server | Swarm runtime (Roles planned) | WireGuard mesh | yes (roles) | yes | yes (Swarm / remote) |
| Platform to run | no | daemon per machine | no | yes | yes |
| License | MIT | Apache-2.0 | MIT | Apache-2.0 | Apache-2.0 |

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

A proxied Service needs that compose `healthcheck` (an error under swarm, a warning under compose). This example executes `wget`, so the image must include `wget`; if you use `curl`, change the healthcheck command accordingly.

See `examples/quickstart` for a runnable web + worker + Postgres App, and `examples/basic` for a fuller config-only file.

## Commands

| Command | Purpose |
|---|---|
| `init`, `config check\|show`, `schema [--ext]` | create and validate config |
| `setup` | provision a Server |
| `plan`, `apply`, `deploy`, `releases`, `rollback VERSION` | review and release lifecycle |
| `app ps\|logs\|stats\|exec` | inspect the running App |
| `tui`, `ui` | read-only dashboards of Servers, Services, Releases and logs, in the terminal or a local browser |
| `proxy boot\|status\|logs`, `tunnel up\|status\|down\|route` | Proxy and Cloudflare Tunnel |
| `secrets list\|print\|rotate [--generated KEY]` | inspect or rotate secrets, including Server-generated ones |
| `backup run\|list\|restore\|check\|pull\|drill`, `schedule install\|status\|remove` | Backups (integrity check, local mirror of archives, restore drill) and Scheduled Jobs |
| `swarm init\|join\|status` | Swarm runtime |
| `builder status\|setup` | local Builder |
| `skill install\|print [SKILL]` | agent skills |

Global flags: `-c/--config`, `-d/--destination` (default: `$YOHO_DESTINATION`, then `production`, then the only Destination), `--json`, `-v/--verbose`.

## Docs

Source in `docs/site` (VitePress). From the repository root, run `npm ci` and `npm run docs:dev`. Design: `VISION.md`, `GLOSSARY.md`, `docs/adr/`.
