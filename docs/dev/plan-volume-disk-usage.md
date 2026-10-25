# Plan: volume disk usage in `yoho tui` and `yoho ui`

Status: implemented (method B only). See "Review log" at the end for what the review changed.

## Problem

The dashboards (ADR 0010) show `df` for two filesystems: the Yoho root and Docker's root (`dash.Host.Disks`). When a disk fills up, the operator can't see which of the App's volumes is growing (Postgres, uploads, ClickHouse), or what inside it is growing. The only option today is to SSH in and run `sudo du` by hand.

## Scope

This plan covers one collection method: `du` run as root, either by logging in as root or through passwordless sudo (`sudo -n`). It is method B of the earlier draft.

- When the SSH user is root, or `sudo -n du` works, show per-volume sizes and the largest top-level entries of each volume.
- Otherwise, list the App's volumes without sizes, together with a note that explains how to enable sizes.
- The App directory is always sized as the SSH user. It belongs to that user, so no root is needed.

Not in scope:
- the helper container (method A) and `docker system df -v` (method C);
- interactive multi-level drill-down and ncdu JSON export (later phases, listed at the end);
- deleting or pruning anything;
- scoped access through the Gate. Scoped Servers show "not available through scoped access".

Nothing is installed on the Server. One POSIX sh script runs over the existing SSH ControlMaster (`sh -s`), the same path as `dash.Script`. It uses only `id`, `sudo`, `du`, `docker`, `nice`, and `timeout` and `ionice` when they are present.

## Prior art (short)

`docker system df -v` and Portainer size volumes through the daemon, which walks everything and is slow. Coolify, Dokploy and Beszel show disk use per server or per filesystem through an agent. ncdu and gdu must run on the host they scan. Embedding gdu (`github.com/dundee/gdu/v5`, MIT) is rejected for three reasons:
- it scans the local filesystem, while the volumes are on the Server;
- it has heavy dependencies (tview, tcell, badger, sqlite, go-git);
- the browsing UI is small to write ourselves.

## Design

### Script: `dash.VolumesScript(project, appDir string)`

```sh
set -eu
LC_ALL=C; export LC_ALL
# Access: root, sudo, or none. Same probe as internal/setup, but checks du itself
# so a sudoers rule limited to du counts.
if [ "$(id -u)" = 0 ]; then S=; A=root
elif command -v sudo >/dev/null 2>&1 && sudo -n du -sk /dev/null >/dev/null 2>&1; then S="sudo -n"; A=sudo
else S=; A=none; fi
echo "@@yoho-du-access"; echo "$A"
lim=; command -v timeout >/dev/null 2>&1 && lim="timeout 120"
low="nice -n 19"; command -v ionice >/dev/null 2>&1 && low="$low ionice -c3"
# App dir: always as the SSH user (cd resolves an app_dir symlink).
echo "@@yoho-du-appdir"
( cd <appDir> 2>/dev/null && $low $lim du -sk . 2>/dev/null ) || :
# The App's volumes only: compose project or swarm stack label.
for v in $( { docker volume ls -q --filter label=com.docker.compose.project=<project>
              docker volume ls -q --filter label=com.docker.stack.namespace=<project>; } | sort -u); do
  info=$(docker volume inspect --format '{{.Driver}}	{{.Mountpoint}}	{{index .Labels "com.docker.compose.volume"}}' "$v")
  printf '@@yoho-du-vol\t%s\t%s\n' "$v" "$info"
  if [ "$A" != none ] && [ "${info%%	*}" = local ]; then
    mp=...  # second field
    $low $lim $S du -k -x -d 1 -- "$mp" 2>&1 | tail -n 200 || echo "@@yoho-du-err"
  fi
done
echo "@@yoho-du-services"
docker ps -a --no-trunc --filter label=... --format '{{.Label "com.docker.compose.service"}}	{{.Mounts}}'
```

Details:

- **Validation.** The volume name must match Docker's `[a-zA-Z0-9][a-zA-Z0-9_.-]*`, and the mountpoint must be absolute and must not contain a newline. The script skips any line that fails these checks. Every value placed in the script is quoted with `remote.Quote`. Volume names and mountpoints are passed to `du` quoted, after `--`.
- **Parsing `du` output.** Each line is `<KiB>\t<path>`. The total is the line whose path equals the mountpoint. Its children are the lines whose path is `mountpoint/<name>` with no further `/`. The view keeps the 10 largest children. Lines that don't parse are dropped, which includes file names containing a newline. Errors from `du` (`Permission denied`, `timeout`) go to the volume's `Error` field. Because `du` gives a partial total even after an error, the size is kept and marked partial.
- **Non-local drivers** (NFS, plugins) are listed but not sized: "driver X: not sized". A `du` over the network could hang or be slow.
- **Ownership.** Only volumes that carry the App's labels are listed or read. Volumes declared `external` in compose aren't labeled, so they're skipped, as AGENTS.md requires. Nothing is written.
- **Cost.** `nice` plus `ionice -c3` keep the scan in idle I/O priority. `-x` keeps `du` on one filesystem. `timeout 120` limits each volume. The scan never runs on the 5 s dashboard loop (see Cadence).
- **Services per volume** come from `docker ps --format '{{.Mounts}}'` (with `--no-trunc`): the volume names each container mounts, mapped to the Service label.

