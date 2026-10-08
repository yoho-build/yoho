# Migrate from Kamal

Kamal runs one container per role; Yoho runs the compose app. Steps:

1. Read `config/deploy.yml` (and `deploy.<dest>.yml`). Note `service`, `image`, `servers`, `proxy`, `env`, `accessories`, `builder`, `registry`, hooks.
2. Ensure a `compose.yaml` exists. If not, create one: each Kamal role becomes a Service (`web` -> `web`, `job` -> a Service running the job command), each accessory becomes a Service with `x-yoho.stateful: true` and its volumes.
3. `yoho init`, then fill `yoho.yml`:
   - `service` -> `app`; `servers.web.hosts[0]` -> `servers.primary.ssh: <ssh.user>@<host>`; `destinations.production.servers: [primary]`.
   - Multiple hosts or roles on different hosts: not supported yet (one Server per Destination). Pick the primary or use `runtime: swarm`.
   - `builder.arch` -> `builder.platforms: [linux/<arch>]`; `builder.remote` -> `builder.location: remote` + `builder.remote`; `builder.secrets` -> `builder.secrets`.
   - `registry` is optional; drop it to use registry-less transport, or keep as `registry:`.
4. Proxy: `proxy.host` -> `x-yoho.proxy.hosts`, `app_port` -> `port`, `healthcheck.path` -> `health_path`, `ssl: true` -> `tls: true`, `response_timeout`/`drain_timeout` -> `deploy_timeout`/`drain_timeout`. Ports other than 80/443 go in `proxy.http_port`/`https_port`.
5. Env: `env.clear` -> compose `environment:`. `env.secret: [A, B]` -> `x-yoho.secrets: [A, B]`.
6. Secrets: move `.kamal/secrets` to `.yoho/secrets` (same dotenv/`$(...)` syntax; `kamal secrets fetch` adapters become `$(op read ...)` or `secrets.providers`). Replace `KAMAL_REGISTRY_PASSWORD` only if you keep a registry.
7. Hooks: copy `.kamal/hooks/*` to `.yoho/hooks/*`; replace `KAMAL_*` with `YOHO_*` (`KAMAL_VERSION`->`YOHO_VERSION`, `KAMAL_HOSTS`->`YOHO_HOSTS`, `KAMAL_PERFORMER`->`YOHO_PERFORMER`, `KAMAL_DESTINATION`->`YOHO_DESTINATION`, `KAMAL_SERVICE_VERSION`->`YOHO_SERVICE_VERSION`). Keep them executable.
8. App code reading `KAMAL_VERSION` etc. should read `YOHO_VERSION`/`YOHO_APP`/`YOHO_SERVER`.
9. Cutover: `yoho config check`, then `yoho proxy status`. Yoho uses its own `yoho-proxy` container and `yoho` network; stop Kamal's proxy only after Yoho's first deploy is healthy, or use different `proxy.http_port` for a trial. Accessory volumes: attach the same named/bind volumes in compose to keep data; take a database dump first.
