# Cloudflare Tunnel

Yoho can run cloudflared for you as the `yoho-tunnel` container, pointed at the shared Proxy. Requests keep flowing during deploys because cloudflared targets the Proxy, which health-gates and drains; cloudflared itself does not retry failed origin requests.

## Remotely managed tunnel

Create a tunnel in the Cloudflare dashboard with a public hostname (its service can stay where it is for now), then:

```yaml
# yoho.yml
proxy:
  http_port: 0
  https_port: 0
  tunnel:
    token_secret: TUNNEL_TOKEN   # a secret key from .yoho/secrets or a provider
    replicas: 1                  # connectors per Server (default 1)
    # image: cloudflare/cloudflared:<tag>   # optional; pinned by default
    api_token_secret: CF_API_TOKEN   # optional: lets `yoho tunnel route` change ingress rules
```

Each public hostname must point at `http://yoho-proxy:80`, so the Proxy health-gates every deploy. Switch it with [`yoho tunnel route`](#switching-a-hostname-to-the-proxy) after the first deploy, or set the service in the dashboard yourself.

## Quick Tunnel (no account)

Omit `token_secret`:

```yaml
proxy:
  tunnel: {}
```

Yoho starts a Quick Tunnel with a random `*.trycloudflare.com` URL, shown by `yoho tunnel up` and `yoho tunnel status`. Useful for demos; supports one replica, and the URL changes when the connector is recreated. Requests arrive with the `*.trycloudflare.com` Host, so leave `x-yoho.proxy.hosts` empty (catch-all); `yoho config check` warns otherwise.

## Commands

```sh
yoho tunnel up       # start or update the connector(s) on each Server of the Destination
yoho tunnel status   # connector state, connections, mode, Quick Tunnel URL
yoho tunnel down     # remove the connector(s) and the token file
yoho tunnel route --hostname app.example.com --to proxy   # point one ingress rule at the Proxy
```

`up`, `status` and `down` take no flags besides the global ones. `yoho apply` and `yoho deploy` also converge the Tunnel, and `yoho plan` shows it. `yoho setup` opens no Proxy ports in the firewall when a Tunnel is configured. Set `x-yoho.proxy.tls` off for tunneled Services. Customer domains that should terminate TLS at Cloudflare use Cloudflare for SaaS, not `custom_domains`; see [Customer-owned domains](custom-domains.md).

## Switching a hostname to the Proxy

`yoho tunnel route` changes the service of one ingress rule of the remotely managed tunnel through the Cloudflare API, with a check before and after and an automatic undo:

```sh
yoho tunnel route -d production --hostname app.example.com --to proxy
yoho tunnel route -d production --hostname app.example.com --to proxy --dry-run
```

It needs `proxy.tunnel.token_secret` (which names the account and tunnel) and a Cloudflare API token with Account > Cloudflare Tunnel > Edit in `proxy.tunnel.api_token_secret`, or in `destinations.<name>.migrate.tunnel.api_token_secret` when a [migration cutover](configuration.md#migration-cutover-migrate) already set one. The token is resolved on your machine, sent only in the `Authorization` header to `api.cloudflare.com` and redacted from errors. Operator access only. The command takes the Destination lock (`--lock-wait` waits for it), except with `--dry-run`.

1. **Read.** Yoho reads the tunnel's configuration and selects the one rule for the hostname. When the hostname has several rules (for example `/ws` to another Service), `--path` selects one by its path, exactly as the rule writes it. Without `--path`, the rule without a path is selected, and Yoho warns about the others, which keep their service. Wildcard hostnames are refused. A rule that already points at the Proxy (`http://yoho-proxy` and `http://yoho-proxy:80` are the same) ends the command without a change.
2. **Check before.** With `--to proxy`, before anything changes, the Proxy on each Server must have this App Destination's route for the hostname, and that route must answer its health path (the Service's `x-yoho.proxy.health_path` in the current Release, default `/up`) with 2xx. The request is sent the way cloudflared will send it: from the Docker network that the connector (`yoho-tunnel`, or a `cloudflared` Service of this App) shares with `yoho-proxy`, with `Host: <hostname>` and `X-Forwarded-Proto: https`. Yoho opens the connection through SSH (`ssh -W`), so nothing has to be installed on the Server or in the cloudflared image (it has no shell or curl). The Server's sshd must allow TCP forwarding. On runtime swarm, overlay networks cannot be reached from the Server itself, so this check fails there.
3. **Check the rule serves the check path.** When the check after the change runs (below), the selected rule must be the first rule Cloudflare uses for `https://<hostname><check path>`: rules are tried in order, a rule's hostname matches exactly or by `*.` wildcard, and its `path` is a regular expression matched anywhere in the request path (`/api` also matches `/v1/api`; anchor it as `^/api`). If another rule (for example the hostname's rule without a path, for a `--path` rule and the default `/up`) would answer, the check would not test the switched rule, so Yoho stops and asks for a `--check-path` the rule serves.
4. **Show the undo.** Yoho prints the old rule and the command that puts it back, for example `yoho tunnel route -d production --hostname app.example.com --to http://web:3000 --check-path /up` (also a `tunnel_route` event with `--json`). It saves both in `<app dir>/tunnel-route.json` on the Server. `--dry-run` stops here.
5. **Switch.** Holding the lock, Yoho reads the configuration again; when the selected rule changed since step 1, it stops without changing anything. Otherwise it changes only the `service` of that rule in the configuration as it is now. Every other rule (including edits others made meanwhile), the rule order, the catch-all and the rule's other settings (`originRequest`, …) are kept. It reads the configuration back to confirm the change.
6. **Check after.** From your machine, Yoho requests `https://<hostname><check path>` without following redirects until it answers 2xx (for `--to http_status:NNN`: exactly NNN) or `--timeout` (default 60s) passes; Cloudflare takes a few seconds to reach the connectors. Targets Yoho cannot request this way (`hello_world`, `unix:`, `tcp://`, `ssh://`, …) are not checked: Yoho says so (a `not_checked` event with `--json`) and never undoes them. A hostname behind Cloudflare Access redirects to its login page; Yoho reports that and undoes the switch, so add an Access bypass policy for the health path first.
7. **Undo.** When the switch or the check fails, or you press Ctrl-C after the switch, Yoho reads the live configuration again and puts the old service back on that one rule, so changes others made to other rules meanwhile are kept. If the rule no longer has the service Yoho set, someone else changed it: Yoho leaves it alone and says so. The error names the undo outcome.

