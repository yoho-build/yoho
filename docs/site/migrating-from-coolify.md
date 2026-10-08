# Migrating from Coolify

1. **Find the app.** With the Coolify CLI: `coolify app get <uuid> --format json` and `coolify app env list <uuid>`. Or over SSH: `/data/coolify/applications/<uuid>/` (and `/data/coolify/services/<uuid>/`) hold `docker-compose.yaml` and `.env`.
2. **Clean the compose file.** Remove Coolify labels, the `coolify` network, and `SERVICE_FQDN_*` / `SERVICE_URL_*`. Keep volume names to carry data over.
3. **Routing.** Domains become `x-yoho.proxy.hosts`; the container port becomes `proxy.port`.
4. **Secrets.** `SERVICE_PASSWORD_*` / `SERVICE_BASE64_*` map to `x-yoho.generate` for new values; for already-initialized databases, copy the existing value. Put all sensitive env values in 1Password or Bitwarden, reference them from `.yoho/secrets`, and declare per Service in `x-yoho.secrets`. Coolify injects every variable into every container; Yoho gives each Service only what it declares.
5. **Data.** Mark databases `stateful: true` with `x-yoho.backup`; dump and restore or reuse the volume.
6. **Cut over.** `yoho init`, `yoho config check`, `yoho setup`, `yoho deploy`; switch DNS or tunnel ingress to the Yoho Proxy; stop the Coolify app last (never `down -v`).
