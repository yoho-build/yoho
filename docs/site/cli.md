# CLI reference

Global flags (every command): `-c, --config FILE`, `-d, --destination NAME` (default: the only one), `--json` (NDJSON events), `-v, --verbose` (stream remote output).

| Command | Description |
|---|---|
| `init` | Create `yoho.yml`, `.yoho/secrets`, sample hooks. |
| `config check` | Validate Yoho file, compose files, secret references (runs no secret commands). |
| `config show` | Print the resolved Yoho file with defaults as JSON. |
| `schema [--ext]` | JSON Schema of the Yoho file, or of `x-yoho` with `--ext`. |
| `setup` | Provision a Server (interactive). See [Server setup](setup.md). |
| `plan [--version V] [--detailed-exitcode]` | Read-only diff of the configuration against the Server: containers, Proxy routes, Proxy, Tunnel, Scheduled Jobs. Nothing is built or changed. With `--detailed-exitcode`: exit 0 no changes, 1 error, 2 changes. |
| `apply [--auto-approve] [--version V] [--skip-build]` | Show the plan, ask `yes`, then build, ship, deploy, converge Tunnel and Scheduled Jobs, and remove what was deleted from config. Refuses without a terminal unless `--auto-approve`; `--json` requires `--auto-approve`. |
| `deploy [--skip-build] [--version V]` | Kamal-compatible `apply --auto-approve` without printing the plan. Version defaults to the git SHA. |
| `releases` | List Releases on the Server, newest first. |
| `rollback VERSION` | Redeploy a previous Release. Volumes and migrations are not reverted. |
| `app ps` | List the App's containers. |
| `app logs [SERVICE...] [-f] [-n N]` | Logs; `-n` lines from the end (default 100). |
| `app exec SERVICE -- COMMAND...` | Run a command in a running Service container. |
| `proxy boot \| status \| logs` | Manage the shared kamal-proxy. |
| `tunnel up \| status \| down` | Managed Cloudflare Tunnel. |
| `secrets list` | Keys, sources, lengths. |
| `secrets print KEY... --reveal` | Print values; terminal only. |
| `backup run \| list \| restore` | Backups. |
| `schedule install \| status \| remove` | Scheduled Jobs via systemd timers. |
| `swarm init \| join \| status` | Swarm runtime. |
| `builder status` | Build engine in use and Apple `container` state. |
| `builder setup [-y]` | Install/upgrade Apple container, Rosetta, builder VM; smoke test. |
| `skill install [--dir DIR] [--agent claude\|codex\|all]` | Write the agent skill to `.claude/skills/yoho` and/or `.agents/skills/yoho`. |
| `skill print` | Print the skill's `SKILL.md`. |
| `version` | Print the version. |
| `completion` | Shell completion script. |

Commands added recently (`backup`, `schedule`, `setup`, `swarm`, `tunnel`) have their own flags; see `yoho <command> --help`.

## Plan and apply

Configuration is the desired state. `yoho plan` compares it with the Server and prints Terraform-style changes:

```
  ~   service web [prod]: update in-place (zero downtime)
        image yoho/shop-web:f4e6 → yoho/shop-web:d459
  -/+ service db [prod]: replace (brief downtime)
        config changed: ports
  -   route shop-production-old: delete

Plan: 0 to add, 1 to change, 1 to replace, 1 to destroy.
```

`+` create, `~` update in place, `-/+` replace (stop then start: non-proxied and Stateful Services), `-` delete. Unchanged resources are hidden; with none changing it prints `No changes. Your Servers match the configuration.` With `--json`, plan emits one `change` event per change and a `plan_summary` event.

Image references in the plan are the ones the Version would build; uncommitted work gets a new Version each run, so pass `--version` to compare repeatedly.
