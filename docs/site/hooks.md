# Hooks & env vars

Hooks are executable scripts in `.yoho/hooks/<name>` (or `hooks.path`), run on your machine. A missing hook is skipped, a non-executable one warns, a non-zero exit aborts the command. `yoho init` writes samples.

| Hook | When |
|---|---|
| `docker-setup` | during `yoho setup`, Docker install |
| `pre-connect` | before connecting to Servers |
| `pre-build` | before building images |
| `pre-deploy` | before the cutover |
| `post-deploy` | after a successful deploy |
| `pre-app-boot`, `post-app-boot` | around starting new containers |
| `pre-proxy-reboot`, `post-proxy-reboot` | around Proxy (re)boot |
| `pre-backup`, `post-backup` | around a Backup |
| `pre-restore`, `post-restore` | around a restore |

## Hook environment

`YOHO_APP`, `YOHO_DESTINATION`, `YOHO_VERSION`, `YOHO_SERVICE_VERSION` (`<app>@<version>`), `YOHO_HOSTS` (comma-separated), `YOHO_COMMAND`, `YOHO_SUBCOMMAND`, `YOHO_PERFORMER`, `YOHO_RECORDED_AT` (UTC RFC3339), `YOHO_LOCK`, `YOHO_RUNTIME` (seconds, `post-deploy` only). Backup and restore hooks add `YOHO_BACKUP_JOB`, `YOHO_BACKUP_TARGET` and `YOHO_BACKUP_ID` (`post-backup`, `pre-restore`, `post-restore`).

Secrets are never injected into hooks.

## Container environment

Injected into compiled Services unless the compose file sets them: `YOHO_APP`, `YOHO_DESTINATION`, `YOHO_SERVER`, `YOHO_SERVICE`, and `YOHO_VERSION` (only on Services whose image is tagged with the Version, so third-party images are not recreated each deploy). Services also carry labels `yoho.app`, `yoho.destination`, `yoho.service`, `yoho.version`.
