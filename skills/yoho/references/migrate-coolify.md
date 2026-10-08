# Migrate from Coolify

Coolify stores each compose app on its server; recover the compose file and env, then rebuild them as a Yoho app. Never delete the Coolify app until Yoho is serving and verified.

## 1. Collect the app

Using the Coolify CLI (preferred):

```
coolify resource list
coolify app get <uuid> --format json      # compose, domains, env, build settings
coolify app env list <uuid> --format json
```

Or over SSH on the Coolify server: compose and env live under `/data/coolify/applications/<uuid>/` (`docker-compose.yaml`, `.env`) and services under `/data/coolify/services/<uuid>/`. Volumes: `docker volume ls | grep <uuid>`; volume data is under `/var/lib/docker/volumes/`.

## 2. Build compose.yaml

- Start from the stored `docker-compose.yaml`. Remove Coolify-injected `labels:` (traefik/caddy), `coolify` network, `SERVICE_FQDN_*` and `SERVICE_URL_*` env.
- Keep named volumes with the same names so data carries over, or copy data into new volumes.
- Move domains to `x-yoho.proxy.hosts` and the container port to `x-yoho.proxy.port`. Add `health_path` if not `/up`.

## 3. Secrets

- `SERVICE_PASSWORD_*` / `SERVICE_BASE64_*` magic variables: map to `x-yoho.generate` (`password32`, `base64_32`, ...) when the app should get a fresh value, but to preserve existing credentials (databases already initialized) copy the current value instead.
- Move every sensitive env value into 1Password (or Bitwarden), then reference it in `.yoho/secrets`: `DB_PASSWORD=$(op read op://Prod/shop/db_password)`. Never leave values in the repo.
- Declare per Service in `x-yoho.secrets` (`CONTAINER_NAME:KEY` to alias). Non-secret env goes in compose `environment:`. Remove `${VAR}` secret interpolation; use `*_FILE` env vars (`POSTGRES_PASSWORD_FILE=/run/secrets/POSTGRES_PASSWORD`).

## 4. Stateful data

Mark databases `x-yoho.stateful: true`, with `x-yoho.backup.dump` (e.g. `["pg_dump","-U","postgres","app"]`) and `volumes`. Dump on the old server, restore on the new, or reuse the volume when the new Server is the same machine.

## 5. Cut over

`yoho init`, fill `yoho.yml`, `yoho config check`, `yoho setup` (if new Server), `yoho deploy`. If Cloudflare Tunnel was used with Coolify, point cloudflared at the Yoho proxy (see `proxy.tunnel`). Switch DNS/ingress, verify, then stop the Coolify app (not `down -v`).
