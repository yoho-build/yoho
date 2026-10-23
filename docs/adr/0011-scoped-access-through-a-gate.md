# Scoped access through a Gate

## Context

One Server often runs many Apps for one operator. The operator wants to share a single App on it with a team: teammates on their laptops and the team's CI (GitHub Actions, GitLab CI) should deploy, roll back, read logs, see status and open `yoho ui` for that App, and nothing else on the Server.

The operator path cannot be narrowed. The CLI sends arbitrary POSIX sh to `ssh … sh -s` (`internal/remote/ssh.go`), and the deploy user is in the `docker` group, which is root on the host (`internal/setup/setup.go`). No allowlist, regex or parser can make arbitrary scripts safe. A compose file is root too: whoever can submit one can mount `/var/run/docker.sock`, `/` or another App's secrets directory, or ask for `privileged`.

Prior art keeps the shell closed and exposes fixed commands: gitolite and Dokku run a dispatcher from an `authorized_keys` `command=` line, and dokku-acl adds per-App command lists. Dokku still lets any user pass `docker-options`, so its ACL does not stop an escape. Short-lived SSH certificates (step-ca, Vault, Teleport Machine ID, oidc-ssh-ca) replace long-lived CI keys, with the forced command written into the certificate.

## Decision

1. **A second, scoped path next to the operator path.** The operator keeps full SSH and every command works as today. A **Member** reaches one App through the **Gate**: `yoho gate`, run by sshd per connection from a forced command, reading one request, exiting. Nothing stays running, so ADR 0003 holds for this part.

2. **Fixed verbs, structured requests.** The Gate accepts `hello`, `ps`, `stats`, `logs`, `dash`, `releases`, `deploy` and `rollback` as one JSON request on stdin, never a shell line. `exec` is a later, opt-in verb. Access management is never a Gate verb: Members cannot grant themselves anything. The client keeps the same subcommands; a Destination marked `access: scoped`, or a successful `hello`, makes `internal/remote` use the Gate instead of `sh -s`.

3. **Identity and OS boundary.** Members log in as one `yoho-gate` Unix user that is not in the `docker` group. Each key or certificate maps to one Member, gitolite style (`restrict,command="yoho gate --member NAME"`). sudoers lets `yoho-gate` run only the root-owned `yoho gate` binary as root.

4. **The Gate checks the payload, not only the caller.** `deploy` carries a `plan.Deploy`, the existing contract between the CLI and the runtimes. The Gate refuses projects that would break out of the App: `privileged`, host `network_mode`/`pid`/`ipc`/`cgroupns`/`userns_mode`, `cap_add`, `devices`, unconfined `security_opt`, `volumes_from`, the Docker socket, every bind mount, external volumes and networks, Yoho-built images (`yoho/…`, the Registry prefix) it did not build in this deploy, host ports the operator did not allow, any `network_mode` but `none` (`bridge` is Docker's shared bridge), and shm, tmpfs, ulimit, pids, memory, CPU and replica values above the operator's limits (defaults are applied where the project sets none; disk is not capped). The Gate pulls every image it did not build itself, anonymously unless the operator allowed the registry, and runs compose with `pull_policy: never`, so the Server's registry login never serves a Member. Names are fixed to the App's compose project and directories. The Gate also checks the Server's Docker state: a name another App Destination shares (`shop/prod-web` and `shop-prod/web` are both `yoho-shop-prod-web`), an existing volume, network or image tag that belongs to someone else, or a local image that was not pulled from its registry is refused. Images arrive either way the CLI ships them today: pulled from a registry, or streamed on the `deploy` request's stdin as a strictly checked archive (regular files and directories under canonical names only, every tag built by this deploy, bounded size and time).

   **A scoped App has its own network.** Its proxied Services join `yoho-<app>-<destination>-proxy` instead of the shared `yoho` network, the runtime connects the Proxy to it, and a recreated Proxy rejoins every such network. A Member's containers therefore reach their own App and the Proxy (the App's public routes, as from the internet), not other Apps' containers. The Gate drops `NET_RAW` and sets a default pids limit on every container.

5. **Secrets stay on the Server for scoped Apps.** This amends ADR 0006 for scoped Apps only. The operator resolves secrets locally and pushes them (`yoho access secrets push`); the push names its generation in `<app dir>/access-secrets`, and every scoped deploy reads that generation, never the newest one (each deploy writes its own, with only what it declared), until the next push. Retention keeps it. Members and CI never hold or see the values through Yoho, and there is no `secrets reveal` verb. This does not keep secrets from a Member's code: the containers a Member deploys run with the App's secrets, so their code can read and send them anywhere. Granting `deploy` means trusting the Member with that App's secrets; it does not extend to other Apps.

