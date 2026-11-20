# CLI reference

With `access: scoped` (or `YOHO_ACCESS=scoped`) only `deploy`, `plan`, `rollback`, `releases`, `app ps/logs/stats`, `tui`, `ui` and `config check/show` run, through the Gate; see [Sharing one App with a team](team-access.md).

Global flags (every command): `-c, --config FILE`, `-d, --destination NAME` (default: `$YOHO_DESTINATION`, then `production`, then the only Destination), `--json` (NDJSON events), `-v, --verbose` (stream remote output). On a terminal, `deploy`/`apply` show one live line per built Service (current BuildKit step and elapsed time) that freezes as `✓ web 1m12s`; without a terminal only the start and finish lines print, and `-v` streams the full build output. With `--json` each build emits `build_service` (start), throttled `build_progress` (`service`, `index`, `total`, `step`, `done`, `steps_total`, `cached`; at most one per 500 ms per Service) and `build_service_done` (`service`, `duration_ms`, `cached`, `ok`). Progress needs `docker buildx` with `--progress=rawjson`; older buildx falls back to plain output.

| Command | Description |
|---|---|
| `upgrade` / `update` | Update the running local CLI to the latest stable GitHub release, checking SHA-256 and signed build provenance before atomic replacement. No Yoho file required. Skips current/newer stable versions; package manager installs use their package manager. |
| `init [--template NAME[,NAME...]] [--list-templates]` | Create `yoho.yml`, `.yoho/secrets`, sample hooks. `--template` also writes ready-to-deploy Services (`postgres`, `redis`, `clickhouse`, `rustfs`) into the compose file, appending to an existing one and refusing name conflicts, and a commented-out `backups:` example into a new `yoho.yml` (printed when `yoho.yml` exists); see [Service templates](getting-started.md#service-templates). |
| `config check [--offline]` | Validate Yoho file, compose files, `x-yoho` and secret names; resolves local secrets only when `secrets.nonempty` is nonempty. `--offline` (or `YOHO_OFFLINE=1`) never resolves secrets. Exit 0 ok, 1 findings, 2 files could not be loaded. Warns when `ssh.via: cloudflare` would go through a `cloudflared` Service in the same App, and (like `plan` and `deploy`, once per run) when a Stateful Service has no Backup job of the Destination covering it. See [CI check](#ci-check). |
| `config show` | Print the resolved Yoho file with defaults as JSON. |
| `doctor [--max-age D]` | Read-only production-readiness check for a Destination: config and Backup coverage (Stateful Services first), Scheduled Backup freshness and last successful restore drill, firewall and public `0.0.0.0` ports outside the Proxy, free disk on Docker data root and Backup cache, Proxy state and version, pending secret-rotation generations, and image transport path. Exit 0 no errors, 1 any error. See [Doctor](#doctor). |
| `schema [--ext | --events]` | JSON Schema of the Yoho file, or of `x-yoho` with `--ext`. |
| `setup [--plan] [-y] [--containerd-image-store | --no-containerd-image-store]` | Provision the Destination's Servers (interactive). `--plan` only shows the checklist, `-y` applies every needed step. Enables the containerd image store by default without running containers; opt out with `--no-containerd-image-store`. See [Server setup](setup.md). |
| `plan [--version V] [--detailed-exitcode] [--out FILE] [--ttl 1h]` | Read-only diff of the configuration against the Server: containers, Proxy routes, Proxy, Tunnel, Scheduled Jobs. Nothing is built or changed. With `--detailed-exitcode`: exit 0 no changes, 1 error, 2 changes. With scoped access the Gate plans (Services, routes and the Proxy only) with its limits applied, as a scoped deploy would; it needs the `deploy` grant. See [Team access](team-access.md). |
| `apply [--plan FILE] [--refresh] [--scan] [--auto-approve] [--version V] [--skip-build] [--cutover-resume] [--cutover-allow-empty] [--lock-wait[=D]]` | Show the plan, ask `yes`, take the Destination lock, then build, ship, deploy, converge Tunnel and Scheduled Jobs, and remove what was deleted from config. Refuses without a terminal unless `--auto-approve`; `--json` requires `--auto-approve` unless `--plan FILE` supplies the approval. With a Destination `migrate` block, cuts over once; see [Migration cutover](configuration.md#migration-cutover-migrate). |
| `deploy [--refresh] [--scan] [--skip-build] [--version V] [--cutover-resume] [--cutover-allow-empty] [--lock-wait[=D]]` | Kamal-compatible `apply --auto-approve` without printing the plan (also converges Tunnel and Scheduled Jobs, and runs a pending migration cutover). Takes the Destination lock before building, so a concurrent deploy is refused before it builds; `--lock-wait` waits for it instead (bare flag: 15m, checking every 15s); see [Locks](zero-downtime.md#locks). Version defaults to the git SHA. `--cutover-resume` continues an unfinished cutover; `--cutover-allow-empty` accepts a `migrate.stop` that matches no running container. If the rollout succeeds but the requested Scheduled Jobs cannot be installed or removed (for example no systemd user bus), the exit status is still non-zero, the output ends with `Deployed APP@VERSION to DEST; Scheduled Jobs not installed: REASON` and `--json` emits a `jobs_failed` event (`rollout: "ok"`, `jobs: "failed"`, `version`, `error`) before `finished`. A Gate deploy whose SSH session drops (exit 255) still finishes on the Server; `yoho deploy` reconnects, or `yoho releases` shows the outcome. |
| `releases` | List Releases on the Server, newest first. |
| `remove [--volumes [--confirm APP]] [-y]` | Take the Destination off its Servers (like Kamal `remove`): under the Destination lock, remove its Scheduled Jobs, its Proxy routes (not the shared Proxy), its `yoho-<app>-<destination>` containers including one-offs (swarm: `docker stack rm`, then its Swarm secrets), its networks (never the shared `yoho` network), the App directory with its Releases and secrets (not the audit log, see [Audit log](#audit-log)), and images no other Destination's Release keeps. Objects are selected by their exact compose project / stack label, so other Apps, other Destinations and drill projects are never touched. Volumes and Backups are kept, and a later deploy starts on the same data. `--volumes` also deletes the Destination's named volumes (like ONCE `remove --remove-data`): type the App name at the prompt, or pass `--confirm APP` without a terminal. Backups (the local spool and every Backup Target) are never deleted. Shows what it removes and asks `yes` on a terminal unless `-y`; without a terminal, and with `--json`, `-y` is required. `--json` emits `remove_plan` per Server and a final `removed` event (`containers`, `routes`, `networks`, `volumes_removed`, `volumes_kept`). Tunnel ingress rules are not changed. |
| `rollback VERSION [--with-release-secrets] [--lock-wait[=D]]` | Redeploy a previous Release's images and compiled compose, running the rollback Hooks. Volumes and migrations are not reverted. Secrets are not either: its Services keep the current secret and generated values, and a Release that reads a secret the current one lacks is refused with the missing names. `--with-release-secrets` runs the Release's own secrets generation (rotated values come back; warned). See [Secrets](secrets.md#rollback-keeps-the-current-secrets). |
| `rollback --cutover [--force]` | Undo the migration cutover: stop this App's containers, start the old ones, restore the saved tunnel ingress. Refuses when the ingress changed since, unless `--force`. |
| `lock acquire -m MESSAGE` | Hold the Destination lock by hand (maintenance): deploys, rollbacks, rotates, Backups and Scheduled Backups refuse with the message until `lock release`. See [Locks](zero-downtime.md#locks). |
| `lock status` | Who holds the Destination lock: performer, command, Version, message, since (`--json`: a `lock` event). |
| `lock release [--force]` | Release a lock taken with `lock acquire`; `--force` also removes one a running or killed command holds (after a killed run). |
| `audit [-n 50] [--since D] [--json]` | The Destination's audit log: who ran which mutating command, with which Version, how it ended and how long it took (TIME, BY, COMMAND with a compact summary of its flags such as `secrets rotate --generated POSTGRES_PASSWORD` or `deploy --lock-wait`, VERSION, RESULT, DURATION; a killed command shows `unfinished`). `--since` takes `36h`, `7d` or a date. `--json` prints the log's records, one per line. See [Audit log](#audit-log). Members of the Gate: `access audit`. |
| `migrate plan [--cutover-allow-empty]` | Run the migration cutover preflight alone (read-only): old containers to stop, claimed-data conflicts, ingress rules to repoint. `--cutover-allow-empty` accepts a `migrate.stop` that matches no running container. |
| `migrate status` | Print the cutover journal (`<app dir>/cutover.json`). |
| `migrate draft --from FILE [--out DIR] [--service NAME] [--project NAME]` | Print `compose.yaml` and `yoho.yml` from a names-only inventory JSON, or from one service's `railway.json` / `railway.toml`. `--out` writes those two files only when neither exists. Does not write `.yoho/secrets`, contact Railway, or contact a Server. See [Migrating from Railway](migrating-from-railway.md). |
| `app ps` | List the App's containers. When a Service of the current Release has an [`x-yoho.check`](configuration.md#service-checks), runs each check once and adds a `CHECK` column. |
| `app logs [SERVICE...] [--all] [-f] [-n N] [--since T] [--until T] [-t] [--grep TEXT [-i] [-E]]` | Logs of the named Services; `-f`/`--follow`, `-n`/`--lines` from the end (default 100; `0` for none, so `-f -n 0` shows only new lines; `-1` for every line, at most 10000 through the Gate), `-t`/`--timestamps`. `-f` runs until Ctrl-C, SIGTERM or the connection drops; the Server then stops `docker logs` (and `grep`) itself, even for a Service that logs nothing. `--since`/`--until` take what docker takes: a duration back from now (`30s`, `10m`, `2h`) or a timestamp (`2026-11-03T15:04:05Z`, `2026-11-03`, Unix seconds); `--until` is compose only and cannot be combined with `-f` (`--until cannot be combined with -f`: it ends the output at a point in the past; use `--since` with `-f`). `--grep TEXT` keeps lines containing the fixed string TEXT, filtered on the Server with `grep` (also with `-f`); `-i`/`--ignore-case`, `-E`/`--regex` for an extended regular expression. Without `-n`, `--since` and `--grep` show every line they select (add `-n` to bound the output, e.g. `--since 10m -n 200`); with `--grep` and no `-f`, `-n` counts matching lines. A bad `-E` pattern fails before docker runs, with `invalid --grep pattern` (exit 2). `grep` runs under `nice`; without `-f` the Server stops a search after 60 seconds (where `timeout` exists, as in coreutils and BusyBox) with `--grep search took longer than 60s` (exit 124): narrow it with `--since` or `-n`. Through the Gate (a Member), a search without `-f` reads at most the last 100000 lines of the logs, `-E` patterns may not use back-references (`\1` to `\9`), and a Gate older than the CLI refuses `--since`, `--until` and `--grep` (`unknown field`) until the operator runs `yoho access install` again. With one Service it is implied; with several, name one or more (the error lists them) or pass `--all` for every Service interleaved (compose only; swarm takes exactly one Service). |
| `app stats [SERVICE...] [--no-stream]` | CPU, memory, network and block I/O of the App's containers (default: every Service) on the Destination's first Server. Live view in a terminal; one snapshot with `--no-stream`, without a terminal, or when `CI` is set or `TERM=dumb` (use this from scripts and coding agents); `--json` prints docker's JSON object per container. Swarm: only tasks on the first Server. |
| `app exec SERVICE [-i] [-t] -- COMMAND...` | Run a command in a running Service container. Default: no stdin, no pseudo-terminal (`docker compose exec -T`), for scripts. `-i` passes this terminal's stdin; `-t` allocates a pseudo-terminal (`ssh -t`, `docker compose exec` without `-T`; swarm `docker exec -it`) and needs a terminal; `-it` for consoles and shells (`yoho app exec web -it -- bin/rails console`). An unknown SERVICE fails before connecting, with the list of Services. The command's output and exit code are passed through: a failing command (or `service "web" is not running`) prints only its own stderr and exits with its code. |
| `app run SERVICE [-i] [-t] [--allow-stateful] -- COMMAND...` | One-off container from the current Release of SERVICE: `docker compose run --rm --no-deps` with the Release's compiled compose, so the same image, secrets files and env, networks and volumes, but no published ports, no Proxy route and no dependency started; labelled `yoho.oneoff=run` (and compose's one-off label) and removed when the command exits. Serving containers are untouched. For migrations and consoles: `yoho app run web -- bin/rails db:migrate`, `yoho app run web -it -- bin/rails console`. The command's exit status is yoho's. A Stateful Service is refused unless `--allow-stateful` (a second container on its data, ADR 0005). Compose only: on swarm use `app exec` or `x-yoho.release_command`. |
| `tui [--interval 5s] [-n LINES]` | Live, read-only terminal dashboard: every Server of the Destination (load, memory, disk, Docker), the App's containers with health, CPU and memory, the Proxy, Releases, and container logs. Keys: `tab`/`←→` Server, `↑↓` select, `d` Service details (CPU, memory, network/disk I/O, PIDs), `enter` logs (`/` filters them), `r` refresh, `q` quit. Needs a terminal. See [Dashboards](dashboards.md). |
| `ui [--port 0] [--no-open] [--interval 5s]` | The same view in the browser, served on `127.0.0.1` from this machine with a one-time login link. Read-only; nothing runs on the Servers. See [Dashboards](dashboards.md). |
| `access install [--binary PATH]` | Operator: set up the Gate on the Server (yoho-gate user, sudoers rule, root-owned `/usr/local/bin/yoho`). Idempotent; needs root or sudo. See [Sharing one App with a team](team-access.md). |
| `access secrets push` | Operator: resolve secrets locally and store them, with Proxy, Registry and hostname settings (including a catch-all when `custom_domains` is set), for scoped deploys. |
| `access add MEMBER [--verbs all\|read\|deploy,logs,...]` / `access rm MEMBER` / `access ls` | Operator: grant, revoke and list Members' verbs on the App Destination. |
| `access key add MEMBER PUBKEY_FILE` / `access key rm MEMBER` | Operator: authorize or remove a Member's restricted SSH key (forced command `yoho gate`). |
| `access audit [-n 50]` | Operator: last Gate requests (JSON lines). |
| `proxy boot \| status \| logs` | Manage the shared kamal-proxy. `status` also shows the running kamal-proxy version and whether the pinned (or `proxy.image`) one is newer. |
| `proxy upgrade [-y] [--allow-downgrade]` | Upgrade `yoho-proxy` on every Server of the Destination to the pinned or `proxy.image` kamal-proxy: pull first, recreate with the same ports/flags/volume, verify every route again. Brief interruption; see [Upgrading the Proxy](zero-downtime.md#upgrading-the-proxy). |
| `tunnel up \| status \| down` | Managed Cloudflare Tunnel (cloudflared connector; Quick Tunnel without a token). |
| `tunnel route --hostname HOST --to proxy\|ORIGIN [--path P] [--check-path P] [--dry-run] [--timeout 60s] [--lock-wait[=D]]` | Operator: point one ingress rule of the remotely managed tunnel at the Proxy through the Cloudflare API. Checks the route through the Proxy first and that the rule serves the check path, prints the old rule and the undo command, switches the rule in the configuration as it is then, checks `https://HOST<check path>` afterwards (2xx; the status for `http_status:NNN`; no check for other non-HTTP targets) and puts the old rule back when that fails. Needs `proxy.tunnel.api_token_secret`. See [Cloudflare Tunnel](cloudflare-tunnel.md#switching-a-hostname-to-the-proxy). |
| `secrets list` | Keys, sources, lengths. |
| `secrets print KEY... --reveal` | Print values; terminal only. |
| `secrets rotate [SERVICE...] [--dry-run] [--lock-wait[=D]]` | Re-resolve secrets (no rebuild), restart only Services whose values changed, record the current Release again. Needs a checkout of the deployed Version. Named Services must be the only ones that changed. `--dry-run` lists what would restart. |
| `secrets rotate --generated KEY [--generated KEY...] [--dry-run]` | Replace an `x-yoho.generate` secret: apply the new value to the database (Postgres, MySQL/MariaDB, or an `x-yoho.rotate` command), then swap it in and restart every Service that uses it. A failed database step keeps the old value; a re-run reuses the pending one. See [Secrets](secrets.md#rotating-generated-secrets). |
| `backup run [JOB] [--lock-wait[=D]]` | Take a Backup now. `--lock-wait` waits for a held Destination lock (also on `backup restore` and `backup drill`). |
| `backup list [JOB]` | Stored Backups, newest first. |
| `backup pull [JOB] [--to DIR] [--limit N]` | Mirror new archives of an `archive` target to a local directory; never deletes. |
| `backup restore ID [--job JOB] [-y] [--report]` | Restore a Backup (`latest` for the newest); asks for confirmation unless `-y`. `--report` emits recovery point and restore duration; full recovery, image source and Scheduled Jobs remain unknown until verified after apply. |
| `backup restore ID --from DEST [--copy-generated-secrets] -d DEST2` | Restore a Backup taken by another Destination of the same App (e.g. production into staging). Keeps the target's generated secrets unless `--copy-generated-secrets`. Another App's Backup is refused. |
| `backup restore --cleanup` | Remove leftover `restore-*`, `run-*` and stale `check-*` staging; Services that an interrupted restore stopped are not resumed afterwards. |
| `backup check [JOB] [--id ID \| --read-data]` | Read-only integrity check of the Backup Target or one archive Backup. Not a restore proof. |
| `backup drill [JOB] [--id ID] [--keep]` | Restore a Backup into a throwaway `yoho-<app>-<destination>-drill-<8 random hex>` project (internal network, no ports, no route), wait for health, run `x-yoho.backup.verify`, print per-phase timings and an RTO estimate, then remove it. The running App is untouched. `--keep` leaves it running. |
| `backup drill --cleanup` | Remove leftover drill projects of the Destination (only drill-labelled containers, volumes and networks) and their `drill-*` staging. |
| `schedule install [JOB...] [--binary PATH]` | Install systemd timers for Backup jobs with a schedule (default: all of the Destination). |
| `schedule status` | Installed Scheduled Jobs, timers, last runs. |
| `schedule remove [JOB...]` | Disable and delete Scheduled Jobs (default: all of the Destination). |
| `swarm init [-y]` | Initialize a Swarm on the first Server (the manager). Errors unless `runtime` is `swarm`. |
| `swarm join [-y]` | Join the other Servers as workers. Errors unless `runtime` is `swarm`. |
| `swarm status` | Swarm nodes and the App's services. |
| `builder status` | Build engine in use and Apple `container` state. |
| `builder setup [-y]` | Install/upgrade Apple container, Rosetta, builder VM; smoke test. |
| `builder check` | `builder.location: remote`: probe each Server from the Builder (direct ship) and print the setup commands for the ones that fail. |
| `skill install [--dir DIR] [--agent claude\|codex\|all] [--only A,B \| --exclude A,B] [--no-migrate-refs] [--project [--force]]` | Write the bundled agent skills (all by default) to `.claude/skills/<name>` and/or `.agents/skills/<name>`; symlinked layouts are written once. `--project` also generates `yoho-deploy-<app>` from the Yoho file (at the git root unless `--dir`); a rerun keeps hand edits unless `--force`. See [Agents](agents.md). |
| `skill list` | List the bundled skills with descriptions. |
| `skill print [SKILL]` | Print a skill's `SKILL.md` (default `yoho`). |
| `version` | Print the version. |
| `completion` | Shell completion script. |

Run `yoho <command> --help` for the full flag list.

`app logs`, `app stats` and `app exec` read the Service names from the compose files without the full checks of a deploy (no `env_file`, `label_file` or `x-yoho` checks), so they work while a deploy would fail. Services disabled by a compose profile are never deployed and are not listed.

`deploy` and `apply` accept `--refresh` to pull base images and rebuild without cache, including an unchanged Version. It affects Services with `build:` only; bump image-only Service tags explicitly. It conflicts with `--skip-build`. `--scan` scans built images available locally with Trivy or Grype before rollout; findings and scanner failures warn and continue. Both flags require operator access. See [Refreshing base images](builds.md#refreshing-base-images) for scan skips, rollback behavior and a weekly recipe.

With `--json`, refresh emits `deploy.refresh` with `refresh: true`. Each attempted image scan emits `image.scan` with `image`, `scanner`, `critical`, `high` and `ok`; failed scans have `ok: false` and zero counts (not a clean scan). Skipped scans emit a notice or warning and no `image.scan` event.

## Audit log

Every command that changes a Server or runs code on it appends to `<root>/audit/<app>/<destination>.log` on the Destination's first Server: `deploy`, `apply`, `rollback`, `remove`, `app exec`, `app run`, `backup run|restore|drill`, `secrets rotate`, `lock acquire|release` and `proxy boot|upgrade`. Read-only commands, `plan` and `--dry-run` are not logged. `yoho audit` prints it.

The file is JSON lines, mode 0600 inside 0700 directories (`<root>/audit/<app>`), outside the App directory, append-only (one write per line, the line travels on stdin). Each command writes a `started` record before it changes anything and a `finished` record after it, paired by `run`, so a killed command is visible as `unfinished`. A record has `time` (UTC), `run`, `phase`, `performer` (`user@host`, as on the Release), `command`, `app`, `destination`, `version`, `args`, `exit`, `error`, `seconds`, `yoho` (CLI version) and `via` (`cli`). `version` is the Version the command works on: set for `deploy`, `apply` and `rollback`, and, from the deployed Release the command already read, for `secrets rotate`, `backup run`, `backup restore` and `proxy upgrade` (on the `finished` record, which `yoho audit` shows). It is empty for `lock acquire|release`, `proxy boot`, `backup drill`, `app exec|run` and `remove`. The file rotates to `<destination>.log.1` and `.2` at 5 MiB; older records are dropped.

Recorded: the flags you set and the positional arguments (Service, Version, Backup id), and for `app exec` / `app run` the command argv, as Kamal does. Never recorded: secret values. The argv and error text pass the secrets redactor first (the Destination's resolved secret values) and then a pattern pass that masks `NAME=value` for names like password, token, secret or key, `--password VALUE` style flags, `Authorization: Bearer ...` and `user:password@` in URLs. `secrets rotate --generated` records key names only. Environment output of the command itself is never captured. Do not rely on redaction for secrets typed into an argv: prefer `app exec web -- sh -c 'cmd "$VAR"'`.

Logging is best effort: a failed write warns and the command goes on. The audit directories are created on the first write, so a first deploy gets its `started` record, and `yoho remove` keeps the log and appends its own `finished` record after the removal. `yoho remove --volumes` keeps it too; delete `<root>/audit/<app>/<destination>.log` (and its `.1`, `.2`) by hand if you want it gone. Earlier versions kept the log in the App directory as `audit.log`: the next command appends it (with `.1`, `.2`) into the new file once and removes the old one, and `yoho audit` reads it in the meantime.

Members who act through the [Gate](team-access.md) are recorded in `access/audit.log` in a different format (`member`, `verb`, `exit`, `seconds`; see `access audit`). The two share `time`, `app`, `destination`, `args`, `exit`, `error` and `seconds`.

## Plan and apply

Configuration is the desired state. `yoho plan` compares it with the Server and prints Terraform-style changes:

```
  ~   service web [prod]: update in-place (zero downtime)
        image yoho/shop-production-web:f4e6 → yoho/shop-production-web:d459
  -/+ service db [prod]: replace (brief downtime)
        config changed: ports
  -   route shop-production-old: delete

Plan: 0 to add, 1 to change, 1 to replace, 1 to destroy.
```

`+` create, `~` update in place, `-/+` replace (stop then start: non-proxied and Stateful Services), `-` delete. Unchanged resources are hidden; with none changing it prints `No changes. Your Servers match the configuration.` With `--json`, plan emits one `change` event per change and a `plan_summary` event.

When the current Release was deployed through the [Gate](team-access.md), an operator plan names the limits the Gate added and an operator deploy would drop in its own reason, `Gate hardening removed by an operator deploy: cap_drop, cpus, mem_limit, pids_limit, pull_policy`, and warns once; `YOHO_ACCESS=scoped yoho plan` shows what a Gate deploy changes.

Image references in the plan are the ones the Version would build. Uncommitted work uses a version hashed from that content, so repeated plans match until the content changes. Pass `--version` to pin a tag.

## CI check

`yoho config check --offline` needs no SSH key, Docker or password manager. It loads the Yoho file, the compose files and the Destination overlay, validates the schema and `x-yoho`, and checks that every name in `x-yoho.secrets` has a line in `.yoho/secrets` (or `secrets.values`, or is generated) without running any line. It opens no connection and runs no Hook. With `secrets.nonempty` set it skips resolving and prints the warning `secrets.nonempty not verified (--offline)`.

Exit codes: `0` no errors, `1` findings or errors in the checked files (including an invalid `x-yoho` block), `2` the files could not be loaded (unreadable, parse error, failed interpolation, unknown Destination). With `--json` the check result before the final `finished` event is `{"event":"check","ok":true,"errors":0,"warnings":0,"offline":true,"services":3,"secret_keys":5,"time":...}`; findings arrive earlier as `finding` events.

```yaml
# .github/workflows/check.yml
on: pull_request
jobs:
  yoho:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: yoho-build/yoho/.github/actions/setup-yoho@v0.2.0
      - run: yoho config check --offline -d production --json
```

Compose interpolation reads only the App's `.env` and the Destination's `env`, never the job's process environment. Commit those values (non-secret ones only) or put them in `destinations.<name>.env`, as for a deploy.

## Doctor

`yoho doctor [-d DEST] [--max-age DURATION]` is a read-only command that aggregates Yoho's production-readiness checks in one place without resolving secrets from a password manager or modifying the Server. It is safe to run interactively, in CI, or from a Scheduled Job or external monitor.

Checks performed:

- **Config and Backup coverage** (`config`, `backup`): runs the offline `yoho config check` validations and reports Services with no Backup job covering them (Stateful Services first).
- **Backup freshness and restore drill** (`backup`, `drill`): compares each installed Scheduled Job's newest successful Backup against its `OnCalendar` schedule (or `--max-age` if passed) using the Server's clock, flags inactive timers, uninstalled scheduled jobs and unscheduled jobs, and reports the date of the last successful `yoho backup drill` from the audit log (warns when missing or older than 90 days).
- **Firewall and exposed ports** (`firewall`, `ports`): inspects `ufw` state and required ports, and flags any compose Service or running container publishing ports on `0.0.0.0` or `::` outside `yoho-proxy` (which bypasses `ufw`).
- **Disk space** (`disk`): reports free disk on the Docker data root and the Backup cache (`<backup dir>/.restic-cache`), warning at ≥85% used (or <5 GiB free on disks ≥10 GiB) and erroring at ≥95% used (or <1 GiB free on disks ≥2 GiB).
- **Proxy** (`proxy`): verifies `yoho-proxy` is running when the App has proxied Services (or a Tunnel) and compares its image version against the pinned or configured `proxy.image`.
- **Pending secret rotations** (`secrets`): checks for interrupted `yoho secrets rotate --generated` runs (`generated/<KEY>.next`), which block deploys until finished.
- **Image transport** (`transport`): reports the transport path in use (`pussh`, full archive `docker save | docker load`, `registry`, or Server/remote build) and why, checking local `docker pussh` availability and whether the Server's Docker uses the containerd image store.

Each finding has a `severity` (`error`, `warning`, or `info`) and a one-line fix hint when action is needed. With `--json`, `yoho doctor` emits one `finding` event per check result (`check`, `severity`, `level`, `subject`, `message`, optional `hint`) followed by a closing `doctor` summary event (`{"event":"doctor","ok":true,"errors":0,"warnings":1,"info":5,"destination":"production","time":...}`).

Exit codes: `0` when there are no `error` findings (warnings and info exit `0`), `1` when one or more findings have severity `error` (or when configuration or Server connection fails).

## `preview up`, `preview down`, `preview list`, `preview prune`

```sh
yoho preview up --name pr-123 [--pr 123] [--branch BRANCH] [--version VERSION] [--skip-build] [--lock-wait]
yoho preview down --name pr-123 [--yes] [--lock-wait]
yoho preview list
yoho preview prune [--older-than DURATION] [--keep N] [--include-unknown-age] [--yes|--auto-approve] [--lock-wait]
```

Derive an ephemeral Destination from `previews.from`, list its Server markers
(with age, last deploy, and App directory disk usage), remove a verified preview
including its volumes, or bulk-prune stale previews by age or keep-N count.
Names normalize to lowercase letters, digits and dashes, at most 40 characters.
`down --yes` and `prune --yes`/`--auto-approve` confirm data deletion for CI and
are required with `--json`. `up`, `down`, and `prune` emit `preview.up`,
`preview.down`, `preview.prune_plan`, and `preview.prune` JSON events. See
[Previews](previews.md) for configuration (`previews.max_active`), safety limits,
secrets layering and GitHub Actions.

`prune` records one operation under the template Destination (`previews.from`),
starting when the first marked preview is revalidated under its Destination lock.
The record survives preview deletion; query it with
`yoho status -d TEMPLATE --operation ID`. No Server operation record is created
until a marked preview passes that revalidation.

Transport choices appear as `transport` events with `host`, `mode`, `reason` and `hint` under `--json`. Plans report the expected transport; apply/deploy can report further fallbacks if a transfer fails. See [Builds & image transport](builds.md#transport).

## Mesh

All mesh commands operate on every Server in the Yoho file and support global `--json` output.

| Command | Behavior |
| --- | --- |
| `mesh up` | Install tools, generate Server-local keys and reconcile peers, addresses and firewall rules. |
| `mesh status` | Per-peer handshake age, received/transmitted bytes and ping RTT. |
| `mesh rotate-keys` | Generate new keys on every Server and reconcile peer configs. |
| `mesh down` | Remove owned interface, keys, config, unit enablement, state and tagged rules. |

See [WireGuard mesh](mesh.md) for public endpoints, NAT routing and prerequisites.

## Saved plans and operation outcomes

For an unattended operator run, save the exact plan for review, then verify and apply it:

```sh
yoho plan --out /tmp/yoho-plan.json --ttl 1h
yoho apply --plan /tmp/yoho-plan.json --auto-approve --json
```

The version 1 JSON file has `format_version`, `app`, `destination`, `servers`,
`runtime`, `yoho_version`, `config_digest`, `secret_inventory_digest`,
`target_version`, optional git `source_revision`, `build_input_digest`, `changes_digest`, `remote_state`, `changes`, `created_at`
and `expires_at`. Timestamps are UTC RFC 3339. Digests are SHA-256 hex;
`remote_state` maps Server names to Release fingerprints. `changes` contains
`kind`, `name`, `server`, `action`, optional `reasons`, and optional `downtime`
(true means interruption). Descriptive change reasons are SHA-256 hashes, with the complete changes fingerprint in `changes_digest`; structural metadata is preserved. The file contains no secret values. The inventory
digest covers secret names and current Release generation names only.

Apply rejects expired files, another App or Destination, different Yoho versions,
and any difference after re-reading inputs and Server state under the deploy
lock. It prints the changed field names and exits 15 before build or rollout.
Successful verification emits `plan_verified`. The saved file supplies approval: no second prompt or `--auto-approve` is needed. Verification precedes change output and deploy Hooks. A verified zero-change plan returns success without deploying or reconciling resources. `--plan` cannot be combined with
`--version`, `--refresh` or `--skip-build`. Put the artifact outside Docker build contexts (or
exclude it with `.dockerignore`), so writing it does not change build inputs.
`plan --out` refuses an unconverged `app_dir` with exit 10 and a convergence hint. Converge the move before saving a plan. Members may use ordinary `plan`,
but saved plans require an Operator.

Mutating commands print an operation ID before work starts. Raw streams (`app exec/run/stats/logs`, audit output) and read-only modes receive no operation header or final event on stdout. Persistent operation records start only after validation, confirmation, connections and applicable locks, at the first mutation; a local build failure stays a known failure. With `--json`, the
first event is `operation_started` with `operation_id`; subsequent events carry
the same ID. Audit `run` and `operation_id`, and the Release's `operation_id`,
identify the same invocation. Gate deploys also emit their reattachable
`rollout_id`, using that operation ID.

The final `finished` event has `outcome`: `success`, `partial`, `failed` or
`unknown`. A partial result includes `changed` and `unchanged` Service arrays;
`jobs_failed` is partial even though the Release is live. Events include
`schema_version: 1`. `yoho schema --events` prints their versioned JSON Schema
(draft 2020-12). Schema and other raw artifact commands keep their payload format.

After an interrupted operator mutation, reconnect and query:

```sh
yoho status --operation OPERATION_ID --json
```

Operator records live at `<root>/apps/<app>/<destination>/operations/<id>.json`
with `id`, `command`, `started_at`, `state`, `services`, and the outcome fields.
Writes are atomic; the newest 50 records are kept. A running record whose deploy
lock is gone reports `unknown`. A held lock means work may still be running;
wait and query again. Never treat missing records or an unknown outcome as
success, or automatically retry a mutation that may already have run. Scoped
Members recover through the Gate's automatic rollout reattachment and `releases`.

| Exit | Meaning |
| --- | --- |
| 0 | Success |
| 1 | Generic failure |
| 10 | Mutation configuration or validation error |
| 11 | Destination lock held |
| 12 | Cannot connect to the Server before mutation |
| 13 | Partial outcome, including `jobs_failed` |
| 14 | Outcome unknown after mutation began |
| 15 | Stale, expired, incompatible or invalid saved plan |

Codes 10–15 apply to mutating commands; malformed flags/arguments and an unconverged `plan --out` also exit 10. With `--json`, Cobra validation emits one error event. Existing contracts stay unchanged:
`plan --detailed-exitcode` uses 0/1/2 (unchanged/error/changes), `config check`
uses 0/1/2, and `app exec` / `app run` pass through the executed command's code.

## Service placement

`yoho service move SERVICE --to SERVER [--plan] [--auto-approve] [--lock-wait]`
plans or executes a stop-first Stateful Service move with a fresh Backup, an
empty target, health checks and consumer reconciliation. It prints the Role
config edit to commit. `--cleanup-source` is a separate explicit run removing
retained source volumes after a successful move. See [Roles](roles.md).

For Roles Destinations, apply and rollback hold all Server locks in sorted order;
plans and operation Service state include Server placement. Mixed rollout: exit
13; disconnected/unknown outcome: exit 14. Backups follow the volume owner.

For a Roles Destination, `yoho status` shows each proxied Service’s per-Server Proxy and route state; `yoho status --operation ID` reads rollout progress. `yoho releases` includes a Server column.
