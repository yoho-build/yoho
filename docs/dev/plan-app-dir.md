# Plan: optional per-Destination App directory (`app_dir`)

Status: implemented (see "Implementation notes" at the end of the Review log).

## Problem

Every App lives at `<root>/apps/<app>/<destination>` (for example `/home/deb/apps/yoho/apps/elpoca/staging`). Users who keep one directory per project on a Server (`/home/deb/apps/elpoca-engine`) cannot put an App there. Today the only lever is `servers.<name>.root`, which every App on that Server shares. Renaming the App changes the compose project and volume names, so Yoho treats it as a new App with empty volumes.

## Goal

An optional, per-Destination setting that moves the App directory and leaves everything else alone:

```yaml
destinations:
  staging:
    servers: [small-deb]
    app_dir: /home/deb/apps/elpoca-engine
```

Not changed: compose project `yoho-<app>-<destination>`, volume names, Proxy route names, Scheduled Job unit names, `<root>/backups`, `<root>/jobs`, `<root>/bin`, `<root>/proxy`, `<root>/tunnel`. Volume data never moves.

## Design: the canonical path becomes a symlink

`<root>/apps/<app>/<destination>` stays the path Yoho uses everywhere. When `app_dir` is set, that path is a symlink to `app_dir`, and the real files live in `app_dir`.

Why a symlink instead of teaching `release.AppDir` about overrides:

- **Discovery keeps working.** Several readers glob the fixed layout instead of reading config: image retention (`deploy.RetainedImageRefs` scans `<root>/apps/<app>/*/releases`), dashboards, `schedule run` on the Server (it gets only `Root` from the job spec), and older Yoho binaries. With an override, a Destination using `app_dir` would drop out of the retention scan, and images another Destination needs for rollback could be pruned. The symlink keeps every reader correct without changing them.
- **Retained Releases stay valid.** Compiled `compose.yaml` files under `releases/<version>/` hold absolute bind sources (secrets generations, `files/<hash>`). They keep using the canonical path, so a later move of `app_dir` doesn't break rollback to an older Release. Docker resolves the symlink when a container is created.
- **The code change is small.** Path builders and Server scripts don't change. The new work is in one place: making sure the canonical path is in the expected state before a command uses it.

Cost: users see the canonical path in Yoho output (releases, hints), not `app_dir`. `yoho status` / the step detail also prints `app_dir` so it is discoverable.

## Ownership and safety

`app_dir` is a directory Yoho owns completely. Pruning and cleanup delete inside it.

1. **Config validation** (`config.Validate`):
   - absolute, already clean (`path.Clean(p) == p`), uses safe path characters (the `safePathRe` set from `internal/schedule`), not `/`
   - lexically not equal to, inside, or an ancestor of the Destination's `root`
   - no two Destinations in one Yoho file have the same or nested `app_dir` on a shared Server
2. **Physical path trust.** Reuse the Backup filesystem trust script (`internal/backup/paths.go` `pathSafety` / `pathCreation`). Move it to a small shared package (`internal/safepath`) so deploy and build can use it without importing backup.
   - Every ancestor of `app_dir` must be owned by root or the deploy user and not group- or other-writable (owned sticky ancestors allowed, as for Backups). Only root-owned aliases (macOS `/var`) are allowed, checked through their physical path.
   - Missing components are created one at a time with `mkdir -m 0700` and re-validated after each (`yoho_make`). No `mkdir -p` / `install -d` through unchecked parents.
   - Root containment is checked **physically** too. Resolve the candidate directory: `cd -P` of its deepest existing ancestor, plus the missing components appended (they can't be links because they don't exist yet). The resolved candidate must not equal, be inside, or be an ancestor of `cd -P <root>`. A sibling of root such as `/home/deb/apps/elpoca-engine` next to `/home/deb/apps/yoho` is allowed.
   - When `app_dir` is set, the Destination name must match the App name pattern (`^[a-z0-9][a-z0-9-]*$`), so every path built from it is a single safe component.
