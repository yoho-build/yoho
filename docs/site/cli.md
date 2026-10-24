# CLI reference

With `access: scoped` (or `YOHO_ACCESS=scoped`) only `deploy`, `rollback`, `releases`, `app ps/logs/stats`, `tui`, `ui` and `config check/show` run, through the Gate; see [Sharing one App with a team](team-access.md).

Global flags (every command): `-c, --config FILE`, `-d, --destination NAME` (default: `$YOHO_DESTINATION`, then `production`, then the only Destination), `--json` (NDJSON events), `-v, --verbose` (stream remote output).

| Command | Description |
|---|---|
| `init` | Create `yoho.yml`, `.yoho/secrets`, sample hooks. |
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
| `app logs [SERVICE...] [--all] [-f] [-n N]` | Logs of the named Services; `-f`/`--follow`, `-n`/`--lines` from the end (default 100). With one Service it is implied; with several, name one or more (the error lists them) or pass `--all` for every Service interleaved (compose only; swarm takes exactly one Service). |
| `app stats [SERVICE...] [--no-stream]` | CPU, memory, network and block I/O of the App's containers (default: every Service) on the Destination's first Server. Live view in a terminal; one snapshot with `--no-stream`, without a terminal, or when `CI` is set or `TERM=dumb` (use this from scripts and coding agents); `--json` prints docker's JSON object per container. Swarm: only tasks on the first Server. |
| `app exec SERVICE -- COMMAND...` | Run a command in a running Service container. |
| `tui [--interval 5s] [-n LINES]` | Live, read-only terminal dashboard: every Server of the Destination (load, memory, disk, Docker), the App's containers with health, CPU and memory, the Proxy, Releases, and container logs. Keys: `tab`/`←→` Server, `↑↓` select, `d` Service details (CPU, memory, network/disk I/O, PIDs), `enter` logs, `r` refresh, `q` quit. Needs a terminal. See [Dashboards](dashboards.md). |
| `ui [--port 0] [--no-open] [--interval 5s]` | The same view in the browser, served on `127.0.0.1` from this machine with a one-time login link. Read-only; nothing runs on the Servers. See [Dashboards](dashboards.md). |
| `access install [--binary PATH]` | Operator: set up the Gate on the Server (yoho-gate user, sudoers rule, root-owned `/usr/local/bin/yoho`). Idempotent; needs root or sudo. See [Sharing one App with a team](team-access.md). |
| `access secrets push` | Operator: resolve secrets locally and store them, with Proxy, Registry and hostname settings (including a catch-all when `custom_domains` is set), for scoped deploys. |
| `access add MEMBER [--verbs all\|read\|deploy,logs,...]` / `access rm MEMBER` / `access ls` | Operator: grant, revoke and list Members' verbs on the App Destination. |
| `access key add MEMBER PUBKEY_FILE` / `access key rm MEMBER` | Operator: authorize or remove a Member's restricted SSH key (forced command `yoho gate`). |
| `access audit [-n 50]` | Operator: last Gate requests (JSON lines). |
| `proxy boot \| status \| logs` | Manage the shared kamal-proxy. |
| `tunnel up \| status \| down` | Managed Cloudflare Tunnel (cloudflared connector; Quick Tunnel without a token). |
| `secrets list` | Keys, sources, lengths. |
| `secrets print KEY... --reveal` | Print values; terminal only. |
| `secrets rotate [SERVICE...] [--dry-run]` | Re-resolve secrets (no rebuild), restart only Services whose values changed, record the current Release again. Needs a checkout of the deployed Version. Named Services must be the only ones that changed. `--dry-run` lists what would restart. |
| `backup run [JOB]` | Take a Backup now. |
| `backup list [JOB]` | Stored Backups, newest first. |
| `backup pull [JOB] [--to DIR] [--limit N]` | Mirror new archives of an `archive` target to a local directory; never deletes. |
| `backup restore ID [--job JOB] [-y]` | Restore a Backup (`latest` for the newest); asks for confirmation unless `-y`. |
| `schedule install [JOB...] [--binary PATH]` | Install systemd timers for Backup jobs with a schedule (default: all of the Destination). |
| `schedule status` | Installed Scheduled Jobs, timers, last runs. |
| `schedule remove [JOB...]` | Disable and delete Scheduled Jobs (default: all of the Destination). |
| `swarm init [-y]` | Initialize a Swarm on the first Server (the manager). Errors unless `runtime` is `swarm`. |
| `swarm join [-y]` | Join the other Servers as workers. Errors unless `runtime` is `swarm`. |
| `swarm status` | Swarm nodes and the App's services. |
| `builder status` | Build engine in use and Apple `container` state. |
| `builder setup [-y]` | Install/upgrade Apple container, Rosetta, builder VM; smoke test. |
| `skill install [--dir DIR] [--agent claude\|codex\|all] [--only A,B \| --exclude A,B] [--no-migrate-refs] [--project [--force]]` | Write the bundled agent skills (all by default) to `.claude/skills/<name>` and/or `.agents/skills/<name>`; symlinked layouts are written once. `--project` also generates `yoho-deploy-<app>` from the Yoho file (at the git root unless `--dir`); a rerun keeps hand edits unless `--force`. See [Agents](agents.md). |
| `skill list` | List the bundled skills with descriptions. |
| `skill print [SKILL]` | Print a skill's `SKILL.md` (default `yoho`). |
| `version` | Print the version. |
| `completion` | Shell completion script. |

Run `yoho <command> --help` for the full flag list.

`app logs` and `app stats` read the Service names from the compose files without the full checks of a deploy (no `env_file`, `label_file` or `x-yoho` checks), so they work while a deploy would fail. Services disabled by a compose profile are never deployed and are not listed.

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
