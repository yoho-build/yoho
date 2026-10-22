# Zero-downtime & proxy

Every Service with `x-yoho.proxy` sits behind one shared kamal-proxy per Server (`yoho-proxy` container on the `yoho` network).

## Plan before you deploy

`yoho plan` shows, read-only, which Services would be updated without downtime (proxied), replaced with a brief stop (non-proxied or Stateful), created or deleted, plus Proxy routes and the Proxy container itself. `yoho apply` shows the same plan, asks for `yes` (or `--auto-approve`), and runs the flow below. `yoho deploy` is `apply --auto-approve`.

## Deploy flow

1. Hooks `pre-connect`, `pre-build`, `pre-deploy`.
2. Build and ship images, write secrets, compile the compose file per Server.
3. Start and readiness-gate release-command prerequisites, then run `release_command` in a one-off container from the new image. Failure aborts.
4. Start and readiness-gate Services by `depends_on` level: the non-Proxy Services of a level start with one `docker compose up` and converge stop-first, and Yoho waits for them concurrently; the next level starts only when the whole level is ready; proxied Services stage new containers next to old ones (`--scale <svc>=2 --no-recreate`). Thus a worker can depend on a new proxied API that depends on a database. An existing route switches only when every Service is ready; failure removes unswitched Proxy candidates. On a first deploy (no route yet, no old containers), a proxied Service's route goes live as soon as that Service is healthy, since nothing else serves it; if a later Service fails, those routes stay live and the failure names them.
5. kamal-proxy health-checks the new container (`health_path`, up to `deploy_timeout`), switches traffic, and drains the old one (`drain_timeout`).
6. Remove the old container, stale Proxy routes of Services that are no longer proxied, and local images of the App that no retained Release or container uses; hook `post-deploy`.

Stateful Services and other non-Proxy Services (including workers) are recreated stop-first, never automatically overlapped. Volumes are never removed.

Compose readiness requires every replica's enabled Docker healthcheck to be healthy, including an inherited image `HEALTHCHECK`. Exit, unhealthy state, or a restart observed while waiting fails the deploy before Proxy cutover. Without a healthcheck, Yoho explicitly reports the weaker guarantee: the process is running, not necessarily application-ready. Zero replicas skip readiness. A Service required by an active consumer's `depends_on` with `condition: service_completed_successfully` is a one-shot job: Yoho waits for exit status zero instead of requiring it to keep running; `restart: "no"` alone does not identify a job. A scaled-zero consumer does not impose a completion contract; an active consumer requiring completion from a scaled-zero job is rejected before startup. Absent `required: false` profile dependencies are omitted, while missing required dependencies fail preflight. Compose-run one-off containers are excluded from readiness and candidate counts and are never removed by this cutover cleanup.

Non-Proxy Services and release-command prerequisites have a 30-second minimum readiness deadline, extended to cover the effective healthcheck's `start_period` plus `(retries + 1) × (max(interval, start_interval) + timeout)` (Docker defaults apply when unset). A job without a healthcheck has the 30-second deadline; longer migrations should use `release_command`. Proxy Services retain their configured `deploy_timeout`. These gates apply on rollback too; old Release plans recover job classification from their saved Compose file, not today's configuration. A failed plain gate does not undo stop-first replacements already made; it prevents the subsequent Proxy switch, not all App downtime.

On Docker 25 or newer, Yoho compiles `start_period: 60s` and `start_interval: 1s` into a proxied or Stateful Service's healthcheck when neither is set, so the first probes run every second while the container starts. The first deploy after upgrading Yoho recreates each Stateful Service that has a healthcheck once, because the compiled healthcheck changed. A healthcheck that sets only timing and no `test` inherits the image's check and gets no fast-start fields; `yoho config check` warns when its `interval` is over 5s with no `start_interval` or `start_period`.

## Proxy HTTP probes

`x-yoho.proxy.health_port` probes a different port on each traffic target (1–65535), without changing `proxy.port` or publishing a host port. `health_timeout` is the timeout for **each HTTP probe**, expressed as a positive Go duration such as `750ms` or `2s`. Omit these fields to retain the traffic port and pinned kamal-proxy default probe timeout. Explicit port `0` and empty/zero/negative/invalid durations are rejected.

```yaml
x-yoho:
  proxy:
    port: 3000
    health_path: /ready
    health_port: 9090
    health_timeout: 750ms
    deploy_timeout: 47  # overall readiness/cutover deadline in seconds
```