3. **Ownership marker.** `.yoho-app.json` (`{"app","destination"}`, 0600, a regular file owned by the deploy user) in the App directory. Yoho writes it into every App directory it uses, including default ones, before any move.
   - A new `app_dir` must not exist, or be an empty directory. A non-empty directory without a matching marker (a git checkout, someone's files) is refused. A marker for another App or Destination is refused.
   - **An existing canonical symlink is trusted only when** it is owned by the deploy user, its target (`readlink`) is an absolute path that satisfies rule 2, and the target holds a marker for this App and Destination. Otherwise Yoho refuses, both for use and for migration. This also covers dangling links, unless the migration journal explains them (below).
4. **Canonical path states** (one idempotent POSIX sh script, `remote.Quote`d, run under the gate below):

   | canonical path | `app_dir` unset | `app_dir` set |
   |---|---|---|
   | missing, no journal | create a real dir (today) | create `app_dir` (rule 2) + marker, then symlink |
   | real dir | use it (today) | **migrate forward** |
   | trusted symlink → `app_dir` | **migrate back** | use it |
   | trusted symlink → other dir | **migrate back** | **retarget** |
   | untrusted symlink or other file type | refuse | refuse |
   | any state with a journal | resume the journal | resume the journal |

   Reading commands (`plan`, `status`, `logs`, dashboards) never change state; they report a mismatch as a planned change.

## Backup trust exception (narrow)

`yoho_private` and `yoho_path` reject symlinks created by the deploy user, so a symlinked App directory breaks Backup run/restore, generated-secret reads and Scheduled Jobs. Extend the shared trust script with one explicit trusted link:

- Callers that validate the App directory pass `yoho_applink=<canonical path>`. Only in `yoho_path`, when the current component equals `$yoho_applink`, is a link allowed, and only if rule 3's trusted-symlink check holds. Validation then continues on the physical target (`yoho_path "$target" leaf`), and the existing ancestor walk continues above the canonical path.
- `yoho_private`'s terminal check (`[ -d "$1" ] && [ ! -L "$1" ]`, owner, mode 0700) applies to the **trusted physical target** when `$1` is `$yoho_applink`: the target must be a real directory (not a link), owned by the deploy UID, with no group or other bits. `yoho_file`, `yoho_secret` and the generated-secret listing resolve paths below the App directory through the same exception, so their parent checks pass and their own checks stay unchanged.
- Every other link stays rejected, including links inside the App directory, other deploy-user links, and links at the leaf of a Backup path.
- `safeCurrentFile` compares `cd -P` results, so `current` must still resolve inside the physical App directory's `releases/`. That check stays as it is.
- Tests: Backup run/check/restore and the generated-secret listing succeed through a trusted App link. An untrusted link (no marker, a marker for another App, a link owned by a different UID, a dangling link) is still refused.

## Coordination: a gate outside the moving tree

The existing lock is `<canonical>/lock`, found by pathname. A move makes that pathname unreachable for a moment, and a concurrent `AcquireLock` would `mkdir -p` a fresh canonical directory. So:

- **Reserved namespace:** coordination files live in `<root>/appdirs/<app>/d/<destination>/`, a new Yoho-owned tree under root, never next to Destination directories (a Destination named `prod.gate` can't collide). It contains `gate/` (a mkdir lock with `info.json`), `migrate.json` (the journal) and `link.tmp` (a temporary symlink). `<root>/appdirs` gets the same owner and mode as `<root>/apps`. Publishing `link.tmp` over the canonical path is a cross-directory rename, so Yoho requires `<root>/appdirs` and `<root>/apps/<app>` to be on the same device and refuses otherwise.
- **Gate:** `<root>/appdirs/<app>/d/<destination>/gate/`.
- **App gate:** `<root>/appdirs/<app>/app.gate/` (outside `d/`, so it can't collide with any Destination name). Migration holds it together with its Destination gate for the whole move. `RetainedImageRefs` (and any other scan across Destinations, such as dashboards that prune or report retention) takes it briefly around the scan. When the App gate is held, or **any** `d/*/migrate.json` exists, image pruning is skipped with a warning naming the migrating Destination. It never prunes from an incomplete scan, and the deploy itself still succeeds. Read-only dashboards may show a "migrating" state instead.
- **Every lock taker holds the gate briefly.** `AcquireLock` becomes: `mkdir gate` (on failure, report `held` with the gate's info, retrying a few times first because normal holds last milliseconds) → refuse if a journal exists → resolve the canonical state (create when missing, as the table says) → `mkdir <canonical>/lock` → `rmdir gate`. That covers deploy, rollback, Backup, Backup check and Scheduled Job runs, because they all call `deploy.AcquireLock`.
- **Scheduled Job installation** (`yoho schedule install` and the install inside `apply`) takes the full lock (gate, then the inner lock, through `AcquireLock`) for all its App directory writes: `EnsureHMACKey`, `scheduled/`, credentials and the job spec. The lock is released only after the last write.
- **Server builds:** creating a `source.run-<id>` directory and publishing `source/` in `finishSource` also take the gate briefly and refuse while a journal exists. `yoho apply` / `yoho deploy` ensure the App directory (and run any migration) **before** the build step, not only at rollout.
- **Migration and resume hold only the gate**, for their whole duration. They never take or need the inner lock. Because every lock taker passes through the gate, holding the gate while the physical tree has no `lock/` directory proves the App is not in use. Migration and resume locate that tree through the journal's physical `from`/`to`, not through the canonical path. They refuse when the physical tree contains `lock/` (a running or crashed deploy, Backup or job) or any `source.run-*` (an active or crashed build). The refusal names the path and gives a removal hint for stale ones.
- Older Yoho binaries don't know the gate. The docs say: upgrade every CLI that deploys this App before setting `app_dir`. After the move, older binaries still work through the symlink, but a migration racing an old binary is unsupported.
- A crash leaves the gate behind. The `held` error names the gate path and its holder and gives the manual removal hint the lock already gives.

## Migration journal and steps

Changing `app_dir` on a deployed App moves state that cannot be regenerated (generated secrets, `hmac.key`, retained Releases). A change always moves the directory and never starts empty.

- It runs in `yoho deploy` / `yoho apply` / `yoho rollback` before any build or rollout. `yoho plan` shows `app_dir: <old> → <new>`, and `apply` asks for confirmation like any other plan change (ADR 0008).
- **Same filesystem only:** the `stat -c %d` of the source must equal that of the target's parent. A same-filesystem `mv` is a rename, so inodes are kept and running containers keep their bind mounts. Across filesystems Yoho refuses and prints documented maintenance steps. It never copies and deletes automatically.
- **Journal:** `<root>/appdirs/<app>/d/<destination>/migrate.json` (outside the moving tree, 0600), written **before** the first rename: `{"kind": "forward"|"back"|"retarget", "from", "to"}`. While a journal exists, lock takers refuse with a "migration in progress or interrupted; rerun deploy/apply" hint. The next deploy/apply resumes it.
- The marker is written into the source directory **before** the journal, so the moved tree always carries its ownership proof.

| kind | steps (under the gate only) | resume, by observed state |
|---|---|---|
| forward (real canonical → `app_dir`) | marker → journal → `rmdir` an empty `to` → `mv canonical to` → `ln -s to canonical` → drop journal | canonical real dir and `to` missing/empty → redo the rename. Canonical missing and `to` marked → create the link. Link → `to` → drop the journal |
| back (link → real canonical) | journal → `rm canonical` (the link only, no trailing slash) → `mv from canonical` → drop journal | link still present → remove it. Canonical missing and `from` marked → rename. Canonical real dir and marked → drop the journal |
| retarget (`from` → `to`) | journal → `rmdir` an empty `to` → `mv from to` → `ln -s to link.tmp` → `mv -T link.tmp canonical` → drop journal | `from` marked and `to` missing/empty → rename. `to` marked and canonical still → `from` → publish the link: if `link.tmp` exists, it must be a symlink owned by the deploy UID; when its target is exactly `to`, move it, otherwise remove that link (in Yoho's reserved directory, never a target) and recreate it. Canonical → `to` → remove any leftover `link.tmp`, then drop the journal |

- Each resume step re-checks the trust rules (rule 2, the markers) before touching anything. Any state the table does not list makes Yoho refuse and print the journal and the observed state. It never guesses.
- A stale gate after a crash is removed by hand following the hint. The next deploy/apply then takes the gate and resumes the journal; no inner lock is involved.
- Containers are not recreated by the move itself.

## Server-side consumers

- `schedule run`, Backups, hooks and dashboards keep using the canonical path. They need the trust exception and the gate (both above), not new path logic.
- Removing a Destination (if a command does this) removes the symlink and, separately and only with a matching marker, `app_dir`. It never runs `rm -rf` through a trailing slash on the link.

## Code touch points

- `internal/config/types.go`: `Destination.AppDir string \`json:"app_dir,omitempty"\`` with a doc comment (schema follows the json tags), plus validation in `validate.go`.
- `internal/release`: document the symlink in the layout comment. `AppDir` doesn't change.
- New `internal/deploy/appdir.go` (or `release`): `EnsureAppDir(ctx, host, app, dest, appDir)` with the state table, the migration and marker logic, and a `Check` mode for `plan`.
- New `internal/safepath`: the trust script moved from `internal/backup/paths.go`, plus the trusted App link exception.
- `internal/deploy/lock.go`: the gate (in `<root>/appdirs`) and journal checks in `AcquireLock`. `internal/setup` creates `<root>/appdirs`.
- `internal/cli/schedule.go`: hold `AcquireLock` around `EnsureHMACKey` and `schedule.Install`.
- `internal/build`: the gate around `source.run-*` creation and `finishSource`.
- Call sites that ensure/migrate before work: deploy and apply (before the build step), rollback, `schedule install` (it writes `hmac.key` and `scheduled/`). Compose and Swarm run it on every Server of the Destination.
- `internal/plan`: an `app_dir` change entry.
- UI: step "App directory" with timing and a hint for each refusal.
- Docs: `docs/site/configuration.md` (setting, rules, migration, same-filesystem limit), `docs/decisions.md` entry (layout stays canonical, `app_dir` is a symlink target), schema regeneration if it is generated.

## Tests

- Unit: validation cases (relative, unclean, `/`, inside/ancestor of root, duplicate/nested across Destinations), the state-table script against a temp dir via `remote.Local` (each row, including refusals and interrupted migrations), marker mismatch, non-empty directory, cross-filesystem refusal (inject the device check).
- `RetainedImageRefs` still sees Releases behind a symlink.
- E2E (`YOHO_E2E=1`): deploy a test App with `app_dir`, redeploy, rollback, Scheduled Job run, Backup + restore, move `app_dir` to another path, remove `app_dir` again. Containers keep running and secrets resolve after each move.

## Out of scope

- A per-App override of `root`, the backup spool, or job specs.
- Templates such as `app_dir: /home/deb/apps/{app}-{destination}`.
- Renaming the App or the compose project (needs volume migration; separate work).
- Cross-filesystem automatic migration.

## Review log

Round 1, oai-sol-review (gpt-6.1-sol). All findings were accepted after checking them against the source:

- P1 Backup trust rejects the App symlink (`internal/backup/paths.go` `yoho_private`/`yoho_path`, `generated.go`) → "Backup trust exception".
- P1 The lock inside the moving tree is reachable only by pathname (`internal/deploy/lock.go`) → "Coordination: a gate outside the moving tree".
- P1 Interrupted migrations had no journal; the reverse temp path was unspecified → "Migration journal and steps" (marker before the journal, no temp path for back).
- P1 Physical ancestor trust and physical root containment → Ownership rule 2.
- P1 An existing symlink needs ownership proof before it is moved or used → Ownership rule 3.
- P2 Server builds write outside the deploy lock (`internal/build/build.go`, `internal/cli/apply.go`) → gate for builds, migration before the build step.

Answers to the open questions:

1. Keep the canonical symlink, with explicit trusted-link support.
2. `app_dir` stays per Destination only.
3. Migrate automatically in a confirmed `apply` / `deploy` (fits ADR 0008), now that coordination and recovery are specified.
4. Same filesystem only, with documented manual steps for cross-filesystem moves.

Round 2 (same thread). All findings were accepted:

- P1 Resume needed the unreachable inner lock → migration and resume hold only the gate and check that the physical tree has no `lock/`.
- P1 Scheduled Job install writes without the lock → install holds `AcquireLock` across `EnsureHMACKey` and `schedule.Install`.
- P1 `yoho_private`'s terminal `! -L` check → it applies to the trusted physical target.
- P2 Retarget couldn't resume after creating the temporary link → a fixed `link.tmp` in the reserved namespace, validated and then published or recreated.
- P2 The physical containment check rejected the sibling layout → compare the resolved candidate directory itself.
- P2 Metadata names could collide with Destination names → the reserved `<root>/appdirs/<app>/d/<destination>/` tree, and Destination names must match the App pattern when `app_dir` is set.

Round 3 (same thread): P1 retention scans across Destinations raced migration → App gate `<root>/appdirs/<app>/app.gate/`, held by migration and briefly by scans. Pruning is skipped (with a warning) while the App gate is held or any journal exists. Per-Destination metadata moved to `<root>/appdirs/<app>/d/<destination>/`. Accepted.

### Implementation notes

Implemented as specified. Code map: `internal/safepath` (trust functions moved from `internal/backup/paths.go`, the trusted App link exception `yoho_applink`/`yoho_linkok`, `yoho_make`, the marker, the gate helpers and `LockTaker`), `internal/deploy/appdir.go` (`EnsureAppDir` state table, migrations, journal resume; `ReadAppDir`/`PlanAppDir` for `yoho plan`), `internal/deploy/lock.go` (gate and journal refusal in `AcquireLock`), `internal/deploy/cleanup.go` (App gate in `RetainedImageRefs`), `internal/build/build.go` (gate around `source.run-*` creation and `finishSource`), `internal/cli/appdir.go` plus `apply.go`, `deploy.go` (rollback) and `schedule.go` (call sites, the "App directory" step, plan entries, the lock held across `EnsureHMACKey` and `schedule.Install`), `internal/config` (field and validation), `internal/setup` (`<root>/appdirs`), `internal/backup` (callers that touch the App directory pass `yoho_applink`; behaviour otherwise identical, existing tests unchanged). Where the plan and the code disagreed, the safer option was taken:

- `AcquireLock` cannot "create when missing, as the table says": its callers include `schedule run` on the Server, which does not know `app_dir`. With the gate held it refuses during a journal, uses a real directory or the trusted App link (full rule-3 check), refuses anything else, and creates a missing canonical path as a real directory (the old layout). Deploy, apply, rollback and `schedule install` run `EnsureAppDir` before they lock, so a configured `app_dir` is in place first; a real directory created by an early Backup is migrated forward by the next deploy.
- Every link, including forward and fresh ones, is published through `link.tmp` and a rename instead of `ln -s to canonical`: `ln -s` onto a path that an older binary recreated as a directory would put the link inside it, while the rename refuses. So the `<root>/appdirs` / `<root>/apps/<app>` same-device check applies to fresh, forward and retarget.
- Renames use `mv -T` (GNU) so an existing directory is never merged into; without it they fall back to plain `mv` only when the target is absent. The canonical link is replaced with `mv -T`, else BSD `mv -h`, and checked with `readlink` afterwards. The device check is `stat -c %d` with a BSD `stat -f %d` fallback; tests inject it.
- Extra validation: when `app_dir` is set, `root` must use the same safe characters (the journal is parsed by `sed` on the Server and paths must stay unambiguous in it), and retarget refuses when the old and new `app_dir` are nested.
- The journal is written with noclobber directly (no temporary file), keeping the reserved namespace to `gate/`, `migrate.json` and `link.tmp`; a journal that does not parse is refused with its content, like any unlisted state.
- `EnsureAppDir` takes the App gate only when it moves or resumes, so a no-op deploy never contends with retention scans. The App gate is re-entrant within one run (resume, then a new move).
- Using a canonical link or an existing empty `app_dir` requires mode 0700 (Backups check the App directory with `yoho_private`; Yoho never repairs modes). An empty target that a move renames over is removed first, so its mode does not matter.
- Nested subshell calls in the trust functions now end in `|| yoho_bad`, so they fail closed even when a function runs in a conditional context (`set -e` is ignored there). Top-level behaviour is unchanged.
- `schedule install` prepares the binary first (it can cross-build), then holds `AcquireLock` across `EnsureHMACKey` (once, not per job) and `schedule.Install`.
- Not done: no command removes a Destination today, so the removal rule has no call site; read-only dashboards do not show a "migrating" state (optional); there is no `yoho status`, the "App directory" step prints the physical path instead. The E2E scenario was not run; `mv -T` and `stat -c` are exercised only on Linux (macOS tests run the fallbacks).


## Code review of v0.1.7 (fixed in v0.1.8)

oai-sol-review (gpt-6.1-sol) on commit 8c68490:

- P2 An `app_dir` inside another App's directory, configured in a different Yoho file, was accepted. Fixed: `yoho_candidate` refuses when any physical ancestor holds a `.yoho-app.json`.
- P2 Server builds held the Destination gate across `cp -a`, so concurrent lock takers could time out. Fixed: the gate covers only creating `source.run-*` and the cache renames. `cp -a` and the final `rm -rf` run after `yoho_gate_drop`. Migration still refuses while `source.run-*` exists.

agy-review (gemini-4-argon-medium, effort medium). The subagent checked every finding against the source; no P1 survived:

- P3 An unreadable non-empty target counted as empty, and the migration then failed after writing the journal. Fixed: `yoho_empty` refuses when `ls` fails.
- P3 `yoho_nested` compared paths lexically only, so a retarget nested through a root-owned alias failed after writing the journal. Fixed: it compares both the lexical and the physical paths, and resume checks it too.
- P3 An already-correct link skipped root containment. Fixed: the use state runs `yoho_candidate` on the target.
- P3 Ordering: forward now validates the directory before writing the marker (only when `app_dir` is set, so default layouts are unchanged). Resume back checks the device before removing the link. Resume forward checks that `meta` and the canonical parent share a device before publishing.
- P2 Rollback read Releases before converging the App directory. Fixed: `ensureAppDirs` runs right after connecting.
- P3 The retention scan script lacked `set -eu`. Fixed.
- Not changed: `mv` without `-T`/`-h` aborting after the directory moved (resume recovers), apply's read-only plan running before ensure (by design: the plan must show the change), and the same-UID races outside the threat model.
