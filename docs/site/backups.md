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
      repository: "s3:s3.amazonaws.com/shop-backups"   # s3:, b2:, sftp:, rclone:, or a path
      password_secret: BACKUP_PASSWORD
      env_secrets: [AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY]
      keep_last: 14
  jobs:
    nightly: { destination: production, target: offsite, schedule: "*-*-* 03:00:00" }
```

- `restic` (default): encrypted, deduplicated.
- `archive`: files at a path or rclone remote in `tar.gz` (default `format`), AES-256 `zip`, or `7z` (never ZipCrypto); set `format` to choose; `keep_last` trims old files.

Generated secrets (`x-yoho.generate`, on the Server as `generated/<NAME>`) are included as `yoho-generated/<NAME>` only when the target encrypts at rest: restic, or zip/7z with `password_secret`. Any other target leaves them out and warns once. Restore writes them back after volumes and before Services start. A different value already on the Server is replaced by the Backup (the warning names the secret, not the value). The next deploy keeps that file.

## OneDrive, Google Drive and other OAuth remotes

OAuth remotes refresh their access token on every run and write the new token back to rclone's config file. An `RCLONE_CONFIG_*` env secret cannot be written back, so the remote stops working when the stored refresh token expires (about 90 days for OneDrive). Use `rclone_config_secret` instead:

1. Run `rclone config` on your machine and authorize the remote (here `od`).
2. Copy that remote's section from `rclone config file` into `.yoho/secrets` as a multi-line value, or load it from 1Password with `$(op read ...)`:

   ```sh
   RCLONE_CONF='[od]
   type = onedrive
   token = {"access_token":"...","refresh_token":"...","expiry":"..."}
   drive_id = ...
   drive_type = personal
   '
   ```

3. Point an encrypted `archive` Target (or a restic `rclone:` repository) at the remote:

   ```yaml
   backups:
     targets:
       offsite:
         type: archive
         repository: "od:backups/shop"      # rclone remote:path
         format: 7z                         # AES-256, encrypted headers
         password_secret: BACKUP_PASSWORD
         rclone_config_secret: RCLONE_CONF
         keep_last: 30
   ```

Yoho writes the secret over SSH stdin to `<root>/backups/<app>/<destination>/.rclone/<KEY>/rclone.conf` (mode 0600, private directory) the first time a `yoho backup` command or `yoho schedule install` runs, and records the secret's SHA-256 next to it. Server-installed rclone gets `RCLONE_CONFIG` pointing at that file; the pinned rclone container mounts its directory read-write and runs as the deploy user, so refreshed tokens are kept. Scheduled Jobs use the file and never store the secret. Yoho replaces the file only when the secret changes, so after `rclone config reconnect od:` locally, update the secret and run `yoho deploy`, `yoho schedule install` or any `yoho backup` command. `yoho backup check` shows when the config was seeded; an authentication failure adds this re-authorization hint. `env_secrets` still work alongside it and override config values.

## Commands

```sh
yoho backup run [JOB]                      # take a Backup now
yoho backup list [JOB]                     # stored Backups, newest first
yoho backup pull [JOB] [--to DIR]          # mirror archives to a local directory
yoho backup check [JOB]                    # integrity only; never a restore proof
yoho backup check [JOB] --read-data        # restic: also read all data packs
yoho backup check [JOB] --id ID            # archive: selected ID (default latest)
yoho backup restore ID [--job JOB] [-y]    # restore; confirmation required unless -y
```

`JOB` defaults to the Destination's only Backup job (several jobs: name one; `restore` takes `--job`). Pass `-d` to pick the Destination. `ID` is the id shown by `backup list`; `latest` selects the newest. Hooks `pre-backup`, `post-backup`, `pre-restore`, `post-restore` run on your machine and also get `YOHO_BACKUP_JOB`, `YOHO_BACKUP_TARGET` and (after the Backup, or for a restore) `YOHO_BACKUP_ID`.

## Mirror archives locally (`backup pull`)

`yoho backup pull [JOB] [--to DIR] [--limit N]` copies the archives of an `archive` Backup Target from the Destination's Servers to your machine, so you keep an off-Server copy. `--to` defaults to `$YOHO_BACKUP_PULL_DIR`, then `~/yoho-backups`; files land in `DIR/<app>/<destination>/` (with a `<server>/` level when the Destination has several Servers, since Swarm writes the archive on the node that ran the job).

- It reads over the same SSH connection as every other command. The Server needs no `rsync`.
- Each archive is written to `.<name>.part`, checked against the Server's size and SHA-256 (`sha256sum`, `shasum` or `openssl` on the Server), then renamed. An interrupted run leaves only `.part` files, which the next run overwrites.
- It never deletes. Archives already present with the same size are skipped; a same-name file of a different size is reported as a conflict (the command exits non-zero) and left alone. Pruning on the Server (`keep_last`) does not touch the mirror.
- Encrypted `zip`/`7z` archives are copied as ciphertext; no password is read.
- restic repositories and rclone remotes are not files on the Server and are refused. Use `restic copy` or `rclone sync` for those.
- `--json` prints one `backup_pull` event per archive (`id`, `server`, `status` of `copied`, `skipped`, `conflict` or `failed`, `size`, `sha256`, `path`) and a `summary` event (`copied`, `skipped`, `conflicts`, `failed`, `bytes`).

### Run it daily

macOS (launchd): save as `~/Library/LaunchAgents/com.yohobuild.backup-pull.plist`, set the project path, then `launchctl load ~/Library/LaunchAgents/com.yohobuild.backup-pull.plist`.

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.yohobuild.backup-pull</string>
  <key>ProgramArguments</key><array>
    <string>/usr/local/bin/yoho</string><string>backup</string><string>pull</string>
    <string>-d</string><string>production</string><string>--to</string><string>/Users/me/yoho-backups</string>
  </array>
  <key>WorkingDirectory</key><string>/Users/me/code/myapp</string>
  <key>StartCalendarInterval</key><dict><key>Hour</key><integer>4</integer><key>Minute</key><integer>30</integer></dict>
  <key>StandardOutPath</key><string>/Users/me/yoho-backups/pull.log</string>
  <key>StandardErrorPath</key><string>/Users/me/yoho-backups/pull.log</string>
</dict></plist>
```

