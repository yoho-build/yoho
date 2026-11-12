# Preview environments

Deploy each pull request as an ephemeral Destination, with its own hostname,
compose project and empty named volumes. Use a dedicated staging Destination as
the template and preview-safe secrets: preview code can read those secrets and
perform the same external actions as staging code.

## Setup

Point wildcard DNS `*.preview.example.com` at the template Server. Configure:

```yaml
previews:
  from: staging
  host: "{name}.preview.example.com"
```

The template must exist and use the compose runtime with operator access. Its
Yoho and compose overlays, Servers and secrets selection are reused, including
`.yoho/secrets.staging` and provider values restricted to staging. Migration
Cutover, Backups, Scheduled Jobs and tunnel management are cleared. The template's
`app_dir` and fixed subnet are also cleared. External volumes, writable bind
mounts and fixed `container_name` values are refused; named volumes always get
preview-specific names. Volumes with a non-default driver or any `driver_opts`
are refused because renaming cannot isolate bind-backed or network storage.
Networks with fixed IPAM configuration and Services with static IPv4/IPv6
addresses are also refused; let Docker allocate them. Disable staging workers
and other unwanted side effects in the template's compose overlay.

For several proxied Services with hosts, include `{service}` as well:

```yaml
previews:
  from: staging
  host: "{service}-{name}.preview.example.com"
```

The wildcard then covers `web-pr-123.preview.example.com` and
`api-pr-123.preview.example.com`. Each rendered DNS label must fit in 63
characters. Generated hosts must differ from every hostname configured on the
template or any other Destination’s Services, including their compose overlays.
Names are lowercased, non-alphanumeric runs become dashes, outer
dashes are trimmed, and the result is truncated to 40 characters. Empty names
are rejected. Different branch spellings can normalize to the same name; use
`pr-123` in CI. Configured Destination collisions are refused.

TLS follows each template Service's `x-yoho.proxy.tls`. With `tls: true`, the
Proxy obtains individual certificates on demand for the generated hosts;
wildcard DNS does **not** provide a wildcard TLS certificate. Yoho has no
wildcard-certificate configuration here. For high-volume previews, terminate
wildcard TLS upstream and leave Service TLS off, with routing configured outside
preview management. See [custom domains](custom-domains.md) for the Proxy's TLS
behavior and the link to Let's Encrypt rate limits. Custom-domain catch-all
Services without explicit hosts are refused for previews.

## Commands

```sh
yoho preview up --name pr-123
yoho preview up --name pr-123 --version abc123 --skip-build
yoho preview list
yoho preview down --name pr-123 --yes
```

A preview named `N` uses Destination `preview-N`, App directory
`<root>/apps/<app>/preview-N`, and compose project `yoho-<app>-preview-N`.
This namespace keeps configured Destinations untouched: `--name staging` targets
`preview-staging`, never the configured `staging` Destination. A configured
Destination literally named `preview-N` is refused as a collision.

`up` uses the normal build, ship and deploy path and prints the URL(s). Run it
again to update the same preview. `--skip-build` uses image tags for this preview,
not the template's tags. The effective Yoho overlay configuration selects the
template through `previews.from`; that template’s own overlay is loaded and
validated as well. `--lock-wait` works on `up` and `down`.

`list` reads markers from the template's Servers and shows name, Server, hosts
and the last attempted Version. Markers must match the directory they occupy.
`down` asks for confirmation on a terminal;
`--yes` is required in CI and with `--json`. It verifies the preview marker
under the Destination lock and removes the preview's containers (including
one-offs), Proxy routes, networks, named and anonymous volumes, and App directory
with Releases and secrets. Unused built images are pruned using the same
retained-Release and container checks as `yoho remove`. **Preview data is deleted.**
Shared Proxy resources,
the shared `yoho` network, Backups and other Destinations are kept. An absent
preview succeeds with “not found; nothing removed” in human and JSON summaries;
unmarked resources and configured Destination namespace collisions are refused.
Before any App directory writes, previews refuse symlinked directories and
unmarked directories containing anything beyond `lock` and `.yoho-app.json`.
The same checks guard teardown; empty lock-only directories are cleaned up.
Failed first deploys leave a marker and can be torn down. Interrupted teardowns
can be retried. JSON output includes `preview.up` / `preview.down` events with
`name` and `hosts`.

There is one template per App, no Swarm or scoped Gate support, no Backup seeding,
and no automatic expiry. Retain the template's Server configuration until its
previews are removed; changing Servers does not move existing previews.

## GitHub Actions

The reusable workflow at
[`.github/workflows/yoho-preview.yml`](https://github.com/yoho-build/yoho/blob/main/.github/workflows/yoho-preview.yml)
installs Yoho and the 1Password CLI, configures SSH, updates previews on PR
activity and removes them on close. Use a Yoho release that includes preview
support, and an SSH target reachable directly from the runner (configure any SSH
aliases yourself in a custom workflow). Supply trusted `known_hosts` entries.

::: v-pre
```yaml
name: Preview
on:
  pull_request:
    types: [opened, synchronize, reopened, closed]
permissions:
  contents: read
jobs:
  preview:
    uses: yoho-build/yoho/.github/workflows/yoho-preview.yml@main
    with:
      yoho-version: latest
      app-dir: .
      ssh-known-hosts: ${{ vars.PREVIEW_KNOWN_HOSTS }}
    secrets:
      SSH_KEY: ${{ secrets.PREVIEW_SSH_KEY }}
      OP_SERVICE_ACCOUNT_TOKEN: ${{ secrets.OP_SERVICE_ACCOUNT_TOKEN }}
```
:::

Pin the workflow reference to a reviewed commit or release for production use.
The workflow uses `preview-<repository>-<PR number>` concurrency with
`cancel-in-progress: false`, so an update cannot cancel cleanup or strand a
remote deploy lock. It runs `yoho preview up --name pr-<PR number>` for opened,
synchronize and reopened events, and `yoho preview down --name pr-<PR number>
--yes` for closed events, using the PR head SHA and its configuration for both update and teardown.

**Do not use `pull_request_target` to check out and execute fork PR code with
secrets.** The workflow deliberately skips fork PRs and Dependabot jobs and refuses empty
SSH keys or trusted-host entries before writing SSH files. Same-repository PR code
must also be trusted: it runs builds and Hooks with operator SSH access and the
template's secrets. Review code before granting that access.

Closed-event jobs can be missed. In a scheduled workflow, run `yoho preview list`,
compare names with open PRs, then call `yoho preview down --name pr-N --yes` for
stale previews. Keep scheduled cleanup in the same per-PR concurrency group as
updates. Preview inventory has no automatic TTL or prune command.
