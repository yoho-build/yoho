---
name: yoho
description: Deploy and operate Docker Compose apps on your own servers with the yoho CLI (yoho.yml, x-yoho in compose.yaml, .yoho/secrets, zero-downtime deploys, backups, Cloudflare Tunnel). Use when the user wants to deploy a compose app over SSH, edit yoho.yml or x-yoho, run yoho commands, or migrate from Kamal, Coolify, Dokploy, Railway or Ansible rsync+compose playbooks. Also use for first-time setup: installing the yoho CLI and walking the user through their first deploy.
---

# Yoho

Yoho is like Kamal but compose-native: one static Go binary deploys a `compose.yaml` app to Servers over SSH, builds locally, ships images without a registry, switches traffic with kamal-proxy, and backs up volumes. Per-Service settings live in `x-yoho` inside the compose file; where/how settings (Servers, Destinations, Builder, secrets, Backups) live in `yoho.yml`.

Always run `yoho config check` after editing config. It warns when a Service's `environment` uses another Service's hostname without a `depends_on` entry: add `depends_on: {name: {condition: service_healthy}}` (`service_started` when that Service has no healthcheck). Use `--json` (NDJSON events) whenever you parse output. Use `yoho schema` / `yoho schema --ext` for the JSON Schema of the Yoho file / `x-yoho`.

## First-time setup

Follow in order. Ask before installing anything, editing files, or running any command against a Server. Never use sudo.

