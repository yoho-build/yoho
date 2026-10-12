# Server builds sync every build context and honor .gitignore and .dockerignore

## Context

Monorepos share code between apps through workspaces (bun, pnpm, npm, yarn): a root `package.json` and lockfile plus `packages/*`, consumed as `workspace:*`. An app in such a repo builds with the repo root as its Docker context and its own Dockerfile, as in Kamal's `builder.context`:

```yaml
# apps/shop/web/compose.yaml, next to apps/shop/web/yoho.yml
services:
  web:
    build:
      context: ../../..                 # repo root
      dockerfile: apps/shop/web/Dockerfile
```

`builder.location: local` and `remote` already handle this, because Docker reads the context from the operator's disk. `builder.location: server` does not:

- it rsyncs only the Yoho file's directory (`syncSource`, `RsyncArgv`);
- `ServerScript` rejects any context, Dockerfile or file secret outside that directory;
- it honors `.gitignore` but not `.dockerignore`, so files Docker would never use still travel to, and stay on, the Server.

Moving `yoho.yml` to the repo root syncs the whole repo on every deploy. Building locally means a cross-architecture build on the laptop, and shipping the image.

## Decision

### 1. Build inputs

For each Service with a `build` section, Yoho resolves its build inputs, using the same resolution as `localSpec` (relative to `Project.WorkingDir`):

- **Context.** A local directory. Remote contexts (git URLs, `http(s)://`) keep today's handling and take no part below.
- **Dockerfile.** The resolved path. Without a `dockerfile` setting, the file BuildKit loads: `Dockerfile`, else `dockerfile` (names compared exactly, since a case-insensitive volume answers for both). `dockerfile_inline` has no file.
- **Ignore file.** This is the file BuildKit opens, as BuildKit chooses it: `<Dockerfile>.dockerignore` next to the Dockerfile if it exists, else `<context>/.dockerignore`. An inline Dockerfile uses only `<context>/.dockerignore`. Patterns are always relative to the context root. An ignore file that exists but cannot be read fails the build; treating it as missing would ship what it excludes.
- **Additional contexts.** Each local `additional_contexts` entry: a directory, filtered by its own `.dockerignore` only, or a single file. Image and remote entries (`docker-image://`, URLs) are passed through unchanged and do not count as inputs. `oci-layout://` is rejected for server builds (§3).
- **File secrets.** Each file-backed build secret.

`build.ssh` stays out of scope: those key paths are agent sockets and keys on the operator's machine, not build inputs.

### 2. Transfer root

- The transfer root is the deepest common ancestor of the Yoho file's directory and every input path.
- Containment uses physical paths (`filepath.EvalSymlinks`). For an input file this covers its name as written and every link of its symlink chain, not only the target. The transfer root must lie inside the git work tree containing the Yoho file (`git rev-parse --show-toplevel`). Otherwise the build fails, so Yoho never syncs a parent directory outside the repository, and a context symlinked elsewhere is rejected.
- Outside a git work tree, every input must be inside the Yoho file's directory, as today.
- When every input is inside the Yoho file's directory, the transfer root is that directory, as today.
- If the transfer root is wider than the Yoho file's directory and `git` or the work tree is unavailable, the build fails. It does not fall back to syncing everything.
- Any `git rev-parse` failure counts as "no work tree"; Yoho does not parse git's (localized) messages. In the narrow case that only skips the gitignore step.

### 3. What ships: a keep set, then a computed exclude list

The transport stays as it is: `rsync -az --delete` of the transfer root into the per-run source directory, seeded from the cached source, with a NUL-separated `--exclude-from` list and `--delete-excluded`. `--delete-excluded` is always passed, even when the computed list is empty, so a seeded file that `builder.exclude` now matches is deleted rather than built. That mechanism already works with both macOS openrsync and GNU rsync, and rsync keeps symlinks as links, permissions and empty directories.

`--exclude-from` lines can only exclude. So Yoho first computes what must ship (the keep set), then excludes everything else. All paths are relative to the transfer root, slash-separated.

**1. Keep set.** Start with the files and directories each context would send. For each context and each local additional-context directory:

- Walk it with `filepath.WalkDir`, without following symlinks.
- Apply its ignore file from §1, parsed with `github.com/moby/patternmatcher/ignorefile` and matched with `PatternMatcher.MatchesUsingParentResults`.
- Thread the returned `MatchInfo` from each directory to its children, and restore it for siblings. Never call the deprecated singular `MatchesUsingParentResult`.
- Skip an excluded directory unless a `!` pattern could match below it, as BuildKit's fsutil walker decides: a `!` pattern with a wildcard (other than a trailing `/**` or `/*`) always could; otherwise its literal path must lie below the directory. Descend into it only then.
- A permission error under an excluded directory is ignored, as fsutil does. An unreadable path that would ship fails the build.
- A directory included by the ignore file is kept even when empty, because Docker can copy it.

Then remove three things from the set:

- with `builder.gitignore` on (the default), paths git ignores. Yoho runs `git ls-files --others --ignored --exclude-standard --directory -z` once from the work tree root, with `core.quotepath=off` and the config isolation `internal/version` uses. A reported directory removes everything below it. The paths are rebased onto the transfer root.
- `.git` at any depth;
- `.yoho/secrets*` at any depth, found by the walk.

Last, add the never-excluded paths, regardless of the gitignore and dockerignore rules:

- every Dockerfile;
- every ignore file in use, including each additional context's `.dockerignore`;
- every file secret.

