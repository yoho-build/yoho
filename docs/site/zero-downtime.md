# Zero-downtime & proxy

Every Service with `x-yoho.proxy` sits behind one shared kamal-proxy per Server (`yoho-proxy` container on the `yoho` network).

## Plan before you deploy

`yoho plan` shows, read-only, which Services would be updated without downtime (proxied), replaced with a brief stop (non-proxied or Stateful), created or deleted, plus Proxy routes and the Proxy container itself. `yoho apply` shows the same plan, asks for `yes` (or `--auto-approve`), and runs the flow below. `yoho deploy` is `apply --auto-approve`.

## Deploy flow

1. Hooks `pre-connect`, `pre-build`, `pre-deploy`.
2. Build and ship images, write secrets, compile the compose file per Server.
3. Run `release_command` in a one-off container from the new image. Failure aborts.
4. Start the new container next to the old one (`--scale <svc>=2 --no-recreate`).
5. kamal-proxy health-checks the new container (`health_path`, up to `deploy_timeout`), switches traffic, and drains the old one (`drain_timeout`).
6. Remove the old container, stale Proxy routes of Services that are no longer proxied, and local images of the App that no retained Release or container uses; hook `post-deploy`.

Stateful Services are recreated stop-first, never scaled. Volumes are never removed.

On Docker 25 or newer, Yoho compiles `start_period: 60s` and `start_interval: 1s` into a proxied Service's healthcheck when neither is set, so the first probes run every second while the container starts.

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
