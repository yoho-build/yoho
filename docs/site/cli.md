# CLI reference

With `access: scoped` (or `YOHO_ACCESS=scoped`) only `deploy`, `rollback`, `releases`, `app ps/logs/stats`, `tui`, `ui` and `config check/show` run, through the Gate; see [Sharing one App with a team](team-access.md).

Global flags (every command): `-c, --config FILE`, `-d, --destination NAME` (default: `$YOHO_DESTINATION`, then `production`, then the only Destination), `--json` (NDJSON events), `-v, --verbose` (stream remote output). On a terminal, `deploy`/`apply` show one live line per built Service (current BuildKit step and elapsed time) that freezes as `✓ web 1m12s`; without a terminal only the start and finish lines print, and `-v` streams the full build output. With `--json` each build emits `build_service` (start), throttled `build_progress` (`service`, `index`, `total`, `step`, `done`, `steps_total`, `cached`; at most one per 500 ms per Service) and `build_service_done` (`service`, `duration_ms`, `cached`, `ok`). Progress needs `docker buildx` with `--progress=rawjson`; older buildx falls back to plain output.

| Command | Description |
|---|---|
| `upgrade` / `update` | Update the running local CLI to the latest stable GitHub release, checking SHA-256 and signed build provenance before atomic replacement. No Yoho file required. Skips current/newer stable versions; package manager installs use their package manager. |
| `init [--template NAME[,NAME...]] [--list-templates]` | Create `yoho.yml`, `.yoho/secrets`, sample hooks. `--template` also writes ready-to-deploy Services (`postgres`, `redis`, `clickhouse`, `rustfs`) into the compose file, appending to an existing one and refusing name conflicts; see [Service templates](getting-started.md#service-templates). |
| `config check [--offline]` | Validate Yoho file, compose files, `x-yoho` and secret names; resolves local secrets only when `secrets.nonempty` is nonempty. `--offline` (or `YOHO_OFFLINE=1`) never resolves secrets. Exit 0 ok, 1 findings, 2 files could not be loaded. Warns when `ssh.via: cloudflare` would go through a `cloudflared` Service in the same App. See [CI check](#ci-check). |
| `config show` | Print the resolved Yoho file with defaults as JSON. |
| `schema [--ext]` | JSON Schema of the Yoho file, or of `x-yoho` with `--ext`. |
| `setup [--plan] [-y] [--containerd-image-store]` | Provision the Destination's Servers (interactive). `--plan` only shows the checklist, `-y` applies every needed step. See [Server setup](setup.md). |
| `plan [--version V] [--detailed-exitcode]` | Read-only diff of the configuration against the Server: containers, Proxy routes, Proxy, Tunnel, Scheduled Jobs. Nothing is built or changed. With `--detailed-exitcode`: exit 0 no changes, 1 error, 2 changes. |
| `apply [--auto-approve] [--version V] [--skip-build] [--cutover-resume] [--cutover-allow-empty]` | Show the plan, ask `yes`, then build, ship, deploy, converge Tunnel and Scheduled Jobs, and remove what was deleted from config. Refuses without a terminal unless `--auto-approve`; `--json` requires `--auto-approve`. With a Destination `migrate` block, cuts over once; see [Migration cutover](configuration.md#migration-cutover-migrate). |
| `deploy [--skip-build] [--version V] [--cutover-resume] [--cutover-allow-empty]` | Kamal-compatible `apply --auto-approve` without printing the plan (also converges Tunnel and Scheduled Jobs, and runs a pending migration cutover). Version defaults to the git SHA. `--cutover-resume` continues an unfinished cutover; `--cutover-allow-empty` accepts a `migrate.stop` that matches no running container. A Gate deploy whose SSH session drops (exit 255) still finishes on the Server; `yoho deploy` reconnects, or `yoho releases` shows the outcome. |
| `releases` | List Releases on the Server, newest first. |
| `rollback VERSION` | Redeploy a previous Release. Volumes and migrations are not reverted. |
| `rollback --cutover [--force]` | Undo the migration cutover: stop this App's containers, start the old ones, restore the saved tunnel ingress. Refuses when the ingress changed since, unless `--force`. |
| `migrate plan [--cutover-allow-empty]` | Run the migration cutover preflight alone (read-only): old containers to stop, claimed-data conflicts, ingress rules to repoint. `--cutover-allow-empty` accepts a `migrate.stop` that matches no running container. |
| `migrate status` | Print the cutover journal (`<app dir>/cutover.json`). |
| `migrate draft --from FILE [--out DIR] [--service NAME] [--project NAME]` | Print `compose.yaml` and `yoho.yml` from a names-only inventory JSON, or from one service's `railway.json` / `railway.toml`. `--out` writes those two files only when neither exists. Does not write `.yoho/secrets`, contact Railway, or contact a Server. See [Migrating from Railway](migrating-from-railway.md). |
| `app ps` | List the App's containers. |
| `app logs [SERVICE...] [--all] [-f] [-n N] [--since T] [--until T] [-t] [--grep TEXT [-i] [-E]]` | Logs of the named Services; `-f`/`--follow`, `-n`/`--lines` from the end (default 100; `0` for none, so `-f -n 0` shows only new lines; `-1` for every line, at most 10000 through the Gate), `-t`/`--timestamps`. `-f` runs until Ctrl-C, SIGTERM or the connection drops; the Server then stops `docker logs` (and `grep`) itself, even for a Service that logs nothing. `--since`/`--until` take what docker takes: a duration back from now (`30s`, `10m`, `2h`) or a timestamp (`2026-11-03T15:04:05Z`, `2026-11-03`, Unix seconds); `--until` is compose only and cannot be combined with `-f` (`--until cannot be combined with -f`: it ends the output at a point in the past; use `--since` with `-f`). `--grep TEXT` keeps lines containing the fixed string TEXT, filtered on the Server with `grep` (also with `-f`); `-i`/`--ignore-case`, `-E`/`--regex` for an extended regular expression. Without `-n`, `--since` and `--grep` show every line they select (add `-n` to bound the output, e.g. `--since 10m -n 200`); with `--grep` and no `-f`, `-n` counts matching lines. A bad `-E` pattern fails before docker runs, with `invalid --grep pattern` (exit 2). `grep` runs under `nice`; without `-f` the Server stops a search after 60 seconds (where `timeout` exists, as in coreutils and BusyBox) with `--grep search took longer than 60s` (exit 124): narrow it with `--since` or `-n`. Through the Gate (a Member), a search without `-f` reads at most the last 100000 lines of the logs, `-E` patterns may not use back-references (`\1` to `\9`), and a Gate older than the CLI refuses `--since`, `--until` and `--grep` (`unknown field`) until the operator runs `yoho access install` again. With one Service it is implied; with several, name one or more (the error lists them) or pass `--all` for every Service interleaved (compose only; swarm takes exactly one Service). |
| `app stats [SERVICE...] [--no-stream]` | CPU, memory, network and block I/O of the App's containers (default: every Service) on the Destination's first Server. Live view in a terminal; one snapshot with `--no-stream`, without a terminal, or when `CI` is set or `TERM=dumb` (use this from scripts and coding agents); `--json` prints docker's JSON object per container. Swarm: only tasks on the first Server. |
| `app exec SERVICE -- COMMAND...` | Run a command in a running Service container (`docker compose exec -T`; under swarm, in a running task on the first Server that has one). An unknown SERVICE fails before connecting, with the list of Services. The command's output and exit code are passed through: a failing command (or `service "web" is not running`) prints only its own stderr and exits with its code. |
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
| `secrets list` | Keys, sources, lengths. |
| `secrets print KEY... --reveal` | Print values; terminal only. |
| `secrets rotate [SERVICE...] [--dry-run]` | Re-resolve secrets (no rebuild), restart only Services whose values changed, record the current Release again. Needs a checkout of the deployed Version. Named Services must be the only ones that changed. `--dry-run` lists what would restart. |
| `secrets rotate --generated KEY [--generated KEY...] [--dry-run]` | Replace an `x-yoho.generate` secret: apply the new value to the database (Postgres, MySQL/MariaDB, or an `x-yoho.rotate` command), then swap it in and restart every Service that uses it. A failed database step keeps the old value; a re-run reuses the pending one. See [Secrets](secrets.md#rotating-generated-secrets). |
| `backup run [JOB]` | Take a Backup now. |
| `backup list [JOB]` | Stored Backups, newest first. |
| `backup pull [JOB] [--to DIR] [--limit N]` | Mirror new archives of an `archive` target to a local directory; never deletes. |
| `backup restore ID [--job JOB] [-y]` | Restore a Backup (`latest` for the newest); asks for confirmation unless `-y`. |
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

Image references in the plan are the ones the Version would build. Uncommitted work uses a version hashed from that content, so repeated plans match until the content changes. Pass `--version` to pin a tag.

## CI check

`yoho config check --offline` needs no SSH key, Docker or password manager. It loads the Yoho file, the compose files and the Destination overlay, validates the schema and `x-yoho`, and checks that every name in `x-yoho.secrets` has a line in `.yoho/secrets` (or `secrets.values`, or is generated) without running any line. It opens no connection and runs no Hook. With `secrets.nonempty` set it skips resolving and prints the warning `secrets.nonempty not verified (--offline)`.

Exit codes: `0` no errors, `1` findings or errors in the checked files (including an invalid `x-yoho` block), `2` the files could not be loaded (unreadable, parse error, failed interpolation, unknown Destination). With `--json` the last event before the process ends is `{"event":"check","ok":true,"errors":0,"warnings":0,"offline":true,"services":3,"secret_keys":5,"time":...}`; findings arrive earlier as `finding` events.

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
