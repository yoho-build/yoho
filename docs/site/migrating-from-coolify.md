# Migrating from Coolify

Let your AI agent do it: the `yoho-migrate-coolify` skill (installed by `yoho skill install`, or [read it on GitHub](https://github.com/yoho-build/yoho/blob/main/skills/yoho-migrate-coolify/SKILL.md)) walks through inventory, secrets, cutover with rollback, and the tunnel switch.

1. **Find the app.** The uuid is in the Coolify UI URL and in the `com.docker.compose.project` label of its containers. On the Server, `/data/coolify/applications/<uuid>/` (and `/data/coolify/services/<uuid>/`) hold `docker-compose.yaml` and `.env`. Inspect keys and lengths, not values.
2. **Clean the compose file.** Remove Coolify labels, the `coolify` network, and `SERVICE_FQDN_*` / `SERVICE_URL_*`. Keep volume names to carry data over.
3. **Routing.** Domains become `x-yoho.proxy.hosts`; the container port becomes `proxy.port`.
4. **Secrets.** `SERVICE_PASSWORD_*` / `SERVICE_BASE64_*` map to `x-yoho.generate` for new values; for already-initialized databases, copy the existing value. Put all sensitive env values in 1Password or Bitwarden, reference them from `.yoho/secrets`, and declare per Service in `x-yoho.secrets`. Coolify injects every variable into every container; Yoho gives each Service only what it declares.
5. **Data.** Mark databases `stateful: true` with `x-yoho.backup`; dump and restore or reuse the volume.
6. **Cut over.** `yoho init`, `yoho config check`, `yoho setup --plan`, `yoho plan`. Dump the databases and have the user turn off Coolify auto-deploy. Stop the Coolify containers from a one-time `.yoho/hooks/pre-deploy` hook: it runs after the image is built and before Yoho starts any Service, so the build is not part of the downtime. Then run `yoho deploy`. Never run both stacks on the same volume. To roll back, stop the Yoho containers by their compose project, `yoho.app` and `yoho.destination` labels and `docker start` the Coolify ones. If the new version already migrated the database, restore the dump first.
7. **Shared Server.** If Coolify keeps running other apps, its Traefik owns 80/443: set `proxy.http_port: 0` and `https_port: 0` behind a Cloudflare Tunnel, `setup.firewall: false`, and with root `setup.user: root` plus `authorized_keys: []`.
8. **Ingress and cleanup.** Point tunnel hostnames (or DNS) at the Yoho Proxy (`http://yoho-proxy:80`). After a few days, delete the Coolify app with volume deletion off (Yoho uses those volumes).

## Rehearse first

For an App that matters, deploy to a second Destination before the real cutover: its own `servers` (a LAN box works), an overlay that points data at a restored copy, side-effect Services (schedulers, off-Server Backups, the tunnel) replaced with `sleep`, and test-mode payment keys. Stop everything there and run `yoho deploy -d rehearsal` to reproduce the ordering of the cutover. The rehearsal Destination has no cutover Hook, so it never touches the Coolify stack or the production tunnel. A first-class one-shot cutover is proposed in [ADR 0012](https://github.com/yoho-build/yoho/blob/main/docs/adr/0012-one-shot-migration-cutover.md).

