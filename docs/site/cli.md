# CLI reference

Global flags (every command): `-c, --config FILE`, `-d, --destination NAME` (default: the only one), `--json` (NDJSON events), `-v, --verbose` (stream remote output).

| Command | Description |
|---|---|
| `init` | Create `yoho.yml`, `.yoho/secrets`, sample hooks. |
| `config check` | Validate Yoho file, compose files, secret references (runs no secret commands). |
| `config show` | Print the resolved Yoho file with defaults as JSON. |
| `schema [--ext]` | JSON Schema of the Yoho file, or of `x-yoho` with `--ext`. |
| `setup` | Provision a Server (interactive). See [Server setup](setup.md). |
| `deploy [--skip-build] [--version V]` | Build, ship, deploy with zero downtime. Version defaults to the git SHA. |
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
