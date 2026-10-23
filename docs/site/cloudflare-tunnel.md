# Cloudflare Tunnel

Yoho can run cloudflared for you as the `yoho-tunnel` container, pointed at the shared Proxy. Requests keep flowing during deploys because cloudflared targets the Proxy, which health-gates and drains; cloudflared itself does not retry failed origin requests.

## Remotely managed tunnel

Create a tunnel in the Cloudflare dashboard with a public hostname whose service is `http://yoho-proxy:80`, then:

```yaml
# yoho.yml
proxy:
  http_port: 0
  https_port: 0
  tunnel:
    token_secret: TUNNEL_TOKEN   # a secret key from .yoho/secrets or a provider
    replicas: 1                  # connectors per Server (default 1)
    # image: cloudflare/cloudflared:<tag>   # optional; pinned by default
```

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
```

These commands take no flags besides the global ones. `yoho apply` and `yoho deploy` also converge the Tunnel, and `yoho plan` shows it. `yoho setup` opens no Proxy ports in the firewall when a Tunnel is configured. Set `x-yoho.proxy.tls` off for tunneled Services.

## SSH through the tunnel

A remotely managed tunnel can also carry SSH to the Server, for Servers without a public IP. Add a public hostname with service `ssh://host.docker.internal:22` (token connectors map `host.docker.internal` to the Server), protect it with a Cloudflare Access application, and set `ssh: {via: cloudflare, hostname: ssh.example.com}` on the Destination. See [Sharing one App with a team](team-access.md#servers-without-a-public-ip-cloudflare-tunnel).

## Alternative: your own cloudflared Service

See `examples/basic`: a `cloudflared` Service in compose joined to the `yoho` network, with `secrets: [TUNNEL_TOKEN]` and `secrets_as_env: true`.

Do not send deploy SSH through that same Service. A Destination with `ssh: {via: cloudflare, hostname: …}` whose hostname is routed by a `cloudflared` in this App will drop the session (exit 255) when the rollout restarts the connector. Scoped (Gate) rollouts still finish on the Server and record the real Release status; `yoho deploy` reconnects, or `yoho releases` shows the outcome. An operator deploy is not detached and can stop mid-rollout. `yoho config check` and `yoho plan` warn. Prefer a dedicated SSH tunnel outside the App (the managed `yoho-tunnel` connector, or a cloudflared that is not a Service of this App).
