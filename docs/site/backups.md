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
- `archive`: files at a path or rclone remote in `tar.gz`, AES-256 `zip`, or `7z` (never ZipCrypto); `keep_last` trims old files.

## Commands

```sh
yoho backup run [JOB]        # take a Backup now
yoho backup list             # stored Backups, newest first
yoho backup restore [ID]     # restore; confirmation required
```

Run `yoho backup <cmd> --help` for flags. Hooks `pre-backup`, `post-backup`, `pre-restore`, `post-restore` run on your machine. Generated secrets are included in encrypted Backups.

To recover a lost Server: `yoho setup`, `yoho deploy`, `yoho backup restore`. Schedule recurring runs with [scheduled jobs](scheduled-jobs.md).