### Data model (`internal/dash/volumes.go`)

```go
// DiskUsage is the slow, on-demand part of the dashboards: sizes of the
// App directory and the App's volumes on each Server.
type DiskUsage struct {
	CollectedAt time.Time    `json:"collected_at"`
	Servers     []ServerDisk `json:"servers"`
}

type ServerDisk struct {
	Name        string   `json:"name"`
	Error       string   `json:"error,omitempty"`
	Access      string   `json:"access"` // root | sudo | none | scoped
	Note        string   `json:"note,omitempty"` // how to enable sizes
	AppDir      string   `json:"app_dir"`
	AppDirBytes uint64   `json:"app_dir_bytes"`
	Volumes     []Volume `json:"volumes"`
	Elapsed     float64  `json:"elapsed_seconds"`
}

type Volume struct {
	Name     string     `json:"name"`
	Key      string     `json:"key"`    // compose volume key
	Driver   string     `json:"driver"`
	Services []string   `json:"services"`
	Sized    bool       `json:"sized"`
	Partial  bool       `json:"partial"`
	Bytes    uint64     `json:"bytes"`
	Top      []DirEntry `json:"top"` // largest direct children
	Error    string     `json:"error,omitempty"`
}

type DirEntry struct {
	Name  string `json:"name"`
	Bytes uint64 `json:"bytes"`
}
```

When access is `none`, `Note` reads: "Volume sizes need root: connect as root, or allow `sudo -n du` for <user> (for example `<user> ALL=(root) NOPASSWD: /usr/bin/du`)." The wording follows `internal/setup` ("needs sudo: connect as root or configure passwordless sudo (NOPASSWD) for this user").

### Cadence: `Live.Disk(ctx, fresh)`

`Live` keeps a second cache with a `DiskMaxAge` of 10 minutes, separate from the Snapshot cache, and guarded by its own mutex. A slow `du` therefore never blocks the 5 s Snapshot. If several viewers ask while a collection is running, they wait for that same collection.

- **TUI:** the disk view is collected once, the first time it is opened, and again on `r` inside the view. It is never on the ticker.
- **Web:** `GET /api/disk[?fresh=1]` is fetched when the Disk tab is opened, and again by its Refresh button.

### TUI

A new key, `v` ("volumes"), opens a Disk screen for the selected Server. `Tab` switches Server, like the main screen.

```
 Yoho  disk  shop · production  on web-1  (sudo)  sized 2m ago
  App dir  /home/deploy/apps/yoho/apps/shop/production       12 MiB
  Disk /   ██████████░░░░░░░░░░  48%  52 GiB free of 100 GiB

  VOLUME                         SERVICES      SIZE    OF DISK
▸ yoho-shop-production_pgdata    db            8.2 GiB ██░░░░░ 8%
     base/                                     7.9 GiB
     pg_wal/                                   256 MiB
  yoho-shop-production_uploads   web           1.1 GiB ░░░░░░░ 1%
```

- Volumes are sorted by size. `↑↓` selects a volume, and `Enter` expands or collapses its top entries.
- With access `none`, the note is shown in yellow and the volume rows show `—` for size.
- Every name goes through the existing `cut()`, which removes control characters, so a crafted file name can't inject terminal escapes.
- The main screen's footer gains `v disk`.

### Web UI

- **Server card:** a "Volumes" section, loaded on demand by a "Show disk usage" button. It doesn't poll, because a `du` scan is expensive.
- **Table:** volume, Services, size, and a share-of-disk `Meter` (reusing `components/ui/meter`). A row expands to show its top entries.
- **No root:** the note appears in an alert block. Volumes are listed with `—` for size.
- No new npm dependency. React escapes names already.

### Wiring

`dash.Source` gains `AppDir string` (`release.AppDir(app, dest)` under the Destination root). `cli/dash.go` sets it. `tui.Data` and `webui.Data` gain `Disk(ctx, fresh) (*dash.DiskUsage, error)`. `*dash.Live` implements both. Scoped targets (`Target.Reader != nil`) get `Access: "scoped"` and a note.

## Tests

