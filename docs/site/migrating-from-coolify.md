# Migrating from Coolify

Let your AI agent do it: the `yoho-migrate-coolify` skill (installed by `yoho skill install`, or [read it on GitHub](https://github.com/yoho-build/yoho/blob/main/skills/yoho-migrate-coolify/SKILL.md)) walks through inventory, secrets, cutover with rollback, and the tunnel switch.

1. **Find the app.** With the Coolify CLI: `coolify app get <uuid> --format json` and `coolify app env list <uuid>`. Or over SSH: `/data/coolify/applications/<uuid>/` (and `/data/coolify/services/<uuid>/`) hold `docker-compose.yaml` and `.env`.
2. **Clean the compose file.** Remove Coolify labels, the `coolify` network, and `SERVICE_FQDN_*` / `SERVICE_URL_*`. Keep volume names to carry data over.
3. **Routing.** Domains become `x-yoho.proxy.hosts`; the container port becomes `proxy.port`.
4. **Secrets.** `SERVICE_PASSWORD_*` / `SERVICE_BASE64_*` map to `x-yoho.generate` for new values; for already-initialized databases, copy the existing value. Put all sensitive env values in 1Password or Bitwarden, reference them from `.yoho/secrets`, and declare per Service in `x-yoho.secrets`. Coolify injects every variable into every container; Yoho gives each Service only what it declares.
5. **Data.** Mark databases `stateful: true` with `x-yoho.backup`; dump and restore or reuse the volume.
6. **Cut over.** `yoho init`, `yoho config check`, `yoho setup --plan`, `yoho plan`. Then dump the databases, turn off Coolify auto-deploy, stop (not remove) the Coolify containers, and `yoho deploy`. Never run both stacks on the same volume. To roll back, stop the Yoho containers and `docker start` the Coolify ones.
7. **Shared Server.** If Coolify keeps running other apps, its Traefik owns 80/443: set `proxy.http_port: 0` and `https_port: 0` behind a Cloudflare Tunnel, and `setup.firewall: false`.
8. **Ingress and cleanup.** Point tunnel hostnames (or DNS) at the Yoho Proxy (`http://yoho-proxy:80`). After a few days, delete the Coolify app with volume deletion off (Yoho uses those volumes).
