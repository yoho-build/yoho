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
| `destinations.<name>.servers` | Required. Exactly one for the compose runtime; several for swarm (first is the manager; all share one `root`). |
| `destinations.<name>.runtime` | `compose` (default) or `swarm`. Switching causes downtime. |
| `destinations.<name>.env` | Non-secret variables for compose interpolation (image tags, ports). |
| `destinations.<name>.proxy` | Overrides the top-level `proxy` for this Destination. |
| `destinations.<name>.app_dir` | Optional absolute directory on the Server for this Destination's App directory, outside `root`. See [App directory](#app-directory-app_dir). |
| `builder.location` | `local` (default), `remote` (dedicated build Server), `server` (build on the Destination's Server). |
| `builder.engine` | `auto` (Apple `container` on Apple silicon if installed, else Docker), `docker`, `container`. |
| `builder.remote` | SSH target; required when `location: remote`. |
| `builder.platforms` | e.g. `[linux/amd64]`. Default: the Server's architecture. |
| `builder.secrets` | Keys passed to buildx as `--secret id=KEY,env=KEY`. |
| `builder.exclude` | Extra rsync exclude patterns for `location: server`, relative to the transfer root. See [Server builds](builds.md#server-builds). |
| `builder.gitignore` | `location: server`: skip git-ignored paths (default `true`); `false` ships them. |
| `transport.mode` | `auto` (pussh, then save/load, then registry), `pussh`, `load`, `registry`. |
| `registry` | Optional: `server`, `username`, `password_secret`, `prefix` (Yoho-built images become `<prefix>/<app>-<destination>-<service>:<version>`, so Destinations that share a Server never share a tag). |
| `secrets.providers.<n>` | `type`: `op`, `bw`, `bws`, `command`; `command` (argv, reference appended); `account`. |
| `secrets.nonempty` | Opt-in list of locally resolved source keys that must exist and not equal `""`; duplicates/invalid names fail. Whitespace is valid; Server-generated keys are unsupported. See [Secrets](secrets.md#requiring-nonempty-values-opt-in). |
| `secrets.values.<KEY>` | `provider`, `ref`, optional `destinations`. |
| `proxy.image` | kamal-proxy image, pinned by default. |
| `proxy.http_port`, `https_port` | Published host ports; default 80/443; `0` disables publishing. |
| `proxy.bind` | Bind address, e.g. `127.0.0.1`. |
| `proxy.tunnel` | Managed Cloudflare Tunnel: `token_secret` (empty or `tunnel: {}` = Quick Tunnel), `image`, `replicas` (default 1). See [Cloudflare Tunnel](cloudflare-tunnel.md). |
| `backups.targets.<n>` | `type` (`restic` default, `archive`), `repository`, `format` (`tar.gz`, `zip`, `7z`), `password_secret`, `env_secrets`, `keep_last`. |
| `backups.jobs.<n>` | `destination`, `services`, `target`, `schedule` (systemd `OnCalendar`; empty = on demand). |
| `setup` | `user` (default `yoho`), `authorized_keys` (unset: your `~/.ssh/*.pub`; `[]`: none), `packages`, `firewall`, `allow_ports`, `auto_updates`, `swap`, `timezone`. |
| `hooks.path` | Default `.yoho/hooks`. |
| `retain_releases` | Releases kept per Server for rollback. Default 5. |

## Destinations

`production` is the default Destination. `-d` (or `--destination`) selects another; when `-d` is omitted, `YOHO_DESTINATION` is used.

Resolution order:

1. `-d`
2. `YOHO_DESTINATION`
3. `production`, when that Destination is defined
4. the only Destination
5. otherwise Yoho errors: several Destinations and `production` is not defined (`choose one with -d`)

An optional overlay next to the Yoho file, `yoho.<destination>.<ext>` (`yml`, `yaml`, `toml`, `json`, or `jsonc`), is deep-merged over the base before decoding. Maps merge recursively; scalars and lists replace. More than one overlay for the same Destination is an error. `yoho config show` prints the selected Destination and overlay path to stderr. The secrets file `.yoho/secrets.<destination>` is layered over `.yoho/secrets` the same way.

Compose files follow the same idea. After the base files are resolved, Yoho also loads `name.<destination>.ext` when it exists (`compose.yaml` and `compose.staging.yaml` for `-d staging`) and appends those overlays so they override.

### App directory (`app_dir`)

Each Destination keeps its Releases, secrets generations, generated secrets, `hmac.key`, Scheduled Job credentials and Server build source in its App directory, `<root>/apps/<app>/<destination>` by default. `app_dir` moves that directory somewhere else, for example to keep one directory per project on the Server:

```yaml
destinations:
  staging:
    servers: [small-deb]
    app_dir: /home/deb/apps/elpoca-engine
```

Nothing else changes: the compose project `yoho-<app>-<destination>`, volume names (volume data never moves), Proxy routes, Scheduled Job units and `<root>/backups`, `<root>/jobs`, `<root>/bin`, `<root>/proxy` stay where they are.

How it works: `<root>/apps/<app>/<destination>` stays the path Yoho uses everywhere (Release records, compiled compose files, image retention, `yoho schedule run`, Yoho output), and becomes a symlink to `app_dir`. Retained Releases keep working after a later move, because their compiled files name the canonical path. The "App directory" step of deploy, apply and rollback prints the physical directory.

Rules (checked by `yoho config check` and again on the Server):

- An absolute, clean path (no trailing slash, `//`, `.` or `..`) of letters, digits, `_`, `.`, `/` and `-`, not `/`.
- Outside the Server `root`: not equal to it, inside it, or containing it. The Server also compares the resolved physical paths, so a symlinked parent cannot sneak `app_dir` into `root`. A sibling such as `/home/deb/apps/elpoca-engine` next to `/home/deb/apps/yoho` is fine.
- No two Destinations sharing a Server may use the same or nested `app_dir`.
- The Destination name must match the App name pattern (`^[a-z0-9][a-z0-9-]*$`), and `root` must use the same safe characters.
- Every existing parent must be owned by root or the deploy user and not group- or other-writable (owned sticky directories such as `/tmp` are allowed as parents). Missing parents are created one at a time with mode 0700. `app_dir` itself must not be a symlink.
- `app_dir` must not exist yet, or be an empty directory. Yoho owns it completely and marks it with `.yoho-app.json` (`{"app","destination"}`). A non-empty directory without that marker (a git checkout, someone's files) or with another App's or Destination's marker is refused. Yoho writes the marker into default App directories too.
- The canonical symlink is trusted only when the deploy user owns it, it points at a clean absolute path that passes the parent rules, and that directory holds this App and Destination's marker. Otherwise every command refuses, including Backups.

**Changing `app_dir` moves the directory.** `yoho plan` shows `app_dir: <old> → <new>`, and `yoho apply` asks for confirmation like any other change. The move runs in `yoho deploy`, `yoho apply` (before the build step), `yoho rollback` and `yoho schedule install`, on every Server of the Destination. Setting `app_dir` on a deployed Destination moves the existing directory into it; changing it moves it again; removing it moves it back to `<root>/apps/<app>/<destination>`. It never starts empty and never copies. Running containers are not recreated: a same-filesystem rename keeps their bind mounts.

The move waits for nothing and refuses when the App directory is in use: a `lock/` directory (a deploy, rollback, Backup or Scheduled Job running, or a crashed one) or a `source.run-*` directory (a Server build). The refusal names the path; remove stale ones by hand and rerun. Coordination lives outside the moving tree, in `<root>/appdirs/<app>/` (created by `yoho setup`, otherwise on first use): a gate that every lock taker passes through, and a migration journal `migrate.json` written before the first rename. While a journal exists, deploys, rollbacks, Backups, Scheduled Jobs and Server builds refuse ("migration in progress or interrupted"); the next `yoho deploy` or `yoho apply` resumes it from whatever state it finds, and refuses with the journal and the observed paths when the state is not one it expects. A gate left by a killed run is named in the error; remove it by hand. Image pruning is skipped with a warning while another Destination of the App is moving.

**Upgrade every CLI first.** Older Yoho binaries do not know the gate. Upgrade every machine that deploys this App before you set or change `app_dir`. After the move, older binaries still work through the symlink, but a move racing an older binary is unsupported.

#### Same filesystem only

Yoho moves the App directory with a rename, so `app_dir` (its parent, for a new directory) must be on the same filesystem as the current App directory, and `<root>/appdirs` on the same filesystem as `<root>/apps/<app>`. Otherwise it refuses ("on different filesystems") and changes nothing. It never copies and deletes automatically. To move across filesystems, in a maintenance window:

1. Stop traffic or accept downtime: containers bind-mount files from the App directory.
2. On the Server, with no deploy, Backup or Scheduled Job running, copy the current App directory into a new empty `app_dir`, preserving modes and ownership (`cp -a <root>/apps/<app>/<destination>/. /new/app_dir/`), and check the copy.
3. Replace the canonical path with a symlink: `mv <root>/apps/<app>/<destination> <root>/apps/<app>/<destination>.old && ln -s /new/app_dir <root>/apps/<app>/<destination>`. The new directory must hold `.yoho-app.json`, be owned by the deploy user and have mode 0700.
4. Restart (or recreate) the containers of the compose project `yoho-<app>-<destination>`: a running container keeps the bind mounts it resolved when it started, which still point into the old copy.
5. Set `app_dir: /new/app_dir` and run `yoho plan`: it shows no `app_dir` change, because the link already points at `app_dir`.
6. Remove `<root>/apps/<app>/<destination>.old` once everything works.

### Scoped access and SSH routing (`access`, `gate_user`, `ssh`)

```yaml
destinations:
  production:
    servers: [home]
    access: scoped                                   # operator (default) | scoped
    gate_user: yoho-gate                             # default
    ssh: {via: cloudflare, hostname: ssh.example.com}
```

- `access: scoped` sends `deploy`, `rollback`, `releases`, `app ps/logs/stats`, `tui` and `ui` through the Gate as the `gate_user`, and makes every other command that connects fail fast. `YOHO_ACCESS=operator|scoped` overrides it per shell. This option needs the compose runtime. See [Sharing one App with a team](team-access.md).
- `ssh.via: cloudflare` reaches the Destination's only Server through a Cloudflare Tunnel `ssh://` ingress (`cloudflared access ssh --hostname …` as ssh's ProxyCommand), on the operator path and the scoped path. `hostname` must be a plain DNS name.

## x-yoho

| Key | Description |
|---|---|
| `proxy.hosts` | Hostnames routed to the Service. Empty: catch-all. |
| `proxy.port` | Container port. Default 80. |
| `proxy.health_path` | Default `/up`. |
| `proxy.health_port` | Optional Proxy HTTP probe port, 1–65535. Omitted: use `proxy.port` (traffic still uses `proxy.port`). Explicit `0` is invalid. |
| `proxy.health_timeout` | Optional positive Go duration per Proxy HTTP probe, e.g. `750ms` or `2s`. Omitted: pinned kamal-proxy default. Empty, zero, negative and unitless values are invalid. Separate from `deploy_timeout` and Docker `healthcheck.timeout`. |
| `proxy.tls` | Let's Encrypt in the Proxy. Leave off behind a tunnel. |
| `proxy.deploy_timeout`, `drain_timeout` | Seconds; default 30 each. |
| `stateful` | Pin to one Server, stop-first recreate, never scaled. |
| `secrets` | List `["KEY", "NAME:KEY"]` or map `{NAME: KEY}`; `NAME` is the name inside the container. |
| `secrets_as_env` | Default `false`: mount secret files. `true`: use `env_file`; values become container environment variables visible through `docker inspect`. |
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

`yoho config check` also warns when a Service's `environment` (including `env_file` values) uses another Service's name, `container_name` or network alias as a host, such as `postgres://u@analytics-db:5432/x`, without a `depends_on` path to it. Compose may then start the Service first, and the deploy fails as "did not become healthy". Add `depends_on: {analytics-db: {condition: service_healthy}}`; under Swarm, where `depends_on` is ignored, make the app retry.

A proxied Service needs a compose `healthcheck` (an error under swarm, a warning under compose). This example executes `wget`, so the image must include it. For a curl-only image, change the healthcheck test to execute `curl` instead:

```yaml
services:
  web:
    image: shop
    healthcheck: {test: ["CMD", "wget", "-qO-", "http://127.0.0.1:3000/up"], interval: 5s}
    x-yoho:
      proxy: { hosts: [shop.example.com], port: 3000 }
```

## Files from the App directory

Compose resolves `./relative` bind sources on your machine; the Server has no such path. Yoho ships them instead:

- A **read-only** bind mount whose source is inside the App directory (a file or a directory tree), and a top-level `configs:` `file:` there, is uploaded with each deploy to `<root>/apps/<app>/<destination>/files/<hash>/<name>` with its file modes, and the compiled compose points at that copy. `.yoho/` is never shipped.
- The directory is named after a hash of the content, so unchanged files keep their path (no container recreate) and a change shows in `yoho plan` as `config changed: labels, volumes`. Each Release keeps the files it was deployed with, so `yoho rollback` mounts the old content; they are removed when no retained Release or container uses them.
- The total is capped at 50 MB. Bake larger files into an image or keep them in a named volume.
- A **writable** bind mount inside the App directory is an error: state belongs in a named volume.
- Sources outside the App directory (`/var/run/docker.sock`, `/etc/ssl/certs`) are paths on the Server and are left alone. `yoho config check` warns when one looks like a path on your machine (`/Users/...`, `/Volumes/...`, your home directory).
- Under `runtime: swarm` bind mounts from the App directory are an error; Yoho does not ship files to swarm nodes.

```yaml
services:
  clickhouse:
    image: clickhouse/clickhouse-server:24
    volumes:
      - ./clickhouse/init.sql:/docker-entrypoint-initdb.d/init.sql:ro  # shipped per Release
      - chdata:/var/lib/clickhouse                                      # state: named volume
volumes:
  chdata:
```
