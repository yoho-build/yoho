---
name: yoho
description: Deploy and operate Docker Compose apps on your own servers with the yoho CLI (yoho.yml, x-yoho in compose.yaml, .yoho/secrets, zero-downtime deploys, backups, Cloudflare Tunnel). Use when the user wants to deploy a compose app over SSH, edit yoho.yml or x-yoho, run yoho commands, or migrate from Kamal, Coolify, Dokploy or Ansible rsync+compose playbooks. Also use for first-time setup: installing the yoho CLI and walking the user through their first deploy.
---

# Yoho

Yoho is like Kamal but compose-native: one static Go binary deploys a `compose.yaml` app to Servers over SSH, builds locally, ships images without a registry, switches traffic with kamal-proxy, and backs up volumes. Per-Service settings live in `x-yoho` inside the compose file; where/how settings (Servers, Destinations, Builder, secrets, Backups) live in `yoho.yml`.

Always run `yoho config check` after editing config. Use `--json` (NDJSON events) whenever you parse output. Use `yoho schema` / `yoho schema --ext` for the JSON Schema of the Yoho file / `x-yoho`.

## First-time setup

Follow in order. Ask before installing anything, editing files, or running any command against a Server. Never use sudo.

1. Ask what the user wants: new deploy, migrate from Kamal/Coolify/Dokploy/Ansible (read the matching migrate reference), or operate an existing App. Ask for the target Server (`ssh user@host`) and confirm before any remote command.
2. Run `yoho version`. If missing, ask, then install with `curl -fsSL https://yohobuild.com/install.sh | sh` (fallback `curl -fsSL https://raw.githubusercontent.com/yoho-build/yoho/main/scripts/install.sh | sh`) or `go install github.com/yoho-build/yoho/cmd/yoho@latest`.
3. Offer `yoho skill install` so this skill persists in the project.
4. Run `yoho init`. Edit `yoho.yml` and `x-yoho` in `compose.yaml` using the config reference. Secrets only as refs in `.yoho/secrets`; never write values into the repo.
5. Run `yoho config check` and fix what it reports.
6. Run `yoho setup --plan`. `yoho setup` is interactive: tell the user to run it, or run `yoho setup -y` only with their consent.
7. Run `yoho plan`, show the plan, get explicit approval, then `yoho apply`.
8. Verify: hit the health URL, run `yoho app ps` and `yoho releases`. Report the rollback command (`yoho rollback VERSION`).
9. After 2-3 failed attempts on the same step, stop and report the step and the exact error.

## Ship a new version

Use this when the user asks whether there is something new to deploy, or asks to deploy the latest code. Yoho has no git webhook: deploys happen only when someone runs `yoho deploy`, by hand or from CI.

1. `yoho releases` shows the current Version as 12 hex characters of the git SHA, plus `_uncommitted_<8 hex>` when local changes were built. Strip the suffix and resolve the prefix with `git rev-parse <prefix>`.
2. Run `git fetch`. List what changed since that commit: `git log <sha>..origin/<branch>`, plus local commits and `git status`. Limit the log by path only to cover every build input: the build `context` and anything it pulls in, such as shared packages, lockfiles or the compose and Yoho files. In a monorepo the build context is often wider than the app directory.
3. Scan the diff for risk:
   - Database migrations.
   - Dockerfile, lockfile or compose changes.
   - New environment variables, from the app's env schema or `process.env`/`os.Getenv` uses.

   Add any new secret keys to `.yoho/secrets` and `x-yoho.secrets` before deploying.
4. Update the work tree with a fast-forward only (`git merge --ff-only`). Keep uncommitted Yoho files as they are.
5. Let `yoho plan` decide whether anything needs deploying (it builds nothing). Read each action:
   - A changed proxied Service is `update in-place (zero downtime)`.
   - Any other changed Service is normally `replace`, with a short restart of that Service, workers included. A change to only its `release_command` is an `update` without a restart.
   - If a stateful Service would be replaced or destroyed, stop and ask the user.
