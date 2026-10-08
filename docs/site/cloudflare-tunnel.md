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

## Alternative: your own cloudflared Service

See `examples/basic`: a `cloudflared` Service in compose joined to the `yoho` network, with `secrets: [TUNNEL_TOKEN]` and `secrets_as_env: true`.
