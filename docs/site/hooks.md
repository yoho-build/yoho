# Hooks & env vars

Hooks are executable scripts in `.yoho/hooks/<name>` (or `hooks.path`), run on your machine. A missing hook is skipped, a non-executable one warns, a non-zero exit aborts the command with `hook <name> failed (exit N), aborting`. `yoho init` writes an executable `<name>.sample` for every hook, listing when it runs and its env; drop `.sample` to enable one (`mv .yoho/hooks/pre-deploy.sample .yoho/hooks/pre-deploy`).

Names match Kamal's (`.kamal/hooks`), with `YOHO_*` for `KAMAL_*`.

| Hook | When |
|---|---|
| `docker-setup` | reserved for `yoho setup`; not run yet |
| `pre-connect` | before connecting to Servers: `deploy`, `rollback`, `proxy boot`, `secrets rotate` |
| `pre-build` | before building images (`deploy`) |
| `pre-deploy` | `deploy`, `rollback`, `secrets rotate`: after the Destination lock is taken and the Release is prepared or checked, before the Proxy boot and any container change |
| `pre-app-boot` | after `pre-deploy` and the Proxy boot, before any new container starts (dependencies, release commands, the new Service containers) |
| `post-app-boot` | after the new containers are healthy, routed, the old ones removed and the Release recorded |
| `post-deploy` | after `post-app-boot`, last; `YOHO_RUNTIME` is set |
| `pre-proxy-reboot`, `post-proxy-reboot` | around each Server's Proxy boot in `yoho proxy boot`, every time; during `deploy` and `rollback` only when an existing Proxy is recreated because its configuration changed (a first boot or a plain start runs no hook) |
| `pre-backup`, `post-backup` | around a Backup |
| `pre-restore`, `post-restore` | around a restore (`post-restore` runs with consumers still stopped) |
| `pre-cutover` | after the migration cutover preflight, before the old containers stop (`migrate`) |
| `post-cutover` | after the migration cutover is done |

A `deploy` runs `pre-connect`, `pre-build`, `pre-deploy`, (`pre-proxy-reboot`, `post-proxy-reboot`), `pre-app-boot`, `post-app-boot`, `post-deploy`. A `rollback` runs the same without `pre-build`, with `YOHO_COMMAND=rollback` and `YOHO_VERSION` the Release rolled back to (in `pre-connect`, the version as typed). How often:

- compose runtime: a Destination has one Server, so each hook runs once per deploy.
- swarm runtime: `pre-deploy`, `pre-app-boot`, `post-app-boot` and `post-deploy` run once per deploy with every Server in `YOHO_HOSTS`; the proxy hooks run once per Server whose Proxy is rebooted, with that Server in `YOHO_HOSTS`.
- `yoho proxy boot` boots one Server at a time: `pre-proxy-reboot`, boot, `post-proxy-reboot`, then the next Server.

A failing `pre-*` hook stops the command before its step: no container or Proxy changes after a failing `pre-deploy`, no new container starts after a failing `pre-app-boot`, and the Proxy is left as it was after a failing `pre-proxy-reboot`. A failing `post-app-boot` or `post-deploy` fails the command, but the new Release is already live and recorded.

Scoped Members (team access) run `pre-connect`, `pre-build`, `pre-deploy` and `post-deploy` around the Gate call for `deploy`.

## Hook environment

| Variable | Value |
|---|---|
| `YOHO_APP`, `YOHO_DESTINATION` | the App and Destination |
| `YOHO_VERSION`, `YOHO_SERVICE_VERSION` | the Version, and `<app>@<version>` |
| `YOHO_HOSTS` | comma-separated Servers as `host` or `host:port` from their ssh target, without the user (`root@203.0.113.10:2222` becomes `203.0.113.10:2222`); the same in every hook |
| `YOHO_COMMAND` | `deploy`, `rollback`, `proxy`, `backup`, `secrets rotate` |
| `YOHO_SUBCOMMAND` | `boot` for `yoho proxy boot`; `run` or `restore` for Backups |
| `YOHO_PERFORMER` | who runs Yoho |
| `YOHO_RECORDED_AT` | call time, UTC RFC3339 |
| `YOHO_LOCK` | `true` while this command holds the Destination lock, otherwise `false` (see below) |
| `YOHO_RUNTIME` | seconds since the deploy or rollback started (`post-deploy` only) |
| `YOHO_CUTOVER` | the cutover id (`pre-cutover`, `post-cutover`) |

Backup and restore hooks add `YOHO_BACKUP_JOB`, `YOHO_BACKUP_TARGET` and `YOHO_BACKUP_ID` (`post-backup`, `pre-restore`, `post-restore`).

`YOHO_LOCK` is `true` in `pre-deploy`, `pre-app-boot`, `post-app-boot`, `post-deploy` and the proxy hooks of a `deploy`, `rollback` or `secrets rotate`, in `post-restore`, and in `pre-cutover` and `post-cutover`. It is `false` in `pre-connect`, `pre-build`, `pre-backup`, `post-backup`, `pre-restore`, the hooks of `yoho proxy boot` (which takes no Destination lock), and for scoped Members (the Gate holds the lock on the Server).

Secrets are never injected into hooks.

Changed in this version: `YOHO_HOSTS` in backup hooks used to include the ssh user (`root@host:port`); it now has the same `host[:port]` form as deploy hooks. `YOHO_LOCK` used to be unset. `pre-app-boot`, `post-app-boot`, `pre-proxy-reboot` and `post-proxy-reboot` used to be listed but never run, and `rollback` ran no hooks.

## Container environment

Injected into compiled Services unless the compose file sets them: `YOHO_APP`, `YOHO_DESTINATION`, `YOHO_SERVER`, `YOHO_SERVICE`, and `YOHO_VERSION` (only on Services whose image is tagged with the Version, so third-party images are not recreated each deploy). Services also carry labels `yoho.app`, `yoho.destination`, `yoho.service`, `yoho.version`.