6. Before a migration that cannot be undone, run `yoho backup run`. Then run `yoho deploy`.
7. Verify:
   - Each hostname returns 200 on its health path.
   - The new containers are healthy.
   - The proxy logs show no 5xx responses during the switch (`yoho proxy logs`).
   - `yoho releases` marks the new Version as current.

   Report the rollback command: `yoho rollback <previous version>`. Rollback does not revert migrations.

If versions carry `_uncommitted_`, the build used local changes. Suggest committing them so that each Version maps to a commit.

## Kamal to Yoho

| Kamal | Yoho |
|---|---|
| `config/deploy.yml` | `yoho.yml` (where/how) + `x-yoho` on each compose Service |
| `deploy.<dest>.yml`, `-d dest` | `destinations:` in `yoho.yml`, `-d dest` (or `YOHO_DESTINATION`; default `production`); optional overlays `yoho.<dest>.yml` and `compose.<dest>.yaml` |
| roles | Not yet (Roles are planned); one Server per Destination, whole compose app |
| accessories | Stateful Services: `x-yoho.stateful: true`, pinned to one Server |
| `.kamal/secrets` | `.yoho/secrets` (+ `.yoho/secrets.<destination>`), same dotenv with `$(...)` |
| `env.secret` | `x-yoho.secrets`, with `NAME:KEY` alias (container name <- secret key) |
| `env.clear` | compose `environment:` |
| `builder.secrets` | `builder.secrets` (buildx `--secret`) |
| kamal-proxy | kamal-proxy, same (`x-yoho.proxy` per Service, `proxy:` in `yoho.yml`) |
| `.kamal/hooks/*` | `.yoho/hooks/*`, same names plus backup/restore hooks |
| `KAMAL_*` in hooks | `YOHO_*` |
| container env `KAMAL_VERSION`, `KAMAL_HOST`... | `YOHO_APP`, `YOHO_DESTINATION`, `YOHO_VERSION`, `YOHO_SERVER`, `YOHO_SERVICE` |
| `kamal setup` | `yoho setup` (interactive, Debian/Ubuntu) then `yoho deploy` |
| `kamal deploy` / `rollback` / `app logs` | `yoho deploy` / `yoho rollback VERSION` / `yoho app logs SERVICE` (required with several Services; `--all` interleaves every Service, compose only) |
| `kamal secrets fetch` | `secrets.providers` + `secrets.values` (op, bw, bws, command) |

## Hooks

Executable scripts in `.yoho/hooks/<name>` (dir configurable via `hooks.path`), run on the operator's machine. Non-zero exit aborts the command. Missing or non-executable hooks are skipped.

Names: `docker-setup`, `pre-connect`, `pre-build`, `pre-deploy`, `post-deploy`, `pre-app-boot`, `post-app-boot`, `pre-proxy-reboot`, `post-proxy-reboot`, `pre-backup`, `post-backup`, `pre-restore`, `post-restore`.

Hook env: `YOHO_APP`, `YOHO_DESTINATION`, `YOHO_VERSION`, `YOHO_SERVICE_VERSION` (`<app>@<version>`), `YOHO_HOSTS` (comma list), `YOHO_COMMAND`, `YOHO_SUBCOMMAND`, `YOHO_PERFORMER`, `YOHO_RECORDED_AT` (UTC RFC3339), `YOHO_LOCK`, `YOHO_RUNTIME` (seconds, post-deploy).

## Container env (injected unless compose sets them)

`YOHO_APP`, `YOHO_DESTINATION`, `YOHO_SERVER`, `YOHO_SERVICE` on every Service; `YOHO_VERSION` only on Services whose image is tagged with the Version (third-party images keep a stable config and are not recreated per deploy).

## Commands

