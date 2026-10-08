# Configuration

Two places, by design:

- **Yoho file** (`yoho.yml`, `.yaml`, `.toml`, `.json`, `.jsonc`; format detected by content): where and how. Servers, Destinations, Builder, secrets, Proxy, Backups, setup.
- **`x-yoho`** on each compose Service: per-Service behavior.

Validate with `yoho config check`; print the resolved file with `yoho config show`; JSON Schema with `yoho schema` and `yoho schema --ext`. Add `# yaml-language-server: $schema=...` for editor completion.

## Yoho file

| Key | Description |
|---|---|
| `app` | Required. Lowercase letters, digits, dashes. Compose project is `yoho-<app>-<destination>`. |
| `compose` | Compose files relative to the Yoho file. Default `compose.yaml`, `compose.yml`, `docker-compose.yaml`, `docker-compose.yml`. |
| `servers.<name>.ssh` | Required. `[user@]host[:port]`; honors `~/.ssh/config`, agent, ProxyJump. |
| `servers.<name>.sudo` | Use `sudo -n` for privileged steps (setup, schedule install). |
| `servers.<name>.root` | State directory on the Server. Default `/var/lib/yoho`. |
| `servers.<name>.private_address`, `labels` | Reserved for Roles / Swarm placement. |
| `destinations.<name>.servers` | Required. Exactly one for the compose runtime; several for swarm. |
| `destinations.<name>.runtime` | `compose` (default) or `swarm`. Switching causes downtime. |
| `destinations.<name>.env` | Non-secret variables for compose interpolation (image tags, ports). |
| `destinations.<name>.proxy` | Overrides the top-level `proxy` for this Destination. |
| `builder.location` | `local` (default), `remote` (dedicated build Server), `server` (build on the Destination's Server). |
| `builder.engine` | `auto` (Apple `container` on Apple silicon if installed, else Docker), `docker`, `container`. |
| `builder.remote` | SSH target when `location: remote`. |
| `builder.platforms` | e.g. `[linux/amd64]`. Default: the Server's architecture. |
| `builder.secrets` | Keys passed to buildx as `--secret id=KEY,env=KEY`. |
| `builder.exclude` | Paths excluded when syncing source for `location: server`. |
| `transport.mode` | `auto` (pussh, then save/load, then registry), `pussh`, `load`, `registry`. |
| `registry` | Optional: `server`, `username`, `password_secret`, `prefix` (images become `<prefix>/<app>-<service>`). |
| `secrets.providers.<n>` | `type`: `op`, `bw`, `bws`, `command`; `command` (argv, reference appended); `account`. |
| `secrets.values.<KEY>` | `provider`, `ref`, optional `destinations`. |
| `proxy.image` | kamal-proxy image, pinned by default. |
| `proxy.http_port`, `https_port` | Published host ports; default 80/443; `0` disables publishing. |
| `proxy.bind` | Bind address, e.g. `127.0.0.1`. |
| `proxy.tunnel` | Managed Cloudflare Tunnel; see [Cloudflare Tunnel](cloudflare-tunnel.md). |
| `backups.targets.<n>` | `type` (`restic` default, `archive`), `repository`, `format` (`tar.gz`, `zip`, `7z`), `password_secret`, `env_secrets`, `keep_last`. |
| `backups.jobs.<n>` | `destination`, `services`, `target`, `schedule` (systemd `OnCalendar`; empty = on demand). |
| `setup` | `user` (default `yoho`), `authorized_keys`, `packages`, `firewall`, `allow_ports`, `auto_updates`, `swap`, `timezone`. |
| `hooks.path` | Default `.yoho/hooks`. |
| `retain_releases` | Releases kept per Server for rollback. Default 5. |

## x-yoho

| Key | Description |
|---|---|
| `proxy.hosts` | Hostnames routed to the Service. Empty: catch-all. |
| `proxy.port` | Container port. Default 80. |
| `proxy.health_path` | Default `/up`. |
| `proxy.tls` | Let's Encrypt in the Proxy. Leave off behind a tunnel. |
| `proxy.deploy_timeout`, `drain_timeout` | Seconds; default 30 each. |
| `stateful` | Pin to one Server, stop-first recreate, never scaled. |
| `secrets` | List `["KEY", "NAME:KEY"]` or map `{NAME: KEY}`; `NAME` is the name inside the container. |
| `secrets_as_env` | Deliver secrets via env instead of files. |
| `generate` | `{NAME: kind}`: `password32`, `password64`, `hex32`, `hex64`, `base64_32`, `base64_64`. Created once on the Server. |
| `release_command` | One-off container from the new image before cutover (migrations). Failure aborts the deploy. |
| `backup.volumes` | Named compose volumes to archive. |
| `backup.dump` | Command run in the container; stdout is the dump. |
| `backup.pre_backup` | Run before copying volumes. Without it, the container is paused during the copy. |
| `backup.post_restore` | Run after a restore. |
| `backup.restore_dump` | Command receiving the dump on stdin. Empty: dumps stay on the Server. |
| `strict_drain` | Swarm only: Proxy routes to tasks instead of the service VIP. |
| `endpoint` | Reserved for Roles. |

Stateful rule: a Service with volumes deployed to several Servers without `stateful: true` is an error.
