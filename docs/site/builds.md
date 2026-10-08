# Builds & image transport

## Where images are built

`builder.location`:

- `local` (default): your machine.
- `remote`: a dedicated build Server (`builder.remote: user@host`).
- `server`: sync source to the Destination's Server and build there (`builder.exclude` skips paths). Needs `rsync` on the Server; `yoho setup` installs it, and deploy fails with a hint if it is missing.

## Local engine

`builder.engine: auto` uses Apple `container` on Apple silicon Macs when installed, else Docker. Set `docker` or `container` to force one.

```sh
yoho builder status   # which engine Yoho will use and Apple container state
yoho builder setup    # install/upgrade Apple container, Rosetta, builder VM; smoke-test a linux/amd64 build (-y: no prompts)
```

If Apple container is older than the latest release, `yoho builder status` and builds that use it print `run yoho builder setup to upgrade (needs sudo)`; the build check runs at most once a day, skips quietly when the network fails, and stays off when `YOHO_NO_UPDATE_CHECK=1`.

## Cross-builds

Servers are usually amd64. `builder.platforms` defaults to the Server's architecture, so an Apple silicon laptop produces `linux/amd64` images through Rosetta. Override with `platforms: [linux/amd64, linux/arm64]`.

## Build secrets

`builder.secrets: [NPM_TOKEN]` passes `--secret id=NPM_TOKEN,env=NPM_TOKEN`; use `RUN --mount=type=secret,id=NPM_TOKEN` in the Dockerfile.

## Transport

`transport.mode: auto` tries, in order:

1. `pussh`: layer-diff push over SSH (unregistry style); needs Docker's containerd image store on the Server (`yoho setup` can enable it).
2. `load`: `docker save | ssh docker load`.
3. `registry`: configure `registry:`.

Image tag is the Version (git SHA, or `_uncommitted_<8 hex>` from a hash of the uncommitted content, with a warning). Use `yoho deploy --version V` to override, `--skip-build` to reuse built images.
