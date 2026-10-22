# Config reference

The Yoho file (`yoho.yml`, `.yaml`, `.toml`, `.json`, `.jsonc`; format detected by content) is validated by `yoho config check`. Print the JSON Schema with `yoho schema`; for `x-yoho` use `yoho schema --ext`.

## Yoho file

| Key | Notes |
|---|---|
| `app` | Required. `^[a-z0-9][a-z0-9-]*$`. Compose project is `yoho-<app>-<destination>`. |
| `compose` | List of compose files relative to the Yoho file. Default `compose.yaml`, `compose.yml`, `docker-compose.yaml`, `docker-compose.yml`. |
| `servers.<name>` | `ssh` (required, `[user@]host[:port]`, honors `~/.ssh/config`), `sudo` (bool, `sudo -n` for privileged steps), `root` (base dir, default `/var/lib/yoho`), `private_address`, `labels` (reserved). |
| `destinations.<name>` | `servers` (list; exactly one for compose runtime; several for swarm, first is the manager, all share one `root`), `runtime` (`compose` default, `swarm`), `env` (non-secret compose interpolation vars), `proxy` (override of `proxy`). |
| `builder` | `location` (`local` default, `remote`, `server`), `engine` (`auto`, `docker`, `container`), `remote` (ssh target; required for `location: remote`), `direct` (`location: remote`: `auto` default streams images from the Builder to the Servers when the Builder can ssh to each without a prompt, else ships through this machine; `true` requires it; `false` never), `platforms` (e.g. `[linux/amd64]`), `secrets` (buildx `--secret id=K,env=K`), `exclude` (`server` builds: extra rsync patterns relative to the transfer root), `gitignore` (`server` builds: default `true`; `false` ships git-ignored files). Server builds sync each build context filtered by its `.dockerignore` (or `<Dockerfile>.dockerignore`) plus Dockerfiles and file secrets; contexts outside the Yoho file's directory (e.g. `context: ../../..` in a monorepo) must stay inside the git work tree. |
| `transport.mode` | `auto` (pussh, then save/load, then registry), `pussh`, `load`, `registry`. |
| `registry` | Optional: `server`, `username`, `password_secret`, `prefix`. |
| `secrets.providers.<n>` | `type` (`op`, `bw`, `bws`, `command`), `command` (argv; ref appended), `account`. |
| `secrets.values.<KEY>` | `provider`, `ref`, optional `destinations`. |
| `proxy` | `image` (pinned kamal-proxy), `http_port`, `https_port` (0 disables publishing), `bind`, `tunnel`. |
| `proxy.tunnel` | `token_secret` (empty or `tunnel: {}` = Quick Tunnel; leave `x-yoho.proxy.hosts` empty with it, one replica only), `image`, `replicas` (default 1). |
| `backups.targets.<n>` | `type` (`restic` default, `archive`), `repository`, `format` (`tar.gz`, `zip`, `7z`; archive only), `password_secret`, `env_secrets`, `rclone_config_secret` (persistent rclone.conf for OAuth remotes such as OneDrive; rclone repositories only), `keep_last`. |
| `backups.jobs.<n>` | `destination`, `services`, `target`, `schedule` (systemd OnCalendar; empty = on demand), `retries` (Scheduled Backups only, 0–5 extra attempts, default 0), `retry_delay_sec` (0/unset = 30s, max 300s). Retries require volume-only selected Services without dump/pre_backup commands. No retry after storage starts or unsafe cleanup; one whole-run timeout/cancellation budget. Reinstall/apply changes. |
| `setup` | `user` (default `yoho`), `authorized_keys`, `packages`, `firewall`, `allow_ports`, `auto_updates`, `swap`, `timezone`. |
| `hooks.path` | Default `.yoho/hooks`. |
| `retain_releases` | Releases kept per Server, default 5. |

## Destinations

`production` is the default Destination. `-d` selects another; when `-d` is omitted, `YOHO_DESTINATION` is used.

Resolution order:

1. `-d`
2. `YOHO_DESTINATION`
3. `production`, when that Destination is defined
4. the only Destination
5. otherwise Yoho errors: several Destinations and `production` is not defined (`choose one with -d`)

An optional overlay next to the Yoho file, `yoho.<destination>.<ext>` (`yml`, `yaml`, `toml`, `json`, or `jsonc`), is deep-merged over the base before decoding. Maps merge recursively; scalars and lists replace. More than one overlay for the same Destination is an error. `yoho config show` prints the selected Destination and overlay path to stderr.

Compose files follow the same idea. After the base files are resolved, Yoho also loads `name.<destination>.ext` when it exists (`compose.yaml` and `compose.staging.yaml` for `-d staging`) and appends those overlays so they override.

Scoped access (sharing one App Destination with teammates or CI, see https://yohobuild.com/team-access/): `access: scoped` routes `deploy`, `rollback`, `releases`, `app ps/logs/stats`, `tui`, `ui` through the Gate as `gate_user` (default `yoho-gate`); every other connecting command fails fast. `YOHO_ACCESS=operator|scoped` overrides it. `ssh: {via: cloudflare, hostname: ssh.example.com}` reaches the Destination's only Server through a Cloudflare Tunnel with `cloudflared access ssh` as ProxyCommand (CI: `TUNNEL_SERVICE_TOKEN_ID`/`TUNNEL_SERVICE_TOKEN_SECRET`).

## x-yoho (per compose Service)

| Key | Notes |
|---|---|
| `proxy` | `hosts`, `port` (80), `health_path` (`/up`), `tls` (Let's Encrypt; off behind a tunnel), `deploy_timeout` (30s), `drain_timeout` (30s). The Service also needs a compose `healthcheck` (error under swarm, warning under compose). The image needs wget or curl: `healthcheck: {test: ["CMD", "wget", "-qO-", "http://127.0.0.1:3000/up"], interval: 5s}`. |
| `proxy.health_port`, `proxy.health_timeout` | Optional per-Service Proxy HTTP probe settings (Compose and Swarm). `health_port`: 1–65535; omitted probes the traffic `port`, explicit `0` is invalid. `health_timeout`: positive Go duration such as `750ms` or `2s`; omitted uses the pinned Proxy default. Empty/zero/negative/unitless/invalid durations fail config check. These do not change the traffic port, overall `deploy_timeout`, Docker `HEALTHCHECK`, or restore readiness. Saved Releases restore these settings on rollback; legacy Releases keep the original probe defaults and saved deploy/drain deadlines. |
| `stateful` | Pinned to one Server, stop-first recreate, never scaled. |
| `secrets` | List `["KEY", "NAME:KEY"]` or map `{NAME: KEY}`. Delivered as files in `/run/secrets/NAME`; use `*_FILE` env vars. |
| `secrets_as_env` | Deliver as env (`env_file`) instead of files. |
| `generate` | `{NAME: kind}`; kinds `password32`, `password64`, `hex32`, `hex64`, `base64_32`, `base64_64`. Created once on the Server. |
| `release_command` | One-off container from the new image before cutover (migrations); failure aborts. |
| `backup` | `volumes`, `dump` (stdout is the dump), `pre_backup`, `post_restore`, `restore_dump` (dump on stdin). Without `pre_backup`, the container is paused while volumes are copied. |
| `strict_drain` | Swarm only: proxy targets tasks. |
| `endpoint` | Reserved for Roles. |

## Secrets

`.yoho/secrets` is dotenv with `$(...)` command substitution; `.yoho/secrets.<destination>` overlays it. Example: `DB_PASSWORD=$(op read op://Production/shop/db)`. Provider-backed keys go in `secrets.values`.