Linux (systemd user timer): `~/.config/systemd/user/yoho-backup-pull.service` and `.timer`, then `systemctl --user enable --now yoho-backup-pull.timer`.

```ini
# yoho-backup-pull.service
[Unit]
Description=Mirror Yoho Backups

[Service]
Type=oneshot
WorkingDirectory=%h/code/myapp
ExecStart=/usr/local/bin/yoho backup pull -d production --to %h/yoho-backups

# yoho-backup-pull.timer
[Unit]
Description=Mirror Yoho Backups daily

[Timer]
OnCalendar=*-*-* 04:30:00
Persistent=true

[Install]
WantedBy=timers.target
```

The job needs the same SSH access and secrets as an interactive `yoho` run (an agent or key without a passphrase prompt). The local Mac or Linux box is not a Scheduled Job: those stay on the Server (ADR 0003).

## Read-only integrity checking

`backup check` requires an existing Backup Target and at least one Backup belonging to this App/Destination. A missing/uninitialized target, empty repository, unreadable archive, missing/foreign manifest, missing referenced file or corrupt volume archive fails with a nonzero exit. It never initializes or prunes the target, restores a restic snapshot, loads dumps, runs backup/restore Hooks, or mounts/changes App volumes. On Swarm it uses the first Server, like other Backup commands.

- **restic:** `restic check` verifies the repository's structural consistency across all snapshots, not just this App. Normal checking does not read every data pack; explicit `--read-data` adds that full read (potentially expensive). It does not validate Yoho manifests or application data. Yoho lists and validates exact owned snapshot tags both before and after the repository check; disappearance of the last owned Backup or a failed/malformed post-check listing fails the command. This is a point-in-time availability observation, not a guarantee against subsequent concurrent pruning. `--id` is not supported.
- **archive:** checks one Backup, newest by default or explicitly `--id` from `backup list`. Only this App/Destination's filename is fetched. Yoho verifies outer archive integrity, exact manifest App/Destination/project ownership, referenced regular files, and readability of each volume's tar/gzip archive. Dumps and generated-secret files are checked for presence/readability, not semantic correctness. Extraction runs as the deploy user in new private staging under the App's Backup spool, which is removed on success, error or cancellation; a cleanup failure reports the remaining path. `--read-data` is not supported.

Checking takes the same Yoho Destination lock as Backup/deploy/restore. Restic uses `--no-lock --no-cache` and local repositories are mounted read-only, so checking does not write repository locks or initialize directories. **Coordinate external restic writers and Yoho jobs from other Destinations sharing the repository yourself**; concurrent writes/pruning can cause transient check failures. The Destination lock is not a repository-wide lock. Helper images may be pulled if absent. `--json` emits UI events and a result row containing `method`, `backup_id`, `verified`, and `restore_proven: "false"`.

