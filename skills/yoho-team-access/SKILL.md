---
name: yoho-team-access
description: Share one Yoho App Destination on a shared Server with teammates and CI through the Gate (scoped access, ADR 0011), without giving them shell, Docker or other Apps. Use when the user wants a teammate or GitHub Actions / GitLab CI to deploy, roll back, read logs or open `yoho ui` for one App, asks about `yoho access`, `access: scoped`, `yoho-gate`, Member keys, the Gate refusing a compose file, or reaching a home Server without a public IP through Cloudflare Tunnel.
---

# Sharing one App with a team

Playbook for scoped access. It assumes the `yoho` skill for general commands; `docs/site/team-access.md` (yohobuild.com/team-access) is the full reference, including the policy list and the `limits` table.

Terms: the **operator** has full SSH to the Server. A **Member** is a teammate or CI job with a restricted key for the `yoho-gate` Unix user. The **Gate** (`yoho gate`) is the forced command that serves fixed verbs for one App Destination: `deploy`, `rollback`, `releases`, `ps`, `logs`, `stats`, `dash`. There is no `exec` and no verb that changes access.

Ground rules:

- Ask before every change to the Server, CI secrets or Cloudflare.
- Never print, echo or paste private keys, secret values or Cloudflare service token secrets into the chat, argv or logs. Pass them through files or stdin (`gh secret set NAME < file`).
- Do not weaken the Gate to make a deploy pass. When it refuses a compose file, change the compose file or ask the operator to raise a limit.
- Tell the user plainly: granting `deploy` lets the Member ship code that runs with that App's secrets. Other Apps stay out of reach.

## 1. Which side is the user on?

- Has full SSH to the Server and wants to share → **Operator** (section 2).
- Was given access, or is wiring CI → **Member** (section 3).
- The Server has no public IP → also section 4.

Check with `yoho access ls -d DEST` (operator path) or `YOHO_ACCESS=scoped yoho releases -d DEST` (Member path).

## 2. Operator: set up once

Needs root or passwordless sudo on the Server, and a Yoho binary from a release (`make dist` for a dev build). From the App's repository:

```sh
yoho deploy -d production                  # the App Destination must exist
yoho access install -d production          # yoho-gate user, sudoers, /usr/local/bin/yoho, /etc/yoho/gate.json
yoho access secrets push -d production     # secrets + Proxy/Registry settings for scoped deploys
yoho access add alice --verbs all -d production        # or read, or deploy,rollback,releases
yoho access key add alice alice.pub -d production
yoho access ls -d production
```