`--check-path` sets the path both checks request (default: the route's health path, or `/` for an origin that is not the Proxy), for example for a `--path` rule whose path does not match `/up`. `--to` also takes an origin such as `http://web:3000` or any other ingress service (`http_status:404`, `hello_world`, `unix:…`, `tcp://…`), which is how the printed undo command puts an old rule back; there is no check before the change then.

`yoho rollback` warns, without failing or blocking, when a hostname of the target Release still has an ingress rule that does not point at the Proxy, because the rollback then does not change what that hostname serves. The check runs only when `token_secret` and an API token secret are configured, takes at most 10 seconds, and is skipped when the secrets or the Cloudflare API do not answer in time.

## SSH through the tunnel

A remotely managed tunnel can also carry SSH to the Server, for Servers without a public IP. Add a public hostname with service `ssh://host.docker.internal:22` (token connectors map `host.docker.internal` to the Server), protect it with a Cloudflare Access application, and set `ssh: {via: cloudflare, hostname: ssh.example.com}` on the Destination. See [Sharing one App with a team](team-access.md#servers-without-a-public-ip-cloudflare-tunnel).

## Alternative: your own cloudflared Service

See `examples/basic`: a `cloudflared` Service in compose joined to the `yoho` network, with `secrets: [TUNNEL_TOKEN]` and `secrets_as_env: true`.

This also works with scoped access (`access: scoped`). There the `yoho` network becomes the App's own Proxy network, `yoho-<app>-<destination>-proxy`, so the connector reaches `http://yoho-proxy:80` but no other App. Yoho creates that network and connects the Proxy to it even when no Service of the App is proxied.

Do not send deploy SSH through that same Service. A Destination with `ssh: {via: cloudflare, hostname: …}` whose hostname is routed by a `cloudflared` in this App will drop the session (exit 255) when the rollout restarts the connector. Scoped (Gate) rollouts still finish on the Server and record the real Release status; `yoho deploy` reconnects, or `yoho releases` shows the outcome. An operator deploy is not detached and can stop mid-rollout. `yoho config check` and `yoho plan` warn. Prefer a dedicated SSH tunnel outside the App (the managed `yoho-tunnel` connector, or a cloudflared that is not a Service of this App).
