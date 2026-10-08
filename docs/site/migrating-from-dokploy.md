# Migrating from Dokploy

1. **Get the compose file** from the Dokploy UI (Compose, General) or the server (`/etc/dokploy/compose/<app>/code/`). For Swarm applications, rebuild a compose file from `docker service inspect`.
2. **Clean it.** Drop `dokploy-network`, Traefik labels, and isolated-deployment suffixes. Keep volume names.
3. **Routing.** Domains to `x-yoho.proxy.hosts` and `port`.
4. **Secrets.** Move sensitive env into your password manager and `.yoho/secrets`; declare in `x-yoho.secrets`.
5. **Databases.** `stateful: true` plus `x-yoho.backup`; configure a Backup Target in `yoho.yml` instead of Dokploy's S3 backup.
6. **Builds.** Dokploy builds on the server; use `builder.location: server` to keep that, or build locally.
7. `yoho init`, `yoho config check`, `yoho setup`, `yoho deploy`, move DNS, then stop the Dokploy app.
