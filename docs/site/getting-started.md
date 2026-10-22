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

## After the first deploy

```sh
yoho app ps
yoho app logs web -f     # name the Service, or --all
yoho app stats
yoho releases
yoho rollback <version>
```

With several Destinations pass `-d production`. Add `--json` for NDJSON events.
