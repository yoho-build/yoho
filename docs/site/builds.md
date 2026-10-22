# Builds & image transport

## Where images are built

`builder.location`:

- `local` (default): your machine.
- `remote`: a dedicated build Server (`builder.remote: user@host`). See [Remote builds](#remote-builds).
- `server`: sync the build inputs to the Destination's Server and build there with `docker build`. See [Server builds](#server-builds). Needs `rsync` on the Server; `yoho setup` installs it, and deploy fails with a hint if it is missing.

## Remote builds

With `builder.location: remote`, the build runs on the Builder, and by default the images stream from the Builder straight to the Servers, so they never cross your connection. `builder.direct` controls this:

- `auto` (default): direct when it can work, otherwise images come back to your machine and ship from there as with `location: local`. The Build step prints which path it took and why.
- `true`: always direct; the deploy fails when the Builder cannot reach a Server.
- `false`: always through your machine.

Direct shipping needs:

- One platform: the Servers' platform, or a `builder.platforms` with one entry. A multi-platform build ships from your machine.
- `transport.mode` `auto` or `load`. `pussh` and `registry` ship from your machine.
- SSH from the Builder to every Server that works without a prompt: the Builder's user needs its own key authorized on each Server and a `known_hosts` entry for each one. Yoho never copies keys or accepts host keys for the Builder; it probes with `ssh -o BatchMode=yes -o StrictHostKeyChecking=yes <server> true` before the build. The Builder connects to the Server's `ssh` address (user, host and port), so a name from your `~/.ssh/config` must resolve on the Builder too. Servers reached through a [Cloudflare Tunnel](cloudflare-tunnel.md) ship from your machine.

```yaml
builder:
  location: remote
  remote: build@10.0.0.2
  direct: auto # default
```

On the direct path Yoho builds with the Builder's own Docker through a docker context named `yoho-remote-<hash>-d` (`docker context ls`), so the image stays in that Docker. It then runs `docker save | gzip -1 | ssh <server> docker load` on the Builder, once per Server, in parallel, and skips Servers that already have the same image. The Ship step reports per Server `web via direct, 412.0 MB in 38s (10.8 MB/s)`, and `--json` adds the same in `transfers`. The fallback path builds with a `docker-container` buildx builder (`yoho-remote-<hash>`) and `--load`s the image to your machine first.

The Docker driver cannot export build cache, so a Service with `cache_to` (other than `type=inline`) needs `builder.direct: false`. Build secrets travel as before, through BuildKit's session from your machine; nothing secret reaches the Builder's shell or the Server. Built images accumulate in the Builder's Docker; prune them there (`docker image prune`) like on any build host.

## Server builds

With `builder.location: server`, Yoho rsyncs only what Docker would use (ADR 0009):

- every build context and local `additional_contexts` entry, filtered by its ignore file, as BuildKit chooses it: `<Dockerfile>.dockerignore` next to the Dockerfile, else `<context>/.dockerignore` (patterns are relative to the context);
- minus what git ignores (`builder.gitignore`, on by default) and, always, `.git` and `.yoho/secrets*` at any depth;
- plus every Dockerfile, ignore file in use and file-backed build secret, even when ignored.

The transfer root is the deepest directory holding the Yoho file's directory and every input. When the inputs sit outside the Yoho file's directory, the root widens up to, at most, the git work tree; outside git, inputs must stay inside the Yoho file's directory. Files in the root that no context uses are excluded, so a sibling app with thousands of files costs one exclude line. The build step prints the root and the number and size of the files that ship.

A bun, pnpm, npm or yarn workspace monorepo builds from the repo root, as with Kamal's `builder.context`:

```yaml
# apps/shop/web/compose.yaml, next to apps/shop/web/yoho.yml
services:
  web:
    build:
      context: ../../..                 # repo root
      dockerfile: apps/shop/web/Dockerfile
```

Keep the context small with an ignore file next to the Dockerfile, so it applies to this build only:

```text
# apps/shop/web/Dockerfile.dockerignore, relative to the context (repo root)
*
!package.json
!bun.lock
!packages/ui/**
!apps/shop/web/**
apps/shop/web/.env*
```

Without an ignore file, everything under the context that git does not ignore ships, and `yoho config check` warns. It also warns when a file secret sits inside a context and no ignore file excludes it, because Docker would add it to the context too.

- `builder.gitignore: false` ships git-ignored files, for builds that need generated code or vendored artifacts. The dockerignore, `builder.exclude`, `.git` and `.yoho/secrets*` rules still apply.
- `builder.exclude` adds rsync patterns, relative to the transfer root. When a build widens, re-root them: `node_modules` matches at any depth either way, but `/dist` becomes `/apps/shop/web/dist`. Widening also causes one full transfer.
- `oci-layout://` additional contexts are not supported on the Server; use `builder.location: local` or `remote`. `docker-image://` and URLs pass through.
- Two kept files whose paths differ only by case fail the build, because a case-insensitive Mac volume holds only one of them.

The Version still comes from git (see [Transport](#transport)), so it does not change for git-ignored files shipped with `builder.gitignore: false`, files a `pre-build` hook writes, or uncommitted changes inside submodules. Commit those inputs or pass `--version` to get a distinct Version.

## Local engine

`builder.engine: auto` uses Apple `container` on Apple silicon Macs when installed and its system service is running, else Docker. Set `docker` or `container` to force one. Apple container requires macOS 26 or later; AMD64 builds use Rosetta 2 in the ARM64 builder VM.

```sh
yoho builder status   # which engine Yoho will use and Apple container state
yoho builder setup    # install/upgrade Apple container, Rosetta, builder VM; smoke-test a linux/amd64 build (-y: no prompts)
```

If Apple container is older than the latest release, `yoho builder status` and builds that use it print `run yoho builder setup to upgrade (needs sudo)`; the build check runs at most once a day, skips quietly when the network fails, and stays off when `YOHO_NO_UPDATE_CHECK=1`.

## Cross-builds

Servers are usually amd64. `builder.platforms` defaults to the Server's architecture, so an Apple silicon laptop produces `linux/amd64` images through Rosetta. Yoho passes `--platform linux/amd64` to `container build`; no local Docker daemon is needed.

```yaml
builder:
  location: local
  engine: container
  platforms: [linux/amd64] # optional: detected from the Server by default
transport:
  mode: load
```

For manual setup, install Rosetta with `softwareupdate --install-rosetta --agree-to-license` and start the service with `container system start`, or use `yoho builder setup` above. Rosetta translates AMD64 Linux user-space binaries, not the kernel. Native dependencies may require compatibility testing; use a remote AMD64 Builder if they cannot run under Rosetta.

Override build targets with `platforms: [linux/arm64, linux/amd64]`. The image shipped is selected by the Server's architecture, not list order; the list must include that architecture. All Servers in a Destination must currently share an architecture; use separate Destinations for mixed-architecture Servers.

To verify the local build path from a Yoho checkout (no Docker or SSH Server required):

```sh
YOHO_E2E=1 go test ./internal/build -run '^TestE2EContainerAMD64Build$' -v -count=1
```

This builds an AMD64 image with a build secret, checks its architecture, runs it under Rosetta, and removes the test image. For build + SSH transport verification, run `YOHO_E2E=1 YOHO_E2E_SSH=user@amd64-server go test ./internal/transport -run '^TestE2EContainerPush$' -v -count=1`.

## Build secrets

`builder.secrets: [NPM_TOKEN]` passes `--secret id=NPM_TOKEN,env=NPM_TOKEN`; use `RUN --mount=type=secret,id=NPM_TOKEN` in the Dockerfile.

## Transport

Apple container images use `load` transport (also selected by `auto`): Yoho runs `container image save --platform <Server platform>`, adds Docker-compatible archive metadata, and streams the archive over SSH to `docker load` on the Server. This needs neither a local Docker daemon nor a registry. Explicit `pussh` or `registry` transport is not supported for Apple container images; use `builder.engine: docker` for those modes.

Remote builds with `builder.direct` ship from the Builder; see [Remote builds](#remote-builds). Everything else ships from your machine.

For Docker-built images, `transport.mode: auto` tries, in order:

1. `pussh`: layer-diff push over SSH (unregistry style); needs Docker's containerd image store on the Server (`yoho setup` can enable it).
2. `load`: `docker save | ssh docker load`.
3. `registry`: configure `registry:`.

Image tag is the Version (git SHA, or `_uncommitted_<8 hex>` from a hash of the uncommitted content, with a warning). Use `yoho deploy --version V` to override, `--skip-build` to reuse built images.