The health endpoint must listen on the target's network-reachable interface, not only its loopback address. These settings do not change Docker `HEALTHCHECK`, container readiness, or restore gates. Both Compose candidates and Swarm VIP/task targets use them; Releases save them for rollback. Older Releases omit them and retain the original Proxy defaults and saved deploy/drain deadlines. Changing either field appears in `yoho plan` and is applied to the route.

## Failed deploys

Before removing fresh Proxy candidates, Yoho collects best-effort diagnostics: container State and exit code/OOM, plus the last two healthcheck results and up to 20 recent log lines **only for verified fresh candidates using this deploy's secret inventory**. Retained or unverified containers (including release prerequisites and plain Services) get state/exit/OOM metadata only: rotated values from their older generations cannot be safely redacted using today's inventory. An uncertain Proxy switch still diagnoses only its verified fresh candidates, never old replicas. Batch prerequisite startup failures sample up to 8 prerequisite Services fairly under one shared 30s/8-container/32 KiB budget; samples do not identify which Service caused the batch failure. Collection has a shared 30-second deadline independent of a cancelled deploy, at most 8 containers, and a 32 KiB output cap. Per-container State JSON is capped at 16 KiB; invalid/truncated JSON is omitted, never printed raw. Logs process at most 64 KiB of input per container and retain at most 2 KiB of redacted text. Known resolved/generated secrets (including escaped, multiline and short values) are masked before output limits; incomplete secret prefixes are discarded on interrupted collection. Full inspect, Config, environment and healthcheck commands are never collected. Unavailable diagnostics do not replace the original deploy error. On rollback, arbitrary health/log text is omitted when retained secret values are unavailable for redaction.

Failures report the phase and per-Service facts. With `--json`, `step_fail.failure_facts` adds `phase` and `services` entries (`service`, `convergence`, `proxy_route`). Compose convergence is `not-attempted`, `attempted`, or `complete`; prerequisite phases separately record startup/readiness attempts. **Complete means that Service passed convergence/readiness, not that the whole App was atomically committed.** Proxy route facts are `not-attempted`, `switched`, `retained` (a successful lookup observed no candidate targets), `absent`, or `uncertain`; non-Proxy Services use `not-applicable`. These are observations from this attempt, not a guarantee that a previous Release is healthy or still serves the whole App. A later route failure can leave earlier routes switched; a failed worker gate can leave its stop-first replacement in place. Uncertain Proxy candidates remain running for safety. Inspect `yoho app ps`, Proxy status and `yoho releases` before choosing recovery; no automatic data or migration undo occurs.

Swarm convergence diagnostics report bounded task state metadata from the manager (at most 8 Services, 4 KiB and 30 seconds). Arbitrary task Error, update Message, logs and stack-command output are omitted: normal deploy history, rollback, and automatic retained-spec recovery can all involve older secret inventories. Recovery warnings likewise omit arbitrary error text; original causes remain available internally through error unwrapping. Pre-command status-query failures list planned Services as `not-attempted/not-observed`: this read-only lookup failed before that phase issued an update/job command, not evidence of an attempted update or observed live convergence. This is phase-local, not an App-wide unchanged-state guarantee. Stack-command or post-command status-query failures include planned/existing Service facts as `attempted/unknown replicas=unknown`, rather than stale completion counts. Its failure facts report observed update states/replica counts; `proxy_route: not-attempted` means this phase has not reconfigured Proxy routes, **not** that a stable VIP still serves old tasks. Swarm can already have changed other Services, and rollback/paused states do not prove an App-wide rollback. Worker-node container health/log collection is not performed by this manager diagnostic path.

## Ports

Defaults are 80/443. If the Server already uses them: `proxy.http_port: 8080`, `proxy.https_port: 0`. Behind Cloudflare Tunnel, no published ports and no TLS are needed; see [Cloudflare Tunnel](cloudflare-tunnel.md).

## Proxy logging

New or recreated Proxy containers inherit the Server's Docker default logging driver. Yoho adds `max-size=10m` only for `json-file` and `local`; it does not pass unsupported rotation options to `fluentd`, `syslog`, `journald`, or other drivers. `plan` and `proxy boot` use the same effective arguments. An already matching Proxy is left unchanged: changing the daemon default does not alter existing containers. App Service logging remains controlled by Compose.

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
