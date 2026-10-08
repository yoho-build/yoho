# Getting started

## Install

```sh
curl -fsSL https://yohobuild.com | sh
```

That downloads the binary for this machine from GitHub Releases, checks its SHA-256 against `checksums.txt`, and installs it to `/usr/local/bin` when that directory is writable, otherwise `~/.local/bin`. The script never runs sudo; if the destination is not writable it prints the command to run. Pin a version with `YOHO_VERSION=v0.1.0` or choose a directory with `YOHO_INSTALL_DIR`. If the repository is private, set `GITHUB_TOKEN` or `GH_TOKEN` before running the installer.

Build from source with Go 1.26: `go install github.com/yoho-build/yoho/cmd/yoho@latest`.

## 60 seconds

```sh
yoho init            # yoho.yml, .yoho/secrets, sample hooks
yoho config check    # validate config, compose files, secret references
yoho setup           # provision a fresh Debian/Ubuntu Server (interactive)
yoho deploy          # build, ship, cut over
```

## Minimal files

```yaml
# yoho.yml
app: shop
servers:
  primary: { ssh: yoho@203.0.113.10 }
destinations:
  production: { servers: [primary] }
```

```yaml
# compose.yaml
services:
  web:
    build: .
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://127.0.0.1:3000/up"]
      interval: 5s
    x-yoho:
      proxy: { hosts: [shop.example.com], port: 3000 }
```

A proxied Service needs a `/up` endpoint (configurable with `health_path`) that returns 2xx when ready, and a compose `healthcheck` (an error under swarm, a warning under compose). The image needs wget or curl.

## After the first deploy

```sh
yoho app ps
yoho app logs -f
yoho releases
yoho rollback <version>
```

With several Destinations pass `-d production`. Add `--json` for NDJSON events.
