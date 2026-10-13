# Zero-downtime & proxy

Every Service with `x-yoho.proxy` sits behind one shared kamal-proxy per Server (`yoho-proxy` container on the `yoho` network).

## Plan before you deploy

`yoho plan` shows, read-only, which Services would be updated without downtime (proxied), replaced with a brief stop (non-proxied or Stateful), created or deleted, plus Proxy routes and the Proxy container itself. `yoho apply` shows the same plan, asks for `yes` (or `--auto-approve`), and runs the flow below. `yoho deploy` is `apply --auto-approve`.

## Deploy flow

1. Hooks `pre-connect`, `pre-build`, `pre-deploy`.
2. Build and ship images, write secrets, compile the compose file per Server.
3. Start and readiness-gate release-command prerequisites, then run `release_command` in a one-off container from the new image. Failure aborts.
4. Start and readiness-gate Services in `depends_on` order: non-Proxy Services converge stop-first; proxied Services stage new containers next to old ones (`--scale <svc>=2 --no-recreate`). Thus a worker can depend on a new proxied API that depends on a database. No Proxy traffic switches until every Service is ready; failure removes unswitched Proxy candidates.
5. kamal-proxy health-checks the new container (`health_path`, up to `deploy_timeout`), switches traffic, and drains the old one (`drain_timeout`).
6. Remove the old container, stale Proxy routes of Services that are no longer proxied, and local images of the App that no retained Release or container uses; hook `post-deploy`.

Stateful Services and other non-Proxy Services (including workers) are recreated stop-first, never automatically overlapped. Volumes are never removed.

Compose readiness requires every replica's enabled Docker healthcheck to be healthy, including an inherited image `HEALTHCHECK`. Exit, unhealthy state, or a restart observed while waiting fails the deploy before Proxy cutover. Without a healthcheck, Yoho explicitly reports the weaker guarantee: the process is running, not necessarily application-ready. Zero replicas skip readiness. A Service required by an active consumer's `depends_on` with `condition: service_completed_successfully` is a one-shot job: Yoho waits for exit status zero instead of requiring it to keep running; `restart: "no"` alone does not identify a job. A scaled-zero consumer does not impose a completion contract; an active consumer requiring completion from a scaled-zero job is rejected before startup. Absent `required: false` profile dependencies are omitted, while missing required dependencies fail preflight. Compose-run one-off containers are excluded from readiness and candidate counts and are never removed by this cutover cleanup.

Non-Proxy Services and release-command prerequisites have a 30-second minimum readiness deadline, extended to cover the effective healthcheck's `start_period` plus `(retries + 1) × (max(interval, start_interval) + timeout)` (Docker defaults apply when unset). A job without a healthcheck has the 30-second deadline; longer migrations should use `release_command`. Proxy Services retain their configured `deploy_timeout`. These gates apply on rollback too; old Release plans recover job classification from their saved Compose file, not today's configuration. A failed plain gate does not undo stop-first replacements already made; it prevents the subsequent Proxy switch, not all App downtime.

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

Each Release records the image IDs it ran. Image tags can move (the same Version built for another Destination on the same Server, or a rebuild), so rollback points a Yoho-built tag back at the recorded image before the cutover, and fails if that image is gone. Third-party tags such as `postgres:17` are not retagged; a moved one is a warning. Rollback also removes the routes of Services that the Release does not proxy.

Retention keeps the newest `retain_releases` successful Releases, the current one, and the newest failed record, so failed deploys never push a rollback target out. If the route has switched but the old containers cannot be stopped, the deploy still succeeds and logs a warning with the `docker rm -f` to run. Changing only `x-yoho.release_command` is a plan change: the next deploy runs it even though no container is replaced. Yoho-built image tags include the Destination (`yoho/<app>-<destination>-<service>:<version>`).