Each of these ships by its name as written and with every link of its symlink chain, because BuildKit opens a Dockerfile or ignore file by its expected name (`app/.dockerignore -> ../rules/shared.ignore` ships both). On the Server, Yoho passes the name as written, so `<Dockerfile>.dockerignore` is found beside it. Directories map through symlinks to their physical path. rsync ships links verbatim, so a symlinked input file must be a relative link whose path has no directory symlinks; an absolute target (which would point at the operator's path on the Server) a link through a directory symlink (which does not ship, so the link would dangle), or a non-normalized target such as `via/../x` (lexical cleaning would hide a directory symlink) fails the build, naming the input and the link.

**2. Ancestors.** Every ancestor directory of a kept path is kept, so rsync can create it.

**3. Exclude list.** Walk the transfer root, without following symlinks:

- A directory with no kept descendant, and not kept itself, gets one exclude line, and its subtree is skipped. This turns `apps/mac`, with its 9,300 files, into a single line.
- A file that is not kept gets its own exclude line. This includes stray files in ancestor directories such as `apps/shop/.env`.
- `.git` and `.yoho/secrets*` entries found by the walk are always emitted, even under `builder.gitignore: false`.

Each line is `/` plus the path relative to the transfer root, escaped with `rsyncEscape`. rsync treats `*`, `?` and `[` in a pattern as wildcards, and anchors any pattern that contains a slash. `builder.exclude` patterns are appended as written, relative to the transfer root, and can remove kept paths; that is the user's choice.

**When gitignore cannot be used.** Outside a git work tree, with every input inside the Yoho file's directory (the only case §2 allows), the gitignore step is skipped. The dockerignore, `.git`, `.yoho/secrets*` and `builder.exclude` rules still apply, and the deploy does not fail.

**Why the rules lean toward shipping.** Excluding a file Docker would have used builds a different image. Shipping an extra file is harmless, because Docker applies the ignore file again on the Server. So:

- A dockerignore that fails to parse fails the build with the file and line.
- The walk is tested against BuildKit's fixtures, including a parent excluded by one pattern and a child re-included by a later `!` pattern (the buildx #850 shape).
- Every exclude line is tested against a transfer root that is not the git top-level, including names with spaces and `[`, such as `my file [1].txt` and `pkg/[a] b/`.

**Permissions.** The per-run source directory (`<cache>.run-<id>/source`) lies under a private `0700` directory that Yoho creates fresh and checks before any seeding or transfer: a directory, not a symlink, owned by the deploy user, with no group or other permission bits (setgid and sticky bits are allowed). The check reads mode bits with `find -perm`, not `ls` output. That directory itself is never a sync destination, so `-a` cannot widen its mode, and synced files (file secrets included) are never readable by other users on the Server. Today's Server source path already sits under the `0700` Yoho root; the check makes that a requirement. After a successful build the source is made `0700` and replaces the seed copy (`<cache>`) under a `mkdir` lock (`<cache>.lock`), and the cache is checked again inside the lock, so a concurrent build cannot recreate the cache between the check and the move and have the source moved into it. The lock is held only for two renames; one older than an hour (`find -mmin +60`, read the same way by GNU and BSD find, unlike `-mtime +0`) was left by a killed run and is removed.

**Unsupported inputs.** `oci-layout://` additional contexts point at a local directory, which a server build would have to transfer and rewrite. They fail with a clear error, telling the user to use `builder.location: local` or `remote`.

### 4. `builder.gitignore`

`builder.gitignore` is a `*bool`:

- unset or `true`: git-ignored paths are excluded;
- `false`: they ship. Use it when a build needs ignored files (generated code, vendored artifacts).

The dockerignore, `builder.exclude`, `.git` and `.yoho/secrets*` rules still apply when it is `false`.

### 5. Paths on the Server

Contexts, Dockerfiles, local additional contexts and file secrets map to `<source>/<path relative to the transfer root>`. `--build-context name=<path>` is rewritten the same way; non-local values are passed unchanged. Build flags and image caching are otherwise unchanged.

### 6. Versions

The Version is the git SHA, plus `_uncommitted_<hash>` over `git diff HEAD` and untracked, non-ignored files across the whole repository. It is computed before the `pre-build` hook. Two things therefore do not change the Version:

- git-ignored files shipped with `builder.gitignore: false`;
- files a `pre-build` hook writes;
- uncommitted changes inside submodule working trees (the gitlink SHA does count).

The docs say so. To get a distinct Version, commit the inputs or pass `--version`. Hashing exactly the shipped bytes is deferred work (see the backlog).

### 7. Checks and output

- **`yoho config check` warns when:**
  - a server build's transfer root is wider than the Yoho file's directory and the context has no ignore file (everything not git-ignored under the context ships);
  - a file secret sits inside a context and the context's ignore file does not exclude it, so Docker would also add it to the context. Every file secret of every Service is checked against every context and local additional-context directory of every Service, because all of them ship.
- **The build step prints** the transfer root, and the number and total size of shipped files. These are computed by Yoho from its own walk (`Lstat`), not parsed from rsync output.
- **Two kept paths that differ only by case** fail the build, because they would collapse into one file on the Server. Yoho checks this against the git index (`git ls-files -z` from the top-level, compared case-insensitively), because a case-insensitive Mac volume shows only one of the two entries in a walk.

### 8. Dependency

Add `github.com/moby/patternmatcher` (Apache-2.0, no transitive dependencies; the `ignorefile` package is part of it). The user approved this on 2026-10-12.

## Consequences

- **Behavior change.** Existing server builds also stop shipping dockerignored files, with the same transport and cache. Their Server source directories get smaller, and a file excluded by both the dockerignore and Yoho is no longer present. Nothing else changes for builds whose inputs sit inside the Yoho file's directory.
- **Monorepos.** Workspace monorepos build on the Server with shared packages. Only what Docker would use, plus the Dockerfile and its ignore file and file secrets, is transferred.
- **`builder.exclude` is relative to the transfer root.** When a build widens, existing patterns are re-rooted. The docs show the migration. Widening also causes one full transfer, because the cached source layout changes.
- **Cost.** Each deploy walks the contexts and runs one `git ls-files`. The cost is proportional to the files under the contexts.

## Alternatives considered

- **Always sync the git top-level.** This ships unrelated parts of the repo, and changes the layout for every app.
- **`rsync --files-from` with `--link-dest`.** Its semantics differ between openrsync and GNU rsync (implied recursion and `--relative`, link-dest resolution). Removing the basis directory races with concurrent deploys. A computed exclude list keeps the proven path.
- **Tar the file list over SSH.** This is exact and portable, but loses rsync's incremental transfer on every deploy.
- **A `builder.context`/`sync_root` setting.** It is redundant with compose `build.context`, and can disagree with it. It could be added later as an override.
- **Build from `git archive HEAD`, like Kamal's default.** That drops uncommitted changes, which conflicts with `_uncommitted_` Versions.
- **Translate dockerignore patterns into rsync filters.** That is lossy: rsync and dockerignore differ on `**`, negation and anchoring. Yoho evaluates the patterns itself and hands rsync literal paths.