- After `yoho secrets rotate` (or adding a secret or Proxy hostname), rerun `access secrets push` so Members pick up the new generation.
- Rerun `access install` after upgrading Yoho; the Gate and Member CLIs must speak the same protocol.
- Limits (ports, memory, CPU, replicas, upload size, registries pulled with the Server's login) live in `<root>/apps/<app>/<dest>/access/settings.json` under `limits`; edit as root. `secrets push` keeps them.
- Revoke: `yoho access rm alice` (verbs) and `yoho access key rm alice` (keys); effective on the next connection.
- Audit: `yoho access audit -n 50 -d production`.

## 3. Member: daily use

Set `access: scoped` on the Destination in the shared Yoho file, or `YOHO_ACCESS=scoped` just for this shell. The CLI connects as `yoho-gate@<host>` with the Member's own key.

```sh
yoho deploy -d production
yoho releases -d production
yoho rollback <version> -d production     # only Releases the Gate deployed
yoho app logs web -n 200 -d production
yoho app ps -d production; yoho app stats --no-stream -d production
yoho ui -d production                     # or yoho tui
```

`setup`, `plan`/`apply`, `proxy`, `tunnel`, `backup`, `schedule`, `app exec` and `secrets` are operator-only and fail before connecting.

Builds run on the Member's machine with Docker (`builder.location: local`, `builder.engine: docker` on Macs where Apple `container` is the default).

### CI (GitHub Actions)

```sh
ssh-keygen -t ed25519 -N '' -C ci-github -f ci-github
yoho access add ci-github --verbs deploy,releases,rollback -d production   # operator
yoho access key add ci-github ci-github.pub -d production                  # operator
gh secret set YOHO_GATE_KEY -R OWNER/REPO < ci-github                      # value on stdin
rm ci-github ci-github.pub
```

Use the reusable workflow rather than hand-writing the steps. It installs a checksum- and attestation-verified Yoho, writes the key to a temp file (removed at the end), pins the host key, runs `yoho releases`, `yoho deploy`, `yoho releases`, optionally curls `verify-url`, and on failure prints the previous version and the `yoho rollback` command in the job summary.

```yaml
jobs:
  deploy:
    uses: yoho-build/yoho/.github/workflows/deploy.yml@v0.2.0
    with:
      working-directory: .
      destination: production
      ssh-host: 203.0.113.10
      ssh-known-hosts: ${{ vars.SERVER_KNOWN_HOSTS }}   # from: ssh-keyscan -t ed25519 HOST
      verify-url: https://app.example.com/up
    secrets:
      ssh-private-key: ${{ secrets.YOHO_GATE_KEY }}
```

Other inputs: `access` (`scoped` default, `operator`), `config` (`-c` file), `yoho-version`, `runs-on` (JSON), `environment`, `ssh-alias`. Cloudflare secrets: `cloudflare-service-token-id` and `-secret`.

Checks before you wire it:
- Yoho connects as `yoho-gate@<host of servers.*.ssh>`. If `servers.*.ssh` is an ssh config alias (`small-deb`), set `ssh-alias` and `ssh-host` so the runner gets a `Host` block for it.
- The runner builds the images, so it needs Docker, and `builder.location` must be `local` (scoped access refuses `server` and `remote`; there is no env override). Keep an operator Yoho file and a CI file (`config: yoho.ci.yml`) if the operator builds on the Server.
- `secrets.*` and `vars.*` in the caller are repository or organization level; a calling job cannot use an environment. The `environment` input gates the deploy job.
- LAN-only Server on a hosted runner: Cloudflare Tunnel (section 4) plus `install-cloudflared: true`, `ssh-alias` (no `ssh-host`), `ssh-known-hosts` (keyscan of the LAN address; the workflow rewrites the name to the alias and sets `HostKeyAlias`), and secrets `cloudflare-service-token-id` / `-secret`. They reach only the `yoho` steps.
- Otherwise a LAN-only Server needs a self-hosted runner on that network (`runs-on: '["self-hosted","macOS","ARM64"]'`).

GitLab: `glab variable set YOHO_GATE_KEY -R GROUP/PROJECT --masked --protected < ci-github`, then write the key to a file, add the host to `known_hosts`, install Yoho, and run `YOHO_ACCESS=scoped yoho deploy -d production`.

## 4. Server without a public IP (Cloudflare Tunnel)

Requires a managed tunnel (`proxy.tunnel.token_secret`); Quick Tunnels cannot carry SSH.

1. Cloudflare tunnel public hostname `ssh.example.com` → service **SSH**, URL `host.docker.internal:22`. Connectors from older Yoho versions need `yoho tunnel down && yoho tunnel up` to get the host entry.
2. Cloudflare Access self-hosted application for `ssh.example.com`: an allow policy for the team, and a **Service Auth** policy with a service token for CI.
3. Yoho file: `destinations.production.ssh: {via: cloudflare, hostname: ssh.example.com}`. Every machine running `yoho` needs `cloudflared`.
4. CI exports `TUNNEL_SERVICE_TOKEN_ID` and `TUNNEL_SERVICE_TOKEN_SECRET` from CI secrets.

## 5. When the Gate refuses

The error lists every reason. Common ones and the fix:

| Refusal | Fix |
|---|---|
| bind mount, `privileged`, `cap_add`, `devices`, host namespaces | Remove it; bake files into the image; use named volumes |
| `network_mode: bridge` / external network / `yoho` network | Leave `network_mode` out; Yoho attaches proxied Services to the Proxy |
| published host port | Operator adds it to `limits.ports`, or route through the Proxy |
| image cannot be pulled anonymously | Make it public, or operator adds the registry prefix to `limits.registries` |
| memory, CPU, replicas, pids above limits | Lower them, or ask the operator to raise `limits` |
| secret not stored for scoped deploys | Operator runs `yoho access secrets push` |
| rollback to a Release not deployed through the Gate | Ask the operator, or deploy that version again |
| locked by the operator or a backup | Wait; Members cannot clear operator locks |
| permission denied for a verb | Operator runs `yoho access add MEMBER --verbs …` |

## Not yet

Short-lived certificates, `yoho login` device codes, `yoho_token_` Access Tokens and GitHub OIDC (the Access Service) are planned, not shipped. Use restricted keys today.
