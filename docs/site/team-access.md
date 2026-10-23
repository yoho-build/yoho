# Sharing one App with a team

One Server often runs several Apps for one operator. Scoped access shares a single App Destination with teammates and CI: they can deploy, roll back, and read status, logs, stats and Releases, and open `yoho ui` and `yoho tui`. They get no shell, no Docker access, and nothing of the other Apps, and Yoho never hands them secret values. The code they deploy does run with the App's secrets, though, so granting `deploy` means trusting the Member with that App's secrets (see [Secrets](#secrets)). See [ADR 0011](https://github.com/yoho-build/yoho/blob/main/docs/adr/0011-scoped-access-through-a-gate.md) for the design.

## How it works

- The **operator** keeps full SSH. Every command works as before.
- A **Member** is a teammate or a CI job. Members log in as the `yoho-gate` Unix user, which is not in the `docker` group. Each Member key in `/home/yoho-gate/.ssh/authorized_keys` has a forced command, so the key can only run the **Gate**:

  ```text
  restrict,command="sudo -n /usr/local/bin/yoho gate --member alice --key SHA256:…" ssh-ed25519 AAAA… yoho-gate:alice
  ```

- The **Gate** (`yoho gate`) reads one JSON request on stdin, checks it, runs it, writes one audit line, and exits. Nothing stays running on the Server. A sudoers rule lets `yoho-gate` run only `/usr/local/bin/yoho gate *` as root. The Gate then runs the verb as the owner of the App directory, with the same code as an operator deploy.
- The Gate serves these verbs: `deploy`, `rollback`, `releases`, `ps`, `logs`, `stats` and `dash` (the dashboards). Every Member may also call `hello`. The Gate has no `exec` verb and no verb that changes access.

### What a scoped deploy may contain

A compose file can mount `/var/run/docker.sock` or `/`, or ask for `privileged`, so the Gate checks the compose file as well as the Member. It refuses the deploy, and lists every reason, when the project contains any of these:

- `privileged`, `cap_add`, `devices`, `device_cgroup_rules`, `gpus`, or `use_api_socket`
- any `network_mode` except `none` (`bridge` and `default` join Docker's shared bridge; leave it out to use the App's own networks), or any `pid`, `uts`, `userns_mode`, `cgroup: host`, `cgroup_parent` or `runtime`
- a `security_opt` other than `no-new-privileges`
- `volumes_from`, `external_links`, or a bind mount (named volumes and tmpfs are fine)
- external volumes or networks, including the shared `yoho` network (Yoho attaches proxied Services to the Proxy itself), volume `driver_opts`, or a volume or network name outside `yoho-<app>-<destination>_`
- a `container_name` outside `yoho-<app>-<destination>-`, or a label that starts with `com.docker.` or `yoho.`
- top-level `secrets`, or `configs` from a file or the environment (inline `content` is fine)
- `include`, `extends`, `env_file` or `label_file` that reads Server files (the CLI already inlines `env_file`)
- privileged `post_start` or `pre_stop` hooks, or `pre_start` containers that break any rule above
- an image other than `yoho/<app>-<destination>-<service>:<version>` for a Service the CLI built, or a Yoho-built image (`yoho/…` or under the Registry prefix) for any other Service or `type: image` volume
- a published host port that the operator did not allow (`limits.ports`), or a random host port
- `shm_size`, a tmpfs size, a `ulimit`, `pids_limit`, memory or CPU limit, or a replica count above the operator's limits, more containers in total than `limits.max_containers`, an unlimited (`-1`) ulimit or pids limit, `memswap_limit`, `cpu_quota`, `cpu_period`, `cpu_rt_*` or `storage_opt`
- a `pull_policy` other than `missing`, `if_not_present` or `never`
- no Services at all (that would stop the whole App), unless the operator sets `limits.allow_empty`
- a Proxy hostname that the operator did not allow

The Gate also checks the Server before it deploys. It refuses a name that another App Destination shares: `shop/prod-web` and `shop-prod/web` are both the compose project `yoho-shop-prod-web`, and `shop/prod` with a Service `web-api` builds the same image as `shop-prod/web` with a Service `api`. It refuses an existing volume or network of that name that is not this App's plain local one, an image tag that another App's build holds, and a local image (by name or ID) that was not pulled from its registry, such as another App's build.

On every container the Gate drops `NET_RAW`, sets `pull_policy: never`, sets `pids_limit`, memory and CPU limits to the operator's limits when the project sets none, and gives an unsized tmpfs the maximum tmpfs size.

The Gate pulls every image the deploy did not build before compose runs. Images under a registry in `limits.registries` are pulled with the Server's `docker login` when they are missing. All other images are pulled anonymously on every deploy, even when a copy is already on the Server, so a Member cannot use the Server's credentials to fetch a private image, and cannot run another App's local image by name or ID.

`release_command` runs in a one-off container with its Service's configuration, so these rules cover it too. Hooks run on the Member's machine, around the Gate call: `pre-connect`, `pre-build`, `pre-deploy` and `post-deploy`.

Images reach the Server in one of two ways. The CLI can stream a `docker save` archive with the deploy request. The Gate accepts one upload per App Destination at a time, up to `limits.max_upload_bytes` (compressed and unpacked) within `limits.upload_timeout_seconds`. It accepts only regular files and directories under plain relative names, so links and `..` cannot redirect `docker load`, checks that every tag in the archive belongs to this deploy, then loads it. With `transport.mode: registry`, the CLI pushes the images and the Gate pulls them with the Server's `docker login`. Builds run on the Member's machine only (`builder.location: local`).

### Other Apps on the Server

A scoped App does not join the shared `yoho` network. Its proxied Services join a network of their own, `yoho-<app>-<destination>-proxy`, and Yoho connects the Proxy to it (again after a Proxy upgrade). A Member's containers reach their own App and the Proxy, which serves only public routes, as from the internet. They cannot reach other Apps' containers by name or address. Logs, stats and the dashboards show only containers labeled with this App Destination.

### Secrets

The Member never sends or sees secret values through Yoho. Before Members deploy, the operator resolves the secrets on their own machine and stores them on the Server:

```sh
yoho access secrets push -d production
```

This writes a secrets generation and names it in `<root>/apps/<app>/<destination>/access-secrets`. Every scoped deploy reads that generation, whatever later deploys wrote, and retention keeps it. The push also writes the settings that scoped deploys use instead of the Member's Yoho file: the Proxy configuration, the Registry (without its password), `retain_releases`, and the Proxy hostnames the App may claim (the `x-yoho.proxy.hosts` of today's compose files, including the catch-all when a Service has `custom_domains` or no hosts). Run `yoho access secrets push` again after you rotate a secret, add a secret to a Service, add a hostname, or enable `custom_domains`.

Keeping the values on the Server keeps them off Member laptops and CI logs. It does not keep them from the App's code: a Member who can deploy can ship code that reads its secrets (`/run/secrets/…` or the environment) and sends them anywhere. Grant `deploy` only to people and CI jobs you would trust with that App's secrets. The secrets of other Apps stay out of reach.

## Operator: set up once

The operator needs root, or passwordless sudo (`sudo: true` on the Server), on the Server. Run these commands from the App's repository, as the operator:

```sh
yoho deploy -d production                  # the App Destination must exist on the Server
yoho access install -d production          # yoho-gate user, sudoers, /usr/local/bin/yoho
yoho access secrets push -d production     # secrets and settings for scoped deploys
yoho access add alice --verbs all -d production
yoho access key add alice alice.pub -d production
yoho access ls -d production
```

- `access install` is idempotent. It does these things:
  - installs this yoho version as `/usr/local/bin/yoho`, owned by root, with mode 0755
  - creates the `yoho-gate` user with no password (`*`), shell `/bin/sh`, and no `docker` group
  - creates a root-owned `~/.ssh` and `authorized_keys`
  - writes `/etc/sudoers.d/yoho-gate`, checked with `visudo -c`, with `env_reset` and a fixed `secure_path` for `yoho-gate`
  - writes `/etc/yoho/gate.json` (root-owned) with the Yoho root; the Gate reads the root only from this file
- `access add MEMBER --verbs …` grants verbs on the selected App Destination. Use a comma-separated list, `all`, or `read` (`releases`, `ps`, `logs`, `stats`, `dash`). Run the command again to change the verbs.
- `access key add MEMBER FILE.pub` authorizes one OpenSSH public key. One key belongs to one Member. If you add the same key again, Yoho moves it to the new Member.
- `access rm MEMBER` revokes the Member's verbs on this App Destination. `access key rm MEMBER` removes all keys of the Member. Both take effect on the next connection.
- `access audit [-n 50]` shows the last Gate requests as JSON lines: time, Member, key fingerprint, client address, verb, redacted arguments, and exit code. Requests for an App Destination without scoped access (probes, typos) go to `<root>/gate-audit.log`. Each log rotates to `<log>.1` at 16 MiB.

The state lives in `<root>/apps/<app>/<destination>/access/` (`members.yml`, `settings.json`, `audit.log`, and `vetted/`, the Gate's record of the Releases it deployed). The directory is owned by root with mode 0700, so `yoho-gate` cannot read or change it.

### Limits

`settings.json` has a `limits` object. `yoho access secrets push` keeps it, so edit it on the Server (as root):

| Field | Default | Meaning |
| --- | --- | --- |
| `max_upload_bytes` | 8 GiB | largest image archive, compressed and unpacked |
| `upload_timeout_seconds` | 1800 | time to receive an image archive |
| `ports` | none | host ports Services may publish |
| `max_shm_bytes` | 1 GiB | largest `shm_size` |
| `max_tmpfs_bytes` | 1 GiB | largest tmpfs, and the size of an unsized one |
| `max_ulimit` | 1048576 | largest value of any `ulimit` |
| `pids_limit` | 4096 | default and largest `pids_limit` |
| `max_memory_bytes` | 2 GiB | largest memory limit per container, and the limit of one without |
| `max_cpus` | 2 | largest CPU limit per container, and the limit of one without |
| `max_replicas` | 4 | largest `scale` or `deploy.replicas` of a Service |
| `max_containers` | 16 | most containers of the App in total |
| `registries` | none | repository prefixes (`ghcr.io/acme/`) pulled with the Server's `docker login` |
| `allow_empty` | false | allow a deploy with no Services |

```json
{ "limits": { "ports": [8443], "max_upload_bytes": 2147483648 } }
```

### Locks and interrupted deploys

A scoped deploy takes the same deploy lock as an operator deploy. If the Member's connection drops, the Gate finishes the deploy and releases the lock. If the Gate itself dies while it holds the lock (killed, or the Server rebooted), the next Gate request clears that lock. A lock held by the operator or a backup stays until that command ends; Members cannot remove it.

To upgrade the Gate after you upgrade Yoho, run `yoho access install` again. The Member's CLI and the Gate must speak the same protocol version.

## Member: use the usual commands

Mark the Destination as scoped. You can do this in the Yoho file the team shares, or only for yourself with `YOHO_ACCESS=scoped`:

```yaml
destinations:
  production:
    servers: [main]
    access: scoped        # use the Gate
    # gate_user: yoho-gate  (default)
```

```sh
yoho deploy -d production
yoho releases -d production
yoho rollback 3f2a1c -d production       # needs only the rollback verb
yoho app ps -d production
yoho app logs web -n 200 -d production     # -f follows
yoho app stats --no-stream -d production
yoho ui -d production                     # or yoho tui
```

With scoped access, the CLI connects as `yoho-gate@<host from servers.*.ssh>` with your own key. Commands that the Gate does not serve fail before they connect and tell you to ask the operator. These include `setup`, `apply`, `plan`, `proxy`, `tunnel`, `backup`, `schedule`, `app exec` and `secrets`.

`rollback` returns only to Releases that the Gate deployed. The Gate checks them again with today's rules before it redeploys, including the limits it wrote into the Release's compiled compose file. A Release that the operator deployed can do anything the operator can, so the Gate refuses it: ask the operator to roll back, or deploy that version again.

A Gate deploy or rollback is detached on the Server after images are loaded. If the Member's SSH session drops mid-rollout (exit 255) — for example SSH rides a Cloudflare Tunnel whose `cloudflared` is a Service of the App being deployed — the rollout still finishes and records the real Release status. `yoho deploy` reconnects and waits for that outcome; `yoho releases` shows it either way. While one Gate rollout runs, another deploy or rollback of that Destination is refused. `yoho config check` and `yoho plan` warn when `ssh: {via: cloudflare}` would go through a `cloudflared` in the same App.

An operator who shares the Yoho file sets `YOHO_ACCESS=operator` to use full SSH. The `yoho access …` commands always use full SSH.

### Deploy from CI

Generate a key for the CI job, authorize it, and store the private key as a CI secret:

```sh
ssh-keygen -t ed25519 -N '' -C ci-github -f ci-github
yoho access add ci-github --verbs deploy,releases,rollback -d production
yoho access key add ci-github ci-github.pub -d production
gh secret set YOHO_GATE_KEY -R OWNER/REPO < ci-github    # value on stdin
rm ci-github ci-github.pub
```

Yoho ships a reusable GitHub Actions workflow. It installs a pinned release (sha256 from `checksums.txt`, and the Sigstore build attestation when `gh` exists), writes the key to a temporary file, runs `yoho releases`, `yoho deploy` and `yoho releases` as the `yoho-gate` Member, optionally checks a URL, and removes the key at the end. When a deploy fails, the job summary lists the previous version and the exact `yoho rollback` command.

```yaml
# .github/workflows/deploy.yml in your repository
name: deploy
on:
  push:
    tags: ["v*"]
jobs:
  deploy:
    uses: yoho-build/yoho/.github/workflows/deploy.yml@v0.2.0
    with:
      working-directory: .
      destination: production
      ssh-host: 203.0.113.10
      ssh-known-hosts: ${{ vars.SERVER_KNOWN_HOSTS }}   # ssh-keyscan -t ed25519 203.0.113.10
      verify-url: https://app.example.com/up
    secrets:
      ssh-private-key: ${{ secrets.YOHO_GATE_KEY }}
```

Inputs:

| Input | Default | Meaning |
| --- | --- | --- |
| `working-directory` | required | Directory with the Yoho file. |
| `destination` | required | The Destination (`-d`). |
| `access` | `scoped` | `scoped` or `operator`. Sets `YOHO_ACCESS`. |
| `config` | empty | Yoho file for `-c`, for a CI-only file (see below). |
| `yoho-version` | `v0.2.0` | Release to install, or `latest`. |
| `runs-on` | `["ubuntu-latest"]` | JSON for `runs-on`, for example `["self-hosted","macOS","ARM64"]`. |
| `environment` | empty | GitHub environment, for approvals and protection rules. |
| `ssh-host` | empty | Host name or IP of the Server. Optional with `ssh.via: cloudflare` when `ssh-known-hosts` is set. |
| `ssh-alias` | empty | The alias in `servers.*.ssh` when it has no `user@host` form (see below). |
| `ssh-known-hosts` | empty | known_hosts line for the Server. The workflow rewrites its name column to `ssh-alias` (or `ssh-host`). Without it the workflow runs `ssh-keyscan` and trusts the first answer. |
| `install-cloudflared` | `false` | Install a pinned `cloudflared` (Linux or macOS, amd64 or arm64) and put it on `PATH`. Needed for `ssh.via: cloudflare`. |
| `cloudflared-version` | `2026.3.0` | cloudflared release to install. The sha256 values of `2026.3.0` are pinned in the workflow (from the release notes). |
| `cloudflared-sha256` | empty | sha256 of the asset for the runner (`cloudflared-linux-amd64`, `cloudflared-linux-arm64`, `cloudflared-darwin-amd64.tgz`, `cloudflared-darwin-arm64.tgz`). Required for any other `cloudflared-version`. |
| `verify-url` | empty | URL fetched with `curl -fsS`, retried for about two minutes. |

Secrets: `ssh-private-key` (required), and `cloudflare-service-token-id` and `cloudflare-service-token-secret` for a Destination with `ssh: {via: cloudflare}`. They are exported as `TUNNEL_SERVICE_TOKEN_ID` and `TUNNEL_SERVICE_TOKEN_SECRET` to the `yoho` steps only, and masked in the log.

Things to know:

- **The Gate host.** Yoho takes `servers.*.ssh`, replaces the user with `yoho-gate` (`gate_user` changes it) and keeps host and port, so `small-deb` becomes `yoho-gate@small-deb`. A runner has no `small-deb` in `~/.ssh/config`, so pass `ssh-alias: small-deb` and `ssh-host: 192.168.3.183`: the workflow adds a `Host small-deb` block (`HostName`, the key, `IdentitiesOnly yes`, and a per-run `known_hosts` file) at the top of the runner user's `~/.ssh/config`, and removes it afterwards. If `servers.*.ssh` is already a host name or IP, pass `ssh-host` only.
- **Builds run on the runner.** The Gate only receives finished images, so the runner needs Docker, and Yoho refuses `builder.location: server` or `remote` with scoped access. There is no environment override for the builder: put `builder.location: local` (and `builder.engine: docker` where Apple `container` would otherwise win) in the Yoho file, or keep an operator file and a second CI file and pass `config: yoho.ci.yml`. `builder.platforms` defaults to the Server's platform, so an arm64 runner cross-builds with buildx and QEMU.
- **Secrets.** Secret values stay on the Server (the operator pushed them). The runner never resolves `.yoho/secrets`.
- **Versions.** Yoho derives the Release version from git, so the workflow checks out the full history.
- **The workflow and the install action are pinned to one tag.** The reusable workflow calls `yoho-build/yoho/.github/actions/setup-yoho` at the same tag it was released with. You can use that action on its own to install Yoho in any job.
- **Secrets and environments.** A job that calls a reusable workflow cannot set `environment`, so `secrets.YOHO_GATE_KEY` and `vars.*` in the caller come from the repository or organization, not from an environment. The `environment` input protects the deploy job itself (required reviewers, branch rules).
- **Concurrency.** Deploys of one `working-directory` and `destination` queue; a running deploy is never cancelled.

#### GitHub-hosted runners for LAN-only Servers (Cloudflare Tunnel)

Put the Server behind a [Cloudflare Tunnel](#servers-without-a-public-ip-cloudflare-tunnel) and deploy from a plain `ubuntu-latest` runner (native amd64, no cross-build). Set `ssh: {via: cloudflare, hostname: ssh-deploy.example.com}` on the Destination (in the CI Yoho file too), create a Service Auth token, and store it as two secrets:

```yaml
jobs:
  deploy:
    uses: yoho-build/yoho/.github/workflows/deploy.yml@v0.2.0
    with:
      working-directory: .
      destination: production
      runs-on: '["ubuntu-latest"]'
      install-cloudflared: true
      ssh-alias: small-deb                              # servers.*.ssh
      ssh-known-hosts: ${{ vars.SMALL_DEB_KNOWN_HOSTS }} # ssh-keyscan -t ed25519 192.168.3.183
    secrets:
      ssh-private-key: ${{ secrets.YOHO_GATE_KEY }}
      cloudflare-service-token-id: ${{ secrets.CF_ACCESS_CLIENT_ID }}
      cloudflare-service-token-secret: ${{ secrets.CF_ACCESS_CLIENT_SECRET }}
```

Host key checking: Yoho adds `-o ProxyCommand=cloudflared access ssh --hostname …`, so ssh never resolves the Server name and looks the host key up under the name it was given (the alias, or `HostName` if set). The workflow writes `HostKeyAlias <ssh-alias>` into the `Host` block, omits `HostName` when `ssh-host` is empty, and rewrites the name column of `ssh-known-hosts` to the alias. You can paste the plain `ssh-keyscan` output of the LAN address. Hashed or `@cert-authority` lines are not supported. Do not set `ssh-host` here unless you want `HostName` for a direct (non-Cloudflare) connection.

#### Self-hosted runners for LAN-only Servers

GitHub-hosted runners cannot reach a Server on a private address such as `192.168.3.183`. Register a self-hosted runner on a machine that can (a Mac or a Linux box on the same network), give it Docker, and select it with `runs-on: '["self-hosted","macOS","ARM64"]'`. Use a runner that only serves repositories you trust, because a workflow that runs on it can use the machine and the key. Or use the Cloudflare Tunnel variant above with a hosted runner.

## Servers without a public IP (Cloudflare Tunnel)

A home Server reachable only through a [Cloudflare Tunnel](cloudflare-tunnel.md) serves both the operator path and the Gate. SSH goes through the tunnel to the host's sshd. Cloudflare Access then sits in front of the Gate as a second check, not as a replacement.

### Cloudflare side

1. Use a remotely managed tunnel, with `proxy.tunnel.token_secret` set. A Quick Tunnel cannot carry SSH.
2. In **Zero Trust > Networks > Tunnels > your tunnel > Public hostnames**, add `ssh.example.com` with service type **SSH** and URL `host.docker.internal:22`. The `yoho-tunnel` connector runs on the `yoho` bridge network with `--add-host host.docker.internal:host-gateway`, so that name points at the Server. sshd must listen on the bridge gateway address; the default `ListenAddress 0.0.0.0` does. If you use a firewall, allow port 22 from Docker's bridge networks.
   Connectors started by an older Yoho lack the host entry. Recreate them with `yoho tunnel down && yoho tunnel up`; this briefly interrupts the tunnel.
3. In **Zero Trust > Access > Applications**, add a self-hosted application for `ssh.example.com`:
   - Add an allow policy for your team (by email or identity provider group).
   - For CI, create **Service Auth > Service Tokens > Create** and add a **Service Auth** policy that allows that token.

### Yoho side

Point the Destination at the hostname. This applies to the operator and to Members:

```yaml
destinations:
  production:
    servers: [home]          # servers.home.ssh: deb@home (user and a known_hosts name)
    ssh: {via: cloudflare, hostname: ssh.example.com}
```

ssh then runs `cloudflared access ssh --hostname ssh.example.com` as its ProxyCommand. Install `cloudflared` on every machine that runs `yoho`; if it is missing, Yoho stops with a hint.

- People: the first connection opens a browser for the Cloudflare Access login.
- CI: export `TUNNEL_SERVICE_TOKEN_ID` and `TUNNEL_SERVICE_TOKEN_SECRET` from the service token. cloudflared reads them from the environment; Yoho never puts them in argv and never prints them.

## Limits today

- Only the compose runtime is supported, and only one Server per Destination.
- Builds run on the Member's machine with Docker. Images built with Apple `container` are not shipped through the Gate yet.
- App files (read-only bind mounts from the repository) are refused. Bake them into the image.
- Host ports in `limits.ports` are published past `ufw` by Docker, so list only ports you mean to open.
- A container's writable layer and its named volumes have no size limit (overlay2 cannot cap them without XFS project quotas), so a Member can fill the Server's disk. Images the Gate pulls anonymously have no size cap either and count toward the same disk. Watch disk usage, or give Docker its own filesystem.
- During a deploy the old and new containers run side by side, so an App can briefly use twice its limits.
- A Member who can deploy can read that App's own secrets from inside the container (see [Secrets](#secrets)).
- Short-lived certificates, `yoho login` and Access Tokens (the Access Service) come later. Until then, Members use restricted keys.
