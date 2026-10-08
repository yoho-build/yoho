---
name: yoho
description: Deploy and operate Docker Compose apps on your own servers with the yoho CLI (yoho.yml, x-yoho in compose.yaml, .yoho/secrets, zero-downtime deploys, backups, Cloudflare Tunnel). Use when the user wants to deploy a compose app over SSH, edit yoho.yml or x-yoho, run yoho commands, or migrate from Kamal, Coolify, Dokploy or Ansible rsync+compose playbooks.
---

# Yoho

Yoho is like Kamal but compose-native: one static Go binary deploys a `compose.yaml` app to Servers over SSH, builds locally, ships images without a registry, switches traffic with kamal-proxy, and backs up volumes. Per-Service settings live in `x-yoho` inside the compose file; where/how settings (Servers, Destinations, Builder, secrets, Backups) live in `yoho.yml`.

Always run `yoho config check` after editing config. Use `--json` (NDJSON events) whenever you parse output. Use `yoho schema` / `yoho schema --ext` for the JSON Schema of the Yoho file / `x-yoho`.

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
| `kamal deploy` / `rollback` / `app logs` | `yoho deploy` / `yoho rollback VERSION` / `yoho app logs` |
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
yoho app ps | logs [SERVICE...] [-f] [-n N] | exec SERVICE -- CMD...
yoho proxy boot | status | logs
yoho tunnel up | status | down    # Cloudflare Tunnel (Quick Tunnel when no token)
yoho backup run [JOB] | list [JOB] | restore ID [--job JOB] [-y]   # ID may be "latest"
yoho schedule install [JOB...] [--binary PATH] | status | remove [JOB...]
yoho swarm init [-y] | join [-y] | status
yoho skill install [--dir DIR] [--agent claude|codex|all] | print
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

- `references/config-reference.md`: every field of `yoho.yml` and `x-yoho`
- `references/migrate-kamal.md`
- `references/migrate-coolify.md`
- `references/migrate-dokploy.md`
- `references/migrate-ansible.md`
