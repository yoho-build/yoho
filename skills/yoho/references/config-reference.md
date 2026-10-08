# Config reference

The Yoho file (`yoho.yml`, `.yaml`, `.toml`, `.json`, `.jsonc`; format detected by content) is validated by `yoho config check`. Print the JSON Schema with `yoho schema`; for `x-yoho` use `yoho schema --ext`.

## Yoho file

| Key | Notes |
|---|---|
| `app` | Required. `^[a-z0-9][a-z0-9-]*$`. Compose project is `yoho-<app>-<destination>`. |
| `compose` | List of compose files relative to the Yoho file. Default `compose.yaml`, `compose.yml`, `docker-compose.yaml`, `docker-compose.yml`. |
| `servers.<name>` | `ssh` (required, `[user@]host[:port]`, honors `~/.ssh/config`), `sudo` (bool, `sudo -n` for privileged steps), `root` (base dir, default `/var/lib/yoho`), `private_address`, `labels` (reserved). |
| `destinations.<name>` | `servers` (list; exactly one for compose runtime), `runtime` (`compose` default, `swarm`), `env` (non-secret compose interpolation vars), `proxy` (override of `proxy`). |
| `builder` | `location` (`local` default, `remote`, `server`), `engine` (`auto`, `docker`, `container`), `remote` (ssh target), `platforms` (e.g. `[linux/amd64]`), `secrets` (buildx `--secret id=K,env=K`), `exclude` (for `server` builds). |
| `transport.mode` | `auto` (pussh, then save/load, then registry), `pussh`, `load`, `registry`. |
| `registry` | Optional: `server`, `username`, `password_secret`, `prefix`. |
| `secrets.providers.<n>` | `type` (`op`, `bw`, `bws`, `command`), `command` (argv; ref appended), `account`. |
| `secrets.values.<KEY>` | `provider`, `ref`, optional `destinations`. |
| `proxy` | `image` (pinned kamal-proxy), `http_port`, `https_port` (0 disables publishing), `bind`, `tunnel`. |
| `proxy.tunnel` | `token_secret` (empty = Quick Tunnel), `image`, `replicas`. |
| `backups.targets.<n>` | `type` (`restic` default, `archive`), `repository`, `format` (`tar.gz`, `zip`, `7z`; archive only), `password_secret`, `env_secrets`, `keep_last`. |
| `backups.jobs.<n>` | `destination`, `services`, `target`, `schedule` (systemd OnCalendar; empty = on demand). |
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

## x-yoho (per compose Service)

| Key | Notes |
|---|---|
| `proxy` | `hosts`, `port` (80), `health_path` (`/up`), `tls` (Let's Encrypt; off behind a tunnel), `deploy_timeout` (30s), `drain_timeout` (30s). |
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
