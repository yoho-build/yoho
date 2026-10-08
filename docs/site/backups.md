# Backups & restore

A Backup is a point-in-time copy of selected volumes and dumps, taken with the Service quiesced or paused, following the ONCE contract.

## Per Service

```yaml
x-yoho:
  stateful: true
  backup:
    dump: ["pg_dump", "-U", "postgres", "postgres"]
    restore_dump: ["psql", "-U", "postgres", "postgres"]
    volumes: [pgdata]
    pre_backup: ["sh", "-c", "sync"]    # optional; otherwise the container is paused
    post_restore: ["sh", "-c", "echo restored"]
```

## Targets and jobs (yoho.yml)

```yaml
secrets:
  providers: { vault: { type: op } }
  values:
    BACKUP_PASSWORD: { provider: vault, ref: "op://Production/shop-backups/password" }
backups:
  targets:
    offsite:
      type: restic                       # or archive
      repository: "rclone:onedrive:backups/shop"   # s3:, b2:, sftp:, rclone:, or a path
      password_secret: BACKUP_PASSWORD
      env_secrets: [RCLONE_CONFIG_ONEDRIVE_TOKEN]
      keep_last: 14
  jobs:
    nightly: { destination: production, target: offsite, schedule: "*-*-* 03:00:00" }
```

- `restic` (default): encrypted, deduplicated.
- `archive`: files at a path or rclone remote in `tar.gz` (default `format`), AES-256 `zip`, or `7z` (never ZipCrypto); set `format` to choose; `keep_last` trims old files.

## Commands

```sh
yoho backup run [JOB]                      # take a Backup now
yoho backup list [JOB]                     # stored Backups, newest first
yoho backup restore ID [--job JOB] [-y]    # restore; confirmation required unless -y
```

`JOB` defaults to the Destination's only Backup job (several jobs: name one; `restore` takes `--job`). Pass `-d` to pick the Destination. `ID` is the id shown by `backup list`; `latest` selects the newest. Hooks `pre-backup`, `post-backup`, `pre-restore`, `post-restore` run on your machine and also get `YOHO_BACKUP_JOB`, `YOHO_BACKUP_TARGET` and (after the Backup, or for a restore) `YOHO_BACKUP_ID`.

## Swarm

On a Destination with `runtime: swarm`, Backups run on the first Server. Stateful Services are pinned there, so that host has the task container and the volume. The container is the running task (`docker ps` filtered by `com.docker.swarm.service.name=<project>_<service>`). Stack volumes are named `<project>_<volume>` (label `com.docker.stack.namespace=<project>`). Restore scales the Service to 0, replaces the volume, then scales it back to the previous replica count and waits until that many tasks are running. Pause, dump, and `docker exec` use the task container, same as compose.

To recover a lost Server: `yoho setup`, `yoho deploy`, `yoho backup restore`. Schedule recurring runs with [scheduled jobs](scheduled-jobs.md).