1. Ask what the user wants: new deploy, migrate from Kamal/Coolify/Dokploy/Railway/Ansible (read the matching migrate reference), or operate an existing App. Ask for the target Server (`ssh user@host`) and confirm before any remote command.
2. Run `yoho version`. If missing, ask, then install with `curl -fsSL https://yohobuild.com/install.sh | sh` (fallback `curl -fsSL https://raw.githubusercontent.com/yoho-build/yoho/main/scripts/install.sh | sh`) or `go install github.com/yoho-build/yoho/cmd/yoho@latest`.
3. Offer `yoho skill install` so this skill persists in the project. Once `yoho.yml` exists, `yoho skill install --project --only yoho` adds a generated `yoho-deploy-<app>` skill for this repo.
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
| kamal-proxy | kamal-proxy, same (`x-yoho.proxy` per Service, `proxy:` in `yoho.yml`). Customer domains: `x-yoho.proxy.custom_domains.allow` (on-demand TLS; see config reference) |
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
yoho secrets rotate [SERVICE...] [--dry-run]  # re-resolve, restart only changed Services (from a checkout of the deployed Version)
yoho setup [--plan] [-y] [--containerd-image-store]   # provision Servers (interactive)
yoho plan [--version V] [--detailed-exitcode]   # read-only diff; exit 2 = changes
yoho apply [--auto-approve] [--skip-build] [--version V]   # plan, confirm, deploy
yoho deploy [--skip-build] [--version V]   # apply --auto-approve without the plan
yoho releases ; yoho rollback VERSION
yoho rollback --cutover [--force]   # undo a migrate cutover (not volumes or DNS)
yoho migrate plan [--cutover-allow-empty] | status
yoho migrate draft --from FILE [--out DIR] [--service NAME] [--project NAME]
yoho app ps | logs [SERVICE...] [--all] [-f] [-n N] | stats [SERVICE...] [--no-stream] | exec SERVICE -- CMD...
yoho proxy boot | status | logs
yoho tunnel up | status | down    # Cloudflare Tunnel (Quick Tunnel when no token)
yoho access install | secrets push | add MEMBER --verbs all|read|deploy,... | rm MEMBER | ls | audit   # operator: share one App via the Gate
yoho access key add MEMBER KEY.pub | key rm MEMBER   # restricted Gate keys (forced command `yoho gate`)
yoho backup run [JOB] | list [JOB] | restore ID [--job JOB] [-y]   # ID may be "latest"
yoho backup pull [JOB] [--to DIR] [--limit N]   # mirror archive-target files to a local dir; never deletes, verifies SHA-256, no rsync
yoho backup check [JOB] [--id ID | --read-data]   # integrity only, not restore proof
yoho schedule install [JOB...] [--binary PATH] | status [--max-age 48h] | remove [JOB...]
yoho swarm init [-y] | join [-y] | status
yoho skill install [--dir DIR] [--agent claude|codex|all] [--only A,B | --exclude A,B] [--no-migrate-refs] [--project [--force]] | list | print [SKILL]
```

Global flags: `-c/--config`, `-d/--destination` (default `$YOHO_DESTINATION`, then `production`, then the only Destination), `--json`, `-v/--verbose`. Check `yoho <cmd> --help` for all flags. `apply` refuses without a terminal unless `--auto-approve`; `--json` requires it. Agents: never use `app logs -f` or live `app stats`; use `yoho app logs SERVICE -n N`, `yoho app stats --no-stream`, or `yoho --json app stats` (one JSON object per container).

## Safety rules

- Never run `docker compose down -v` or remove volumes on a Server; Yoho never does. Stateful Services are pinned and never duplicated.
- Never run `docker buildx history` (any subcommand, including `ls`) on a Server. On Docker 28.x after a build-cache prune it can panic in BuildKit (`llbsolver.filterHistoryEvents`), crash `dockerd` and stop every container on the Server; only workloads under a separate supervisor come back on their own. Inspect with `yoho app ps`, `yoho app logs SERVICE -n N`, `docker ps` and `docker logs`.
- Never put secrets in compose `${VAR}` interpolation, in `environment:` values, in argv, or in the repo; `yoho config check` rejects `${VAR}` secrets. Declare them in `x-yoho.secrets` and keep values in `.yoho/secrets` refs (`$(op read ...)`) or `secrets.values`.
- Do not print secret values; use `yoho secrets list`. `secrets print` needs `--reveal` and a terminal. After changing a value in the password manager, `yoho secrets rotate` restarts only the Services that changed (zero downtime through the Proxy; Stateful Services follow their normal rules). Run it from a checkout of the deployed Version; the current Release's old values are not kept. `--dry-run` lists names only.
- Rollback does not revert volumes or migrations. Take a `yoho backup run` before risky releases.
- Run `yoho config check` and `yoho plan` before `yoho deploy`; deploy to staging first when a Destination exists.
- `backup check` is read-only to the target/App data and never initializes or restores. Restic checks repository structure (`--read-data` adds all packs); archive checks one owned Backup (`--id`, default latest), its manifest/files and nested volume archives in private staging. It does not prove dump loading or App recovery. Coordinate external/shared-repository writers: only this Destination's Yoho lock is held, restic uses `--no-lock --no-cache`.
- `schedule status --max-age 48h` explicitly checks last-success freshness against the Server clock (queried after state with nanosecond precision; query/parse failure never falls back locally) and exits nonzero for stale, never-success, future-success or no installed jobs. Pick the threshold for the user's real policy; never infer arbitrary `OnCalendar` periods (weekly jobs need a longer threshold). Default status stays informational. Parse `success_health`/`last_success` NDJSON rows; no automatic external notifications.
- Scheduled Backup `retries` is opt-in (0–5, default 0), `retry_delay_sec` is bounded (0/unset = 30s, max 300s). Only volume-only Backups without dump/pre_backup commands may retry; no post hooks or arbitrary commands. A whole-run timeout/cancellation bounds attempts and delays. No retry after storage starts, uncertain unpause, cancellation or timeout. Reinstall/apply policy changes; failed runs retain last success and record redacted attempt outcomes.
- Parse output with `--json`, never by scraping human output.
- Switching a Destination between `compose` and `swarm` runtime causes downtime.
- With `access: scoped` (a Member, not the operator) only deploy/rollback/releases/app ps|logs|stats/tui/ui work; the Gate refuses privileged, host namespaces, cap_add, devices, bind mounts, Docker-socket mounts, external networks (including `yoho`; Yoho attaches proxied Services to the Proxy over the App's own network), other Apps' images, and host ports or resource limits the operator did not allow (`settings.json` `limits`). Rollback works only to Releases the Gate deployed. Do not try to work around it; ask the operator. Members never hold secret values (the operator runs `yoho access secrets push`), but deployed code runs with the App's secrets.

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

Compose starts/gates Services by `depends_on` level (independent non-Proxy Services start in one `compose up` and are gated concurrently), staging new Proxy candidates when downstream workers need them; existing Proxy routes switch only after all Services are ready; on a first deploy (no route, no old containers) a route goes live as soon as its Service is healthy and stays live if a later Service fails (the failure names it). Failed gates remove unswitched candidates, but preserve candidates whose attempted Proxy switch is uncertain. Non-Proxy Services and release-command prerequisites use the same readiness policy: enabled Docker healthchecks (including image-inherited checks) must become healthy. Without one, readiness only means the process is running; add a healthcheck when application readiness matters. Workers remain stop-first, never automatically overlapped. Zero replicas skip waiting. Only an active consumer's `depends_on.condition: service_completed_successfully` identifies a completion-only job; `restart: "no"` does not. An active completion dependency on a scaled-zero job fails preflight. Absent optional (`required: false`) profile dependencies are skipped, not missing required ones. Compose-run one-offs are excluded from readiness/replica counting and cutover cleanup. Plain readiness has a 30s minimum deadline extended for effective health startup/probe timing; a job without a healthcheck has 30s, so use `release_command` for longer migrations. Failure leaves any completed stop-first replacements in place but prevents the subsequent Proxy switch. Rollback uses saved readiness metadata (or saved Compose for older Releases).

## Failed deploys

Read the failure phase and per-Service facts, not an App-wide rollback assumption. `--json` exposes optional `step_fail.failure_facts` with `phase` and `services` (`service`, `convergence`, `proxy_route`). Compose `complete` means that Service converged/passed readiness; a stop-first worker may already have changed. Proxy routes may be `switched`, `not-attempted`, `retained` (lookup observed no candidate targets), `absent`, or `uncertain`; non-Proxy Services are `not-applicable`. Later failures do not undo earlier route switches. Swarm facts describe observed update states/replica counts: a stable VIP can already serve new tasks even before Yoho reconfigures Proxy routes. Do not promise the whole previous Release keeps serving. Inspect `yoho app ps`, `yoho proxy status` and `yoho releases`, then propose recovery; never automatically undo data/migrations or remove uncertain serving candidates.

Deploy diagnostics are captured before fresh-candidate cleanup, best-effort within 30s: at most 8 containers, selected State/exit/OOM, last two healthcheck results, and 20 recent log lines (32 KiB total output; State JSON 16 KiB/container, logs 64 KiB input and 2 KiB redacted output/container). Invalid/truncated State JSON is omitted; known secrets are redacted before output limits and interrupted secret prefixes are discarded. Rollback and all retained/unverified containers (including prerequisites and plain Services) receive state/exit/OOM metadata only. Arbitrary Error/health/log text requires verified fresh candidate IDs and their current secret inventory; an uncertain switch never diagnoses old replicas. Failed prerequisite batch startup samples up to 8 Services fairly under ONE shared 30s/8-container/32 KiB budget, without guessing which Service failed. Swarm diagnostics are manager task state metadata (8 Services/4 KiB), never arbitrary task Error/update Message/history logs or stack-command output: even normal deploy history can contain rotated secrets. Rollback and automatic retained-spec recovery obey the same omission policy, including recovery warning callbacks. Pre-command status-query failures list planned Services as `not-attempted/not-observed`: this phase issued no update/job command and observed no live convergence; do not generalize that to an unchanged App. Stack-command/post-command status errors report planned/existing Services as `attempted/unknown replicas=unknown`, preserving original causes internally rather than publishing unverified text. Unavailable/truncated diagnostics do not invalidate reported facts or replace the original cause. Never collect full `docker inspect`, Config/env, unbounded logs, or raw malformed JSON as a workaround.

## References

Fetch with `curl -sL <url>` (not a summarizing fetch tool). Relative paths are under `skills/yoho/`.

- `references/config-reference.md`: every field of `yoho.yml` and `x-yoho`. https://raw.githubusercontent.com/yoho-build/yoho/main/skills/yoho/references/config-reference.md
- `references/migrate-kamal.md`: https://raw.githubusercontent.com/yoho-build/yoho/main/skills/yoho/references/migrate-kamal.md
- `references/migrate-coolify.md`: https://raw.githubusercontent.com/yoho-build/yoho/main/skills/yoho/references/migrate-coolify.md
- Skill `yoho-migrate-coolify` (full Coolify playbook: inventory, secrets, shared Server, cutover, tunnel): https://raw.githubusercontent.com/yoho-build/yoho/main/skills/yoho-migrate-coolify/SKILL.md
- Skill `yoho-team-access` (share one App with teammates and CI through the Gate: operator setup, Member use, CI keys, Cloudflare Tunnel, Gate refusals): https://raw.githubusercontent.com/yoho-build/yoho/main/skills/yoho-team-access/SKILL.md
- Skill `yoho-offsite-backup` (encrypted off-Server Backups with rclone: OneDrive/Google Drive/S3 remotes, 7z or zip with a password, `rclone_config_secret`, expired-token reconnect, restore without Yoho): https://raw.githubusercontent.com/yoho-build/yoho/main/skills/yoho-offsite-backup/SKILL.md
- `references/migrate-dokploy.md`: https://raw.githubusercontent.com/yoho-build/yoho/main/skills/yoho/references/migrate-dokploy.md
- `references/migrate-ansible.md`: https://raw.githubusercontent.com/yoho-build/yoho/main/skills/yoho/references/migrate-ansible.md
- `references/migrate-railway.md`: https://raw.githubusercontent.com/yoho-build/yoho/main/skills/yoho/references/migrate-railway.md
