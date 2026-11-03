# Getting started

Timed path from a blank Server to a live App (web + worker + Postgres), then a zero-downtime redeploy and a secret change: [5-minute quickstart](quickstart.md). Prefer to let your AI agent do this? See [Set up with your AI agent](agents.md).

## Install

```sh
curl -fsSL https://yohobuild.com/install.sh | sh
```

That downloads the binary for this machine from GitHub Releases, checks its SHA-256 against `checksums.txt`, and installs it to `/usr/local/bin` when that directory is writable, otherwise `~/.local/bin`. The script never runs sudo; if the destination is not writable it prints the command to run. Pin a version with `YOHO_VERSION=v0.1.0` or choose a directory with `YOHO_INSTALL_DIR`.

### Signed binary provenance

The default (unset `YOHO_VERIFY_PROVENANCE`, or `0`) checks SHA-256 only and does not require `gh`. To require signed provenance as well:

```sh
curl -fsSL https://yohobuild.com/install.sh | YOHO_VERIFY_PROVENANCE=1 sh
```

Install a current [GitHub CLI](https://cli.github.com/) yourself first. It must support `gh attestation verify` with `--hostname`, `--repo`, `--digest-alg`, `--cert-identity`, `--source-ref`, `--predicate-type` and `--cert-oidc-issuer` (checked against gh 2.101.0). Unknown flags on an older CLI fail closed. Use an existing `gh auth login --hostname github.com` login, or supply `GITHUB_TOKEN` / `GH_TOKEN` through the environment; `GITHUB_TOKEN` takes priority for both downloads and verification. Private releases need a token with access to release assets and attestations; private/internal attestation creation requires GitHub Enterprise Cloud. Verification needs network access to GitHub and the verifier's trust services. The installer does not install or log in to `gh`, retrieve tokens, or use sudo.

SHA-256 is checked first, then `gh` verifies the **downloaded binary's digest** against signed SLSA v1 build provenance from `yoho-build/yoho` on **github.com**, signed by `.github/workflows/release.yml` with GitHub's Actions OIDC issuer and the exact `refs/tags/<release-tag>` certificate claim. `GH_HOST` cannot redirect these checks. Before any GitHub CLI network request, the installer checks `gh config get api_host --host github.com` offline; unreadable configuration or an API alias other than the default or `api.github.com` is rejected. This preserves existing logins without sending credentials to a configured mirror. Without `YOHO_VERSION`, latest is resolved once to a concrete tag and that tag is used for both downloads and verification. Required verification rejects invalid tag values rather than passing them as options.

Missing/unsupported `gh`, authentication or network failures, missing attestations, or mismatched digest, repository, workflow or tag stop installation **before the destination is changed or the binary is executed**. There is no checksum-only fallback. Older releases published without attestations fail in required mode; leave the option unset or `0` only if you deliberately want checksum-only installation. Any other option value is an error.

This option verifies binary provenance, **not the installer script itself**; review or obtain that script through a trusted channel. It does not establish that an App, build, Backup or restore is safe. Separately, the Linux binary Yoho downloads for Scheduled Jobs verifies the same provenance policy by default, in Go and without `gh` (see [Where the binary comes from](scheduled-jobs.md#where-the-binary-comes-from)). Future GitHub Releases attest the exact published `dist/yoho-*` files after checksum generation. The Release workflow follows the latest Go 1.26 patch (`1.26.x` with `check-latest`) for security fixes without changing the project's minimum Go version.

Update an existing Yoho installation with either command (available from v0.5.2):

```sh
yoho upgrade
# alias:
yoho update
```

This resolves the latest stable GitHub release once, verifies its SHA-256 and signed build provenance in Go, and atomically replaces the running executable. Verification is required; `YOHO_VERIFY_PROVENANCE=0` does not disable it. A failed download or verification leaves the executable intact.

It works outside an App directory and supports `--json`. A current or newer stable version is left in place. Symlinks keep pointing to the updated executable; package manager installations must be updated with their package manager. The executable's directory must be writable by your user.

GitHub authentication follows `GITHUB_TOKEN`, then `GH_TOKEN`, then an existing `gh auth` login; public releases also work without a token. Network access to GitHub and Sigstore trust services is required.

Older binaries do not have this command: rerun the installer above once to obtain it.

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

A proxied Service needs a `/up` endpoint (configurable with `health_path`) that returns 2xx when ready, and a compose `healthcheck` (an error under swarm, a warning under compose). This example executes `wget`, so the image must include it. For a curl-only image, change the healthcheck test to execute `curl` instead.

## Service templates

`yoho init --template` writes ready-to-deploy Stateful Services into the compose file:

```sh
yoho init --list-templates
yoho init --template postgres,redis      # also works in a project that already has yoho.yml
```

| Template | Image | Backup | Generated secrets | `secrets rotate --generated` |
|---|---|---|---|---|
| `postgres` | `postgres:17-alpine` | `pg_dump --clean --if-exists`, restored with `psql` | `POSTGRES_PASSWORD` (`POSTGRES_PASSWORD_FILE`) | `ALTER ROLE`, then restart (detected) |
| `redis` | `valkey/valkey:9.0-alpine` (Redis-compatible, append-only file) | volume `redisdata` | none: reachable only on the App's network | n/a |
| `clickhouse` | `clickhouse/clickhouse-server:26.8-alpine` (LTS) | volume `clickhousedata`, container paused during the copy | `CLICKHOUSE_PASSWORD` (`CLICKHOUSE_PASSWORD_FILE`) | swap and restart (`rotate: {strategy: none}`): the image rewrites user `default`'s password from the file on every start; users made with `CREATE USER` keep theirs |
| `rustfs` | `rustfs/rustfs:1.0.4` (S3-compatible) | volume `rustfsdata` | `RUSTFS_ACCESS_KEY`, `RUSTFS_SECRET_KEY` (`*_FILE`); see [Object storage](object-storage.md) | swap and restart (`rotate: {strategy: none}`): RustFS reads the root keys from the files on every start; objects stay readable with the new keys |

Each Service is `stateful: true`, has `restart: unless-stopped`, a healthcheck over TCP on 127.0.0.1, a named volume, and no published ports; other Services reach it by name (`postgres:5432`, `redis:6379`, `clickhouse:8123`, `rustfs:9000`). Generated secrets are created on the Server at the first deploy; list them in `x-yoho.secrets` of each Service that connects. A ClickHouse volume Backup copies the whole data directory; for very large tables use ClickHouse's own `BACKUP` to an S3 bucket instead.

Without a compose file, `compose.yaml` is created. An existing one keeps its text and gets the Services and volumes appended, indented like the file. A Service or volume name that already exists is refused and no file is written; with a yoho.yml, that includes names in its other `compose:` entries and in every Destination overlay (`compose.<destination>.yaml`), which would otherwise merge into the template. The postgres dump holds only the `postgres` database (no roles or other databases); back up the `pgdata` volume or add `pg_dumpall --globals-only` if you need them. Image tags are pinned; update them yourself.

## After the first deploy

```sh
yoho app ps
yoho app logs web -f     # name the Service, or --all
yoho app stats
yoho releases
yoho rollback <version>
```

With several Destinations pass `-d production`. Add `--json` for NDJSON events.
