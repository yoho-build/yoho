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

Generated secrets (`x-yoho.generate`, on the Server as `generated/<NAME>`) are included as `yoho-generated/<NAME>` only when the target encrypts at rest: restic, or zip/7z with `password_secret`. Any other target leaves them out and warns once. Restore writes them back after volumes and before Services start. A different value already on the Server is replaced by the Backup (the warning names the secret, not the value). The next deploy keeps that file.

## Commands

```sh
yoho backup run [JOB]                      # take a Backup now
yoho backup list [JOB]                     # stored Backups, newest first
yoho backup restore ID [--job JOB] [-y]    # restore; confirmation required unless -y
```

`JOB` defaults to the Destination's only Backup job (several jobs: name one; `restore` takes `--job`). Pass `-d` to pick the Destination. `ID` is the id shown by `backup list`; `latest` selects the newest. Hooks `pre-backup`, `post-backup`, `pre-restore`, `post-restore` run on your machine and also get `YOHO_BACKUP_JOB`, `YOHO_BACKUP_TARGET` and (after the Backup, or for a restore) `YOHO_BACKUP_ID`.

## Restore safety and recovery

Restore **interrupts this App's availability**. After fetching and verifying the volume archives, Yoho snapshots the deployed runtime state and stops all Services belonging to this App/Destination, including Web and workers. This deliberately includes consumers whose data dependency is not declared in `depends_on`; unrelated Apps and the shared Proxy/Tunnel remain untouched. Existing routes are not removed: while their backends are stopped they may return errors, but cannot serve half-restored data.

Volumes and generated secrets are restored with consumers stopped. Temporary startup has two phases: first the private `restore_dump` targets and their prerequisites run while dumps load; only **after all dumps succeed** do hook-only `post_restore` targets and their prerequisites start. Starting a target also runs its normal process, so a hook-only target declared as a dump prerequisite is refused before mutation rather than starting a worker against a loading database. Dependencies come from the **saved current Release**, not today's local Compose file; private prerequisites start in dependency order and pass running/healthcheck readiness before dependent targets start. Consumers stay stopped through those commands and the local `post-restore` Hook. This also applies to dump-only restores. After every operation succeeds, Yoho restores the exact previously running Compose container set or Swarm desired replica counts. Previously stopped containers and Services at zero replicas stay stopped. Hooks must not start consumers, change exposure, or recreate/remove deployed objects: restore's state tracking and cleanup do not cover objects a Hook creates or replaces. Cycles, missing dependency metadata and `service_completed_successfully` prerequisites are refused before any stop/data mutation; completion-only jobs need a controlled manual restore. Old Swarm Releases did not retain dependency conditions: if such a Release has prerequisites, redeploy with the current Yoho version to refresh its metadata before restoring.

Temporary targets **and every prerequisite** must have one deployed Compose container or at most one desired Swarm replica, no published ports/shared host network namespace, and only App-owned bridge/overlay networks. Targets on the shared `yoho` network (Proxy/Tunnel access), external networks, or with published ports are refused **before stopping anything or changing data**. Remove/isolate that exposure explicitly and redeploy before retrying; Yoho will not blindly delete routes. All deployed objects require matching `yoho.app` and `yoho.destination` ownership labels, and Swarm Services must be replicated, not global. Restore is not a security boundary against separately managed clients that access the App's private network or data; stop such clients yourself first.

All temporary Swarm targets/prerequisites must also have an explicit `node.id == ...` or `node.hostname == ...` constraint pinning them to the restore Server; remote or movable targets are refused before mutation, and Yoho does not repin unrelated Services. Swarm volume ownership checks inspect every cluster Service spec, including Services with zero replicas, as well as local containers. For `service_healthy`, the effective healthcheck includes an inherited image `HEALTHCHECK` when the Service leaves the test unset or overrides only timing; explicit `NONE` remains disabled. Image metadata must be readable locally during preflight, and restore does not pull images to resolve it.

On failure or cancellation after isolation starts, Yoho attempts to stop every App Service again using a bounded, uncancelled cleanup. Stop/scale-zero requests are issued to every object with separately bounded attempts before Swarm convergence waits, so one slow Service does not prevent later scale-zero requests. It leaves the fetched Backup and `prior-state.json` (the original container states/replica counts) in the reported staging directory. If SSH/Docker is unreachable or cleanup fails, isolation cannot be guaranteed: **verify and manually stop this App's Services immediately**. A killed CLI process cannot run cleanup; consumers remain stopped, but a private temporary target may still run. Do not start consumers until the data is verified.

There is **no automatic volume or dump rollback**. Some volumes, dumps, or generated secrets may already have changed. Fix the cause and rerun the restore, or inspect/repair the data with consumers still stopped. Before manually resuming, use `prior-state.json` to preserve previously inactive Services: start only the recorded running Compose container IDs, or scale each recorded Swarm Service to its previous count. Rerunning a restore after failure snapshots the then-stopped state; it does not automatically recover the original availability. `yoho deploy` can reopen traffic, but does **not** restore old data and may start Services that were intentionally stopped.

## Swarm

On a multi-Server Destination, `yoho backup run`, `list`, and `restore` use only the first Server (the Swarm manager). Stateful Services are pinned there, so that host has the task container and the volume. The container is the running task (`docker ps` filtered by `com.docker.swarm.service.name=<project>_<service>`). Stack volumes are named `<project>_<volume>` (label `com.docker.stack.namespace=<project>`). Restore scales all owned App Services to zero and waits for tasks to stop across the cluster before replacing data. Private restore targets temporarily scale to one for dumps/Hooks; only after success do all Services return to their prior desired counts, including zero. Pause, dump, and `docker exec` use the local Stateful task container, same as compose.

To recover a lost Server: `yoho setup`, `yoho deploy`, `yoho backup restore`. Schedule recurring runs with [scheduled jobs](scheduled-jobs.md).