A passed check is **not a restore proof**: it does not test dump loading, application consistency or recovery. Plan an explicitly authorized isolated restore drill separately.

## Filesystem trust and archive handling

Backup spool and generated-secret directories must be real, deploy-user-owned private directories (no group/other permission bits). Yoho creates missing directories privately; it **refuses**, rather than chmods/chowns, unsafe existing paths. Local Backup Targets and all existing path ancestors must be owned by root or the SSH/Scheduled Job user and must not be group/other-writable. A root/user-owned **sticky ancestor**, such as `/tmp`, is allowed for a private child, never as the Target/spool itself. Root-owned filesystem-alias symlinks in ancestors are trusted only after checking their physical path; configured leaf symlinks and deploy-user-owned ancestor symlinks are refused. This supports normal `/var/lib/yoho` and private home-directory layouts without trusting an arbitrary shared directory. ACLs must not grant untrusted users write access. Local repositories must use an absolute path (`local:/absolute/path` is also accepted for restic); parent-traversal components and colons in filesystem paths are refused. Existing shared-directory Targets need an explicitly provisioned trusted private child; Yoho never repairs unrelated parent directories.

Each run/check/restore has an exclusively created random staging directory with a private ownership marker outside extractor-writable data. Cleanup verifies that marker and path ownership; an existing collision or unproven directory is never deleted. Cleanup failures report the remaining path. Volume restore scratch directories are also exclusively created, without deleting a predictable preexisting scratch path.

Local archive publication uses an exclusive private temporary directory and an atomic, same-filesystem hard link of the complete file. Cleanup is registered in Yoho **before remote creation**, verifies the private marker with a bounded uncancelled context, and removes only that proven publication temporary directory, never the committed archive. This also handles a killed remote shell, lost acknowledgement or cancelled request; an unproven collision is preserved and reported. **An existing Backup ID, file, symlink or directory is never overwritten**; concurrent publications of the same timestamp/ID have one winner and the other fails. Archive filenames and formats are unchanged. The filesystem must support hard links; there is no unsafe overwrite fallback. Fetch/list/retention refuse linked, multiply linked, unsafe-mode or foreign-UID archive files; retention deletes only files owned by the current deploy user, and never traverses links. Filename/App/Destination filtering remains the legacy archive ownership contract, not a cryptographic provenance check. Rclone Targets retain their provider's existing copy/retention semantics, not this local atomic-publication guarantee.

A **killed Yoho process** cannot run deferred cleanup. A verified `.yoho-publish-<nonce>/` may remain in the local Target; if linking already committed, its `archive` and the final Backup share an inode until that temporary directory is removed. List/fetch/retention deliberately refuse this multiply linked Backup rather than relaxing link safety. For recovery, verify the directory's origin, deploy-UID ownership, private mode and `.yoho-check-owner` marker, and verify that its `archive` is the same inode as the intended committed Backup. Remove only that verified private publication directory, keep the committed Backup, then retry `backup check`. Do not delete an unproven directory or a foreign file. A reported cleanup failure requires the same ownership-aware recovery; no automatic sweeping of old publication directories occurs.

Untrusted outer archives are extracted by a restricted, networkless helper running as the deploy UID with a read-only container filesystem, no capabilities, a read-only archive mount, and **only new staged data writable**. Neither the staging parent/marker, App volumes nor other Server resources are mounted writable. Linked (symlink or hardlink) and special outer entries are rejected; manifest, dump and generated-secret reads also validate file/ancestor ownership and reject links. App-volume data stays inside its own tar archive: legitimate internal symlinks, hardlinks, owners and modes are preserved, not recursively rewritten. Volume mutation helpers mount only the selected, ownership-preflighted volume and read-only Backup data; their container filesystem is read-only.

These checks are not an absolute TOCTOU guarantee or a security boundary against host root, an attacker using the deploy UID, a compromised Docker daemon/helper image, or separately managed writers to App volumes. Keep that UID and its private directories exclusive to trusted operators, and retain the restore isolation rules below. The ONCE advisory GHSA-jqwc-wmh4-9qw3 motivated this audit; it is not evidence that Yoho had the same vulnerability.

## Restore safety and recovery

Restore **interrupts this App's availability**. After fetching and verifying the volume archives, Yoho snapshots the deployed runtime state and stops all Services belonging to this App/Destination, including Web and workers. This deliberately includes consumers whose data dependency is not declared in `depends_on`; unrelated Apps and the shared Proxy/Tunnel remain untouched. Existing routes are not removed: while their backends are stopped they may return errors, but cannot serve half-restored data.

