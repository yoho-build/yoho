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
    replicas: 1
```

## Quick Tunnel (no account)

Omit `token_secret`:

```yaml
proxy:
  tunnel: {}
```

Yoho starts a Quick Tunnel with a random `*.trycloudflare.com` URL. Useful for demos; supports one replica and no stable hostname.

## Commands

```sh
yoho tunnel up       # start the connector
yoho tunnel status   # connector state, Quick Tunnel URL
yoho tunnel down
```

Run `yoho tunnel --help` for flags. Set `x-yoho.proxy.tls` off for tunneled Services.

## Alternative: your own cloudflared Service

See `examples/basic`: a `cloudflared` Service in compose joined to the `yoho` network, with `secrets: [TUNNEL_TOKEN]` and `secrets_as_env: true`.