6. **State lives under the App directory.** `<root>/apps/<app>/<destination>/access/` holds `members.yml` (Member → verbs), `tokens.yml` (Access Token hashes), `oidc.yml` (allowed repositories, refs, environments) and `audit.log`, one JSON line per request: time, Member, key or certificate ID, verb, redacted arguments, exit code. The directory is root-owned and unwritable by `yoho-gate`. The operator edits it with `yoho access …` over the operator path.

7. **Short-lived credentials come from an opt-in Access Service.** `yoho access serve` is the first resident, public Yoho component and stays off by default. It runs as a container behind the Proxy, has no Docker socket, and only signs SSH certificates whose principal is the Member and whose `force-command` is the Gate: 12 hours for people, 10 minutes for CI. It accepts:
   - a device login: `yoho login` shows a `yoho_device_…` code, the Member's allowlisted email receives a one-time code through Resend or SMTP, and entering it gets a certificate;
   - an **Access Token** `yoho_token_…` from CI, stored only as a hash;
   - a GitHub Actions OIDC token matching `oidc.yml`, so CI needs no stored secret.

   Until it is enabled, Members and CI use restricted keys added with `yoho access key add`. The operator path never depends on the Access Service, so a broken or compromised Access Service can only mint Gate certificates.

8. **Servers without a public IP.** A home Server reachable only through `yoho tunnel` (a managed Cloudflare Tunnel in front of the Proxy) still serves both paths. The Access Service is just another hostname on the tunnel, routed by the Proxy like an App; a Quick Tunnel's changing URL is refused for it. Gate SSH goes through a tunnel ingress `ssh://` rule to the host's sshd, and the client reaches it with `cloudflared access ssh` as ProxyCommand: a Destination sets `ssh: {via: cloudflare, hostname: ssh.example.com}`, people log in through Cloudflare Access in the browser, and CI uses a Cloudflare Access service token from `TUNNEL_SERVICE_TOKEN_ID` and `TUNNEL_SERVICE_TOKEN_SECRET`. Cloudflare Access is then a second gate in front of the Gate, not a replacement for it.

9. **Token format.** `yoho_token_` and `yoho_device_`, then 32 random bytes in base62, then a 6-character CRC32 checksum, like GitHub tokens: secret scanners and the redactor match the prefix, and the CLI rejects typos without a round trip. Plaintext is shown once. `yoho access token create --push github:OWNER/REPO` writes it to CI through `gh secret set` or `glab variable set --masked --protected`, passing the value on stdin, never in argv.

## Consequences

- The Gate is a second runtime entry: every verb needs a Gate implementation and tests through `remote.Local`.
- Scoped deploys cannot run Hooks or a `release_command` that the policy would refuse; the Gate checks those images and commands like Services.
- `yoho ui` and `yoho tui` work for Members unchanged, because ADR 0010 already collects through one script that becomes the `dash` verb.
- The policy is a list the Gate must keep up with compose; an unknown escape vector is a Gate bug, not a misconfiguration.
- The Access Service runs on the same Server as the Apps it guards. When that Server is down, the operator path still works and Members wait.
- Revocation: removing a Member or key takes effect on the next connection; certificates expire on their own, and sshd `RevokedKeys` covers the urgent case.
- `rollback` re-checks the Release inside the deploy lock: only Releases the Gate deployed (it records the Member's compose and the compiled file's hash), unchanged since, and passing today's policy. Releases the operator deployed can do anything the operator can, so Members cannot roll back to them.
- A deploy whose client disconnects runs to completion and releases the lock. After images are on the Server, the Gate starts the rollout in a new session (`setsid`) and records progress under `<access>/rollout/`; the SSH process tails that log. A dropped session (exit 255) does not mark the Release `failed`. `yoho deploy` reconnects, or `yoho releases` shows the outcome. A Gate that dies holding the lock (killed, or the Server rebooted) left its pid and boot id in it; the next Gate run clears it. Other stale locks still need the operator.
- The Yoho root comes from the root-owned `/etc/yoho/gate.json`, not from the forced command, and sudo resets the environment (`env_reset`, `secure_path`) for the Gate user; verbs run with a fixed minimal environment.
- Requests that name no App Destination with scoped access are audited in `<root>/gate-audit.log`; audit logs rotate at 16 MiB.