```
yoho init                         # yoho.yml, .yoho/secrets, sample hooks
yoho config check | show          # validate / print resolved config
yoho schema [--ext]               # JSON Schema
yoho builder status | setup [-y]  # local Builder (Apple container on Apple silicon)
yoho secrets list                 # keys, sources, lengths (no values)
yoho secrets print KEY --reveal   # TTY only
yoho setup [--plan] [-y] [--containerd-image-store]   # provision Servers (interactive)
yoho plan [--version V] [--detailed-exitcode]   # read-only diff; exit 2 = changes
yoho apply [--auto-approve] [--skip-build] [--version V]   # plan, confirm, deploy
yoho deploy [--skip-build] [--version V]   # apply --auto-approve without the plan
yoho releases ; yoho rollback VERSION
yoho app ps | logs [SERVICE...] [--all] [-f] [-n N] | stats [SERVICE...] [--no-stream] | exec SERVICE -- CMD...
yoho proxy boot | status | logs
yoho tunnel up | status | down    # Cloudflare Tunnel (Quick Tunnel when no token)
yoho backup run [JOB] | list [JOB] | restore ID [--job JOB] [-y]   # ID may be "latest"
yoho schedule install [JOB...] [--binary PATH] | status | remove [JOB...]
yoho swarm init [-y] | join [-y] | status
yoho skill install [--dir DIR] [--agent claude|codex|all] | print [SKILL]   # install writes all bundled skills
```

Global flags: `-c/--config`, `-d/--destination` (default `$YOHO_DESTINATION`, then `production`, then the only Destination), `--json`, `-v/--verbose`. Check `yoho <cmd> --help` for all flags. `apply` refuses without a terminal unless `--auto-approve`; `--json` requires it.

## Safety rules

- Never run `docker compose down -v` or remove volumes on a Server; Yoho never does. Stateful Services are pinned and never duplicated.
- Never put secrets in compose `${VAR}` interpolation, in `environment:` values, in argv, or in the repo; `yoho config check` rejects `${VAR}` secrets. Declare them in `x-yoho.secrets` and keep values in `.yoho/secrets` refs (`$(op read ...)`) or `secrets.values`.
- Do not print secret values; use `yoho secrets list`. `secrets print` needs `--reveal` and a terminal.
- Rollback does not revert volumes or migrations. Take a `yoho backup run` before risky releases.
- Run `yoho config check` and `yoho plan` before `yoho deploy`; deploy to staging first when a Destination exists.
- Parse output with `--json`, never by scraping human output.
- Switching a Destination between `compose` and `swarm` runtime causes downtime.

## Minimal config

```yaml
# yoho.yml
app: shop
servers:
  primary: { ssh: yoho@203.0.113.10 }
destinations:
  production: { servers: [primary] }
```

```yaml
# compose.yaml
services:
  web:
    build: .
    healthcheck: {test: ["CMD", "wget", "-qO-", "http://127.0.0.1:3000/up"], interval: 5s}
    x-yoho:
      proxy: { hosts: [shop.example.com], port: 3000 }
      secrets: [DATABASE_PASSWORD:POSTGRES_PASSWORD]
      release_command: ["bin/rails", "db:migrate"]
```

A proxied Service needs that compose `healthcheck` (an error under swarm, a warning under compose). The image needs wget or curl.

## References

Fetch with `curl -sL <url>` (not a summarizing fetch tool). Relative paths are under `skills/yoho/`.

- `references/config-reference.md`: every field of `yoho.yml` and `x-yoho`. https://raw.githubusercontent.com/yoho-build/yoho/main/skills/yoho/references/config-reference.md
- `references/migrate-kamal.md`: https://raw.githubusercontent.com/yoho-build/yoho/main/skills/yoho/references/migrate-kamal.md
- `references/migrate-coolify.md`: https://raw.githubusercontent.com/yoho-build/yoho/main/skills/yoho/references/migrate-coolify.md
- Skill `yoho-migrate-coolify` (full Coolify playbook: inventory, secrets, shared Server, cutover, tunnel): https://raw.githubusercontent.com/yoho-build/yoho/main/skills/yoho-migrate-coolify/SKILL.md
- `references/migrate-dokploy.md`: https://raw.githubusercontent.com/yoho-build/yoho/main/skills/yoho/references/migrate-dokploy.md
- `references/migrate-ansible.md`: https://raw.githubusercontent.com/yoho-build/yoho/main/skills/yoho/references/migrate-ansible.md