Volumes and generated secrets are restored with consumers stopped. Temporary startup has two phases: first the private `restore_dump` targets and their prerequisites run while dumps load; only **after all dumps succeed** do hook-only `post_restore` targets and their prerequisites start. Starting a target also runs its normal process, so a hook-only target declared as a dump prerequisite is refused before mutation rather than starting a worker against a loading database. Dependencies come from the **saved current Release**, not today's local Compose file; private prerequisites start in dependency order and pass running/healthcheck readiness before dependent targets start. Consumers stay stopped through those commands and the local `post-restore` Hook. This also applies to dump-only restores. After every operation succeeds, Yoho restores the exact previously running Compose container set or Swarm desired replica counts. Previously stopped containers and Services at zero replicas stay stopped. Hooks must not start consumers, change exposure, or recreate/remove deployed objects: restore's state tracking and cleanup do not cover objects a Hook creates or replaces. Cycles, missing dependency metadata and `service_completed_successfully` prerequisites are refused before any stop/data mutation; completion-only jobs need a controlled manual restore. Old Swarm Releases did not retain dependency conditions: if such a Release has prerequisites, redeploy with the current Yoho version to refresh its metadata before restoring.

Temporary targets **and every prerequisite** must have one deployed Compose container or at most one desired Swarm replica, no published ports/shared host network namespace, and only App-owned bridge/overlay networks. Targets on the shared `yoho` network (Proxy/Tunnel access), external networks, or with published ports are refused **before stopping anything or changing data**. Remove/isolate that exposure explicitly and redeploy before retrying; Yoho will not blindly delete routes. All deployed objects require matching `yoho.app` and `yoho.destination` ownership labels, and Swarm Services must be replicated, not global. Restore is not a security boundary against separately managed clients that access the App's private network or data; stop such clients yourself first.

All temporary Swarm targets/prerequisites must also have an explicit `node.id == ...` or `node.hostname == ...` constraint pinning them to the restore Server; remote or movable targets are refused before mutation, and Yoho does not repin unrelated Services. Swarm volume ownership checks inspect every cluster Service spec, including Services with zero replicas, as well as local containers. For `service_healthy`, the effective healthcheck includes an inherited image `HEALTHCHECK` when the Service leaves the test unset or overrides only timing; explicit `NONE` remains disabled. Image metadata must be readable locally during preflight, and restore does not pull images to resolve it.

On failure or cancellation after isolation starts, Yoho attempts to stop every App Service again using a bounded, uncancelled cleanup. Stop/scale-zero requests are issued to every object with separately bounded attempts before Swarm convergence waits, so one slow Service does not prevent later scale-zero requests. It leaves the fetched Backup and `prior-state.json` (the original container states/replica counts) in the reported staging directory. If SSH/Docker is unreachable or cleanup fails, isolation cannot be guaranteed: **verify and manually stop this App's Services immediately**. A killed CLI process cannot run cleanup; consumers remain stopped, but a private temporary target may still run. Do not start consumers until the data is verified.

There is **no automatic volume or dump rollback**. If final staging cleanup fails after an otherwise successful restore resumed Services, Yoho performs another bounded stop to preserve fail-closed error behavior; it reports that remaining staged files may be incomplete, rather than promising a recoverable Backup after partial deletion. A failed stop still requires immediate manual isolation. Some volumes, dumps, or generated secrets may already have changed. Fix the cause and rerun the restore, or inspect/repair the data with consumers still stopped. Before manually resuming, use `prior-state.json` to preserve previously inactive Services: start only the recorded running Compose container IDs, or scale each recorded Swarm Service to its previous count. Rerunning a restore after failure snapshots the then-stopped state; it does not automatically recover the original availability. `yoho deploy` can reopen traffic, but does **not** restore old data and may start Services that were intentionally stopped.

## Swarm

On a multi-Server Destination, `yoho backup run`, `list`, and `restore` use only the first Server (the Swarm manager). Stateful Services are pinned there, so that host has the task container and the volume. The container is the running task (`docker ps` filtered by `com.docker.swarm.service.name=<project>_<service>`). Stack volumes are named `<project>_<volume>` (label `com.docker.stack.namespace=<project>`). Restore scales all owned App Services to zero and waits for tasks to stop across the cluster before replacing data. Private restore targets temporarily scale to one for dumps/Hooks; only after success do all Services return to their prior desired counts, including zero. Pause, dump, and `docker exec` use the local Stateful task container, same as compose.

To recover a lost Server: `yoho setup`, `yoho deploy`, `yoho backup restore`. Schedule recurring runs with [scheduled jobs](scheduled-jobs.md).
