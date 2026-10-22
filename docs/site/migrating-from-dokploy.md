# Migrating from Dokploy

1. **Get the compose file** from the Dokploy UI (Compose, General) or the server (`/etc/dokploy/compose/<app>/code/`). For Swarm applications, rebuild a compose file from `docker service inspect`.
2. **Clean it.** Drop `dokploy-network`, Traefik labels, and isolated-deployment suffixes. Keep volume names.
3. **Routing.** Domains to `x-yoho.proxy.hosts` and `port`.
4. **Secrets.** Move sensitive env into your password manager and `.yoho/secrets`; declare in `x-yoho.secrets`.
5. **Databases.** `stateful: true` plus `x-yoho.backup`; configure a Backup Target in `yoho.yml` instead of Dokploy's S3 backup.
6. **Builds.** Dokploy builds on the server; use `builder.location: server` to keep that, or build locally.
7. `yoho init`, `yoho config check`, `yoho setup`. For a compose app reusing its volumes, add a [`migrate` block](configuration.md#migration-cutover-migrate) to the production Destination: `from: dokploy`, `stop.compose_project` (the `com.docker.compose.project` label of the Dokploy containers), the reused volumes under `claim.volumes`, and `tunnel.hostnames` when a Cloudflare Tunnel fronts the app. `yoho migrate plan` checks it without changing anything; `yoho deploy` then stops the Dokploy containers after the build, checks that nothing else mounts the volumes, deploys and switches the tunnel ingress; `yoho rollback --cutover` undoes it. Without a tunnel, move DNS after the deploy. Turn off Dokploy auto-deploy first so it does not restart the containers. Swarm applications are not supported by `migrate`: stop them by hand before `yoho deploy`.

## Rehearse first

For an App that matters, deploy to a second Destination before the real cutover: its own `servers` (a LAN box works), an overlay that points data at a restored copy, side-effect Services (schedulers, off-Server Backups, the tunnel) replaced with `sleep`, and test-mode payment keys. The rehearsal Destination has no `migrate` block, so it never touches the Dokploy stack or the ingress. Set `proxy: {tunnel: null}` in its overlay (`yoho.rehearsal.yml`): it would otherwise inherit `proxy.tunnel.token_secret` and join the live tunnel. For a cold start, stop only the rehearsal App's containers (by the `yoho.app` and `yoho.destination` labels) and run `yoho deploy -d rehearsal`.
