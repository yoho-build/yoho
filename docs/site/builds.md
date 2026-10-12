# Builds & image transport

## Where images are built

`builder.location`:

- `local` (default): your machine.
- `remote`: a dedicated build Server (`builder.remote: user@host`).
- `server`: sync source to the Destination's Server and build there. It ships what git would (tracked plus untracked files that are not git-ignored), so `node_modules/` and `.env.local` stay local; outside a git work tree everything ships. `builder.exclude` skips more paths, and `.git` and `.yoho/secrets*` never ship. Needs `rsync` on the Server; `yoho setup` installs it, and deploy fails with a hint if it is missing.

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

For Docker-built images, `transport.mode: auto` tries, in order:

1. `pussh`: layer-diff push over SSH (unregistry style); needs Docker's containerd image store on the Server (`yoho setup` can enable it).
2. `load`: `docker save | ssh docker load`.
3. `registry`: configure `registry:`.

Image tag is the Version (git SHA, or `_uncommitted_<8 hex>` from a hash of the uncommitted content, with a warning). Use `yoho deploy --version V` to override, `--skip-build` to reuse built images.
