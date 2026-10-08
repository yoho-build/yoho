# Migrate from Dokploy

Dokploy runs compose apps (or Swarm services) and stores config in its own database. Export, rebuild as compose, deploy with Yoho; keep Dokploy running until verified.

1. Get the compose file: Dokploy UI -> Compose -> General (raw compose), or on the server `/etc/dokploy/compose/<app-name>/code/docker-compose.yml` and env `.env` next to it. For Swarm applications, rebuild a compose file from the service image, ports, mounts and env (`docker service inspect`).
2. Strip Dokploy specifics: `dokploy-network`, Traefik `labels:`, `isolatedDeployment` suffixes. Keep named volumes (Dokploy prefixes with the app name; keep or rename and copy data).
3. Domains -> `x-yoho.proxy.hosts` and `port`. Add `health_path` if the app has no `/up`.
4. Env: non-secret values to compose `environment:`; secret values to a password manager and `.yoho/secrets` (`KEY=$(op read ...)`), declared in `x-yoho.secrets`. Never `${VAR}` for secrets.
5. Databases: mark `x-yoho.stateful: true` and add `x-yoho.backup` (`dump`, `volumes`). Dokploy's S3 backups can be restored by hand; configure `backups.targets` in `yoho.yml`.
6. Registry: Dokploy builds on the server; use `builder.location: server` to keep that, or build locally (default).
7. `yoho init`, `yoho config check`, `yoho setup`, `yoho deploy`, move DNS, then stop the Dokploy app.
