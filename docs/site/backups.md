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
    verify: ["psql", "-U", "postgres", "-tAc", "select count(*) from orders"]   # optional; backup drill only
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

Generated secrets (`x-yoho.generate`, on the Server as `generated/<NAME>`) are included as `yoho-generated/<NAME>` only when the target encrypts at rest: restic, or zip/7z with `password_secret`. Any other target leaves them out and warns once. Restore writes them back after volumes and before Services start. A different value already on the Server is replaced by the Backup (the warning names the secret, not the value). The next deploy keeps that file. A restore with `--from` another Destination leaves the target's generated secrets alone unless you pass `--copy-generated-secrets` (see [Restore from another Destination](#restore-from-another-destination)).

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

Give each Destination its own authorized OAuth remote and `rclone_config_secret`, even when the Destinations share a Server or Backup Target. OAuth remotes can rotate refresh tokens: independently refreshed copies of one authorization can invalidate each other. Targets in the same Destination may share the secret because they use the same persistent config file. `yoho config check` warns when Backup jobs in different Destinations use the same secret key.

## Commands

```sh
yoho backup run [JOB]                      # take a Backup now
yoho backup list [JOB]                     # stored Backups, newest first
yoho backup pull [JOB] [--to DIR]          # mirror archives to a local directory
yoho backup check [JOB]                    # integrity only; never a restore proof
yoho backup check [JOB] --read-data        # restic: also read all data packs
yoho backup check [JOB] --id ID            # archive: selected ID (default latest)
yoho backup restore ID [--job JOB] [-y]    # restore; confirmation required unless -y
yoho backup restore ID --from production -d staging [--copy-generated-secrets]
yoho backup drill [JOB] [--id ID] [--keep] # restore into a throwaway project, check health, time it
yoho backup drill --cleanup                # remove leftover drills of the Destination
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

A passed check is **not a restore proof**: it does not test dump loading, application consistency or recovery. Use [`backup drill`](#restore-drills) for that.

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

## Restore from another Destination

`yoho backup restore ID --from production -d staging` restores a Backup that production took into staging: the usual way to refresh staging with real data, or to rehearse a restore without touching production. Everything in [Restore safety and recovery](#restore-safety-and-recovery) applies to the target (staging): its Services stop first, the same isolation preflight runs, the Destination lock is held and you confirm unless `-y`.

- The Backup job and Backup Target are the **source's** (`--job` names a job of the source Destination). The Target must be reachable from the target's Server: a remote (S3, B2, SFTP, `rclone:`), or a local path when both Destinations share a Server. `-d` is required with `--from`.
- Only the source's Backups are listed and fetched (restic tag `destination=<source>`, archive names `yoho-<app>-<source>-*`), and the manifest must name the same App and the source Destination. **Another App's Backup is always refused.**
- **Generated secrets are not copied by default.** A production Backup on an encrypted Target contains production's `x-yoho.generate` values (database passwords, signing keys). Copying them would silently put production credentials into staging, so staging keeps its own and the command warns with the names it skipped. Pass `--copy-generated-secrets` when the restored data needs the source's values, for example a volume-level database restore whose users were created with the source's generated password. Then they replace the target's, as a same-Destination restore does, and `yoho deploy` makes running Services use them. Dump-based restores (`pg_dump` / `psql`) usually do not need them.
- Hooks get `YOHO_BACKUP_FROM` (the source Destination).

The data is production's: scrub personal data in a `post_restore` command or `post-restore` Hook if staging is less protected than production.

## Restore drills

`yoho backup check` proves a Backup is readable. `yoho backup drill` proves it restores:

```sh
yoho backup drill -d production              # newest Backup of the Destination's only job
yoho backup drill nightly --id 3cb5409d      # a specific job and Backup
yoho backup drill -d production --keep       # leave it running to look around
yoho backup drill -d production --cleanup    # remove kept or leftover drills
```

What it does, on the Destination's (first) Server:

1. **download**: fetches the Backup into private staging and checks its manifest belongs to this App/Destination.
2. **restore**: first runs the real restore's read-only isolation preflight against the running App (ownership, `restore_dump`/`post_restore` prerequisites, published ports, host or shared networking, Proxy routes, foreign volume users) and fails with "a real restore would refuse: ..." where `yoho backup restore` would refuse, even though the drill itself strips ports and networks. Then it creates a throwaway compose project `yoho-<app>-<destination>-drill-<8 hex>` from the **deployed Release** (not your local compose file), restores the volumes into the drill's own fresh volumes and loads dumps with `restore_dump`.
3. **healthy**: starts the backed-up Services and waits until each container is running and its healthcheck reports healthy (5 minutes at most).
4. **verify**: runs each Service's `x-yoho.backup.verify` command inside its drill container; a nonzero exit fails the drill. Without one, the drill proves restore and health, not your data.
5. **cleanup**: removes the drill's containers (with anonymous volumes), volumes and network, also when a step fails. `--keep` skips this.

It prints a table of phases with timings; download + restore + healthy is the **RTO estimate** for the data part of a recovery onto this Server. `--json` emits a `drill_phase` event per phase (`phase`, `duration_ms`, `detail`) and a final `drill` event (`project`, `backup_id`, `services`, `volumes`, `dumps`, `verified`, `rto_ms`, `kept`).

Isolation: the running App is never stopped, restarted or written to. The drill project only contains the backed-up Services, with their image, command, environment, env files, healthcheck, user and file-based secrets and configs from the deployed Release. It drops published ports, `networks`/`network_mode` (one `internal: true` network, no route in or out), Proxy labels, `container_name`, `depends_on`, privileges, devices, deploy settings and restart policies. Named volumes become fresh `<drill project>_<key>` volumes: any `name`, `external`, `driver` or `driver_opts` is dropped, so a drill volume can never alias the App's volume or a host path. Bind mounts must be read-only (a read-write bind mount would be shared with the running App, so the drill refuses). Drill objects carry `yoho.drill`, `yoho.drill.app` and `yoho.drill.destination` labels and never `yoho.app` / `yoho.destination`, so nothing that manages the App treats them as its own.

Cleanup takes the Destination lock first, so it refuses while a drill (or a deploy or Backup) is running and never removes another operator's drill in progress. It removes an object only after proving it belongs to the drill: the project name must be a drill name of this App/Destination; every container, volume and network of the project must carry exactly the drill labels and none of the App's; volumes must be named `<drill project>_*` and used by no container outside the drill. Any mismatch stops the cleanup before it removes anything. The App's own project, volumes and network can never be targets.

The drill holds the Destination lock while it runs (a deploy waits), uses the Server's CPU, memory and disk for a second copy of the data, and reads the deployed Release's secrets. It does not start Web or workers, so it does not prove the App's code works with the data; `restore --from` into staging does. Swarm Destinations are refused for now.

Tip: make the healthcheck mean "ready for connections over the network". PostgreSQL's image runs a temporary socket-only server during first initialization, so `pg_isready` without `-h 127.0.0.1` can report healthy before the real server starts, and a dump load then fails. The drill catches exactly this kind of problem, and `yoho config check` warns about a Stateful Service whose healthcheck runs `pg_isready` without a host.

QA on a small PostgreSQL 17 App (dump-only Backup in a local restic repository, same Server), three consecutive runs: download 1.3 to 1.8 s, restore 2.3 to 3.1 s, healthy 0.3 s, verify 0.1 s, cleanup 0.6 to 0.7 s; RTO estimate 4.4 to 4.9 s, whole command under 7 s. Expect download and restore to grow with Backup size and Target bandwidth; measure your own.

## Losing a Server

When the Server of a compose Destination (or the first Server of a Swarm Destination) is gone, recovery is: a new Server, the App deployed, the data restored. You need the Backup off the lost Server ([High availability & recovery](high-availability.md#off-server-backups-are-the-recovery-path)) and your Yoho file, compose file and secrets locally.

1. **Provision.** Create the Server, point `servers.<name>.ssh` at it (or reuse the name with the new address), then:

   ```sh
   yoho setup -d production --plan   # what will change
   yoho setup -d production -y       # Docker, deploy user, directories, firewall
   ```

   On Swarm, run `yoho swarm init` and `yoho swarm join` next.
2. **Deploy.** `yoho deploy -d production` creates the App directory, the volumes and fresh generated secrets, and starts the Services with empty data. If the Proxy routes real traffic, consider keeping DNS (or the Tunnel) pointed away until step 4.
3. **Restore.** Find the Backup and restore it. The restore stops the Services, restores volumes, generated secrets (encrypted Targets) and dumps, then resumes them:

   ```sh
   yoho backup list -d production
   yoho backup restore latest -d production -y
   ```

   If it warns that generated secrets changed, run `yoho deploy -d production` again so Services read the restored values (the database password from the Backup matches the restored data).
4. **Check and reopen.** `yoho app ps -d production`, look at your data, then point DNS or the Tunnel back. Re-install Scheduled Jobs with `yoho schedule install -d production` so Backups continue.

Timings from QA (local Server, Docker already installed, small PostgreSQL App, local restic repository; a fresh Destination stands in for the new Server): `yoho setup --plan` 0.6 s (setup itself not run on the shared QA Server; installing Docker usually takes minutes), first `yoho deploy` 4.7 s, `yoho backup restore latest --from production -d dr -y --copy-generated-secrets` 7.0 s, second `yoho deploy` 3.7 s: under 20 s once the Server runs Docker. Real recoveries are dominated by provisioning, image transfer and Backup download; `yoho backup drill` measures the last two parts on your own data.

## Swarm

On a multi-Server Destination, `yoho backup run`, `list`, and `restore` use only the first Server (the Swarm manager). Stateful Services are pinned there, so that host has the task container and the volume. The container is the running task (`docker ps` filtered by `com.docker.swarm.service.name=<project>_<service>`). Stack volumes are named `<project>_<volume>` (label `com.docker.stack.namespace=<project>`). Restore scales all owned App Services to zero and waits for tasks to stop across the cluster before replacing data. Private restore targets temporarily scale to one for dumps/Hooks; only after success do all Services return to their prior desired counts, including zero. Pause, dump, and `docker exec` use the local Stateful task container, same as compose.

Restore drills are not supported on Swarm Destinations yet; drill with `restore --from` into a compose staging Destination. To recover a lost Server, see [Losing a Server](#losing-a-server). Schedule recurring runs with [scheduled jobs](scheduled-jobs.md).