- **Parsing:** totals; children; partial size on `du` error; a name with a tab; garbage lines; a non-local driver; access none, root and sudo.
- **Script:** quoting of project and appDir; the label filters are present; no unlabeled `docker volume ls`; `sh -n` syntax check of the generated script.
- **TUI render:** the Disk screen with sizes, with the access-none note, and with a control character in an entry name.
- **Web API:** `/api/disk` requires the Bearer token; `fresh=1` passes through.
- **QA:** a temporary Debian 12 VM on PVE with Docker and a deploy user. Run the TUI and the API:
  - (a) as the deploy user with passwordless sudo: sizes appear;
  - (b) with the sudoers rule removed: the note appears and volumes are listed;
  - (c) as root: sizes appear.

## Later phases

1. Interactive drill-down: `du -d 1` on demand per path, inside a volume listed in the latest `DiskUsage`.
2. ncdu JSON export (`du -a -k` converted locally) for `ncdu -f` or `gdu -f`.
3. The helper-container method for Servers without sudo, which needs ADR 0013, and the Gate verb for scoped access.
4. `yoho app disk [--json]` for scripts and agents.

## Review log

Codex `gpt-6.1-sol` reviewed this plan on 2026-10-25 (`qa/volume-disk-plan-sol.log`, read-only). All ten findings were taken. The implementation in `internal/dash/volumes.go` differs from the sketch above in these ways:

1. **`local` driver can still be NFS or a bind.** Volumes are sized only when the driver is `local` **and** it has no driver options. The script prints option keys only, never values, because CIFS options can carry passwords. Any other volume is listed as not sized.
2. **Failures were hidden by pipelines and `set -e`.** Every operation now emits an explicit record:
   - `@@yoho-du-exit <code>` is written to fd 3, outside the `sort | head` pipeline;
   - `@@yoho-du-lserr` when `docker volume ls` fails;
   - `@@yoho-du-gone` when a volume vanishes between `ls` and `inspect`;
   - `@@yoho-du-skip` when the time budget is used up.

   `CollectDisk` parses partial output even when SSH fails.
3. **Time limits.** `timeout -k 5 120` applies per volume. Without `-k` support, the script falls back to plain `timeout 120`. With no `timeout` at all, access becomes `no-timeout` and nothing runs as root. The whole script has a 300 s budget. `CollectDisk` runs on its own 480 s context. QA confirmed that TERM reaches `du` through sudo. A `-k` KILL reaches only sudo, not its root child, but `du` honors TERM.
4. **Tabs.** Docker templates use `{{"\t"}}`, and the shell splits with `cut -f`, which uses tab by default. There is no parameter-expansion splitting.
5. **Record framing.** With GNU `du -0`, records end in NUL, and `tr '\n\0' '?\n'` turns a newline in a name into `?`. Even without `-0`, a name can't contain `/`, so it can't forge a `<size>\t<mount>/<child>` record. Malformed numeric lines are dropped; diagnostics become the error.
6. **Largest entries.** `du -a -d 1` (files too) goes through `sort -rn | head -n 11`. The total is always the largest line, so the output is the total plus the 10 largest children. No children are lost to a `tail` cut.
7. **Coalescing.** `Live.Disk` keeps an in-flight call: concurrent callers wait on one channel, each caller can cancel its own wait without stopping the scan, and the cache expires based on when the scan finished. A test checks that Snapshot isn't blocked while a scan runs.
8. **Privilege.** `sudo -n du` is independent of `servers.<name>.sudo`, which also switches Scheduled Jobs to system units (recorded in `docs/decisions.md`). A du-only sudoers rule works, which QA checked. If authorization fails on a single scan, that volume shows the `sudo:` error.
9. **Swarm.** The `com.docker.swarm.service.name` label is used, with the `<project>_` prefix stripped. Services are mapped only for volumes the script listed.
10. **UX states.** The views show:
    - App dir sized, partial (`≥`) or unknown (`—`);
    - volumes partial (`≥`, yellow) or not sized (`—` plus a reason).

    The share of disk is measured against Docker's root filesystem, which is correct now that only plain local volumes are sized. The web UI uses a Volumes card on each Server, not a tab.

## QA (2026-10-25, temporary Debian 12 VM on pve-5900x)

The VM was a linked clone of template 100000, VMID 990, running Docker 29.9.2 with Compose v5.6.1. The compose project `yoho-shop-production` had `pgdata` (60 MB in a 0700 directory, a 25 MB top-level file, and a name with a newline, a tab and an escape), `uploads`, and a `type=none,o=bind` volume. The script ran as four users:

| SSH user | Access | Result |
| --- | --- | --- |
| `debian` (full sudo) | `sudo` | Sizes and top entries shown. The bind volume shows "not sized". |
| `duonly` (`NOPASSWD: /usr/bin/du` only) | `sudo` | Same as above. |
| `nosudo` (docker group) | `none` | Volumes listed, with the note. |
| `root` | `root` | Sizes shown. |
