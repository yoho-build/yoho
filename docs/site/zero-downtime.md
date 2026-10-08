# Zero-downtime & proxy

Every Service with `x-yoho.proxy` sits behind one shared kamal-proxy per Server (`yoho-proxy` container on the `yoho` network).

## Deploy flow

1. Hooks `pre-connect`, `pre-build`, `pre-deploy`.
2. Build and ship images, write secrets, compile the compose file per Server.
3. Run `release_command` in a one-off container from the new image. Failure aborts.
4. Start the new container next to the old one (`--scale <svc>=2 --no-recreate`).
5. kamal-proxy health-checks the new container (`health_path`, up to `deploy_timeout`), switches traffic, and drains the old one (`drain_timeout`).
6. Remove the old container; hook `post-deploy`.

Stateful Services are recreated stop-first, never scaled. Volumes are never removed.

## Ports

Defaults are 80/443. If the Server already uses them: `proxy.http_port: 8080`, `proxy.https_port: 0`. Behind Cloudflare Tunnel, no published ports and no TLS are needed; see [Cloudflare Tunnel](cloudflare-tunnel.md).

## Commands

```sh
yoho proxy boot     # start or reconfigure the Proxy
yoho proxy status   # container and routes
yoho proxy logs
```

## Rollback

`yoho releases` lists Releases (image digests and compiled compose). `yoho rollback VERSION` redeploys one with the same zero-downtime cutover. Volumes and migrations are not reverted. The last `retain_releases` (5) are kept.
