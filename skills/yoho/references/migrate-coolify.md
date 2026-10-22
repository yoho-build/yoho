# Migrate from Coolify

The full playbook is the `yoho-migrate-coolify` skill: `skills/yoho-migrate-coolify/SKILL.md` (installed next to this skill by `yoho skill install`), or https://raw.githubusercontent.com/yoho-build/yoho/main/skills/yoho-migrate-coolify/SKILL.md (read it with `curl -sL`).

In short:

1. Inventory read-only: `/data/coolify/applications/<uuid>/` (`docker-compose.yaml`, `.env`), containers by `com.docker.compose.project=<uuid>`, exact volume names, image digests, host ports, restart policies, tunnel routes. Keys and lengths only, never values; never filter `.env` line by line (multiline values leak).
2. Write `compose.yoho.yaml`: drop Coolify labels, network and `SERVICE_*`/`COOLIFY_*` env; pin images to the running digests; declare the existing volumes `external: true` with their names; `stateful` + `backup.dump` for databases; `x-yoho.proxy` for the web Service (no published ports or `container_name` on it).
3. Secrets: copy Coolify's `.env` on the Server to a root-only file (or move values into a password manager); after checking every line is `KEY='single-line value'` or `KEY=plain-word` (a count, never the lines), read it once in `.yoho/secrets` (`_SERVER_ENV=$(ssh host 'cat file')`) and `eval` it per key with `${KEY?missing}`; build args become `builder.secrets`. Compare `yoho secrets list` lengths with the originals.
4. On a Server shared with Coolify: `proxy.http_port: 0`/`https_port: 0` with a tunnel (Traefik keeps 80/443), `setup.firewall: false`, `setup.user: root` + `authorized_keys: []` when connecting as root; read `yoho setup --plan` before changing anything.
5. Cut over: dump databases, have the user turn off Coolify auto-deploy (never edit Coolify's database), stop (not remove) the Coolify containers from a one-time `pre-deploy` hook so the build is not downtime, `yoho deploy`. Never run both stacks on one volume. Roll back: stop Yoho's containers by compose project plus `yoho.app` and `yoho.destination` labels, restore the dump if the new version migrated, then `docker start` the Coolify containers.
6. Point tunnel hostnames at `http://yoho-proxy:80`; delete the Coolify app later without deleting volumes.

## Rehearse on a second Destination first

Rehearse the cutover on a separate Destination before the real one, so the production window only repeats steps you have already seen work:

- A second Destination (`rehearsal`) with its own `servers`, for example a LAN box. It has no cutover Hook and no ingress change, so it never touches the Coolify stack. Set `proxy: {tunnel: null}` in the rehearsal overlay (`yoho.rehearsal.yml`) or give it its own tunnel: the Destination inherits `proxy.tunnel.token_secret`, and `apply` would register the rehearsal Server as a connector of the live tunnel and send production traffic to test data. Do not guard a production-only Hook with `YOHO_DESTINATION`; keep it out of the rehearsal Destination's overlay instead.
- An overlay (`yoho.rehearsal.yml`, `compose.rehearsal.yaml`) that points data at a restored copy of the dump or volume, never at the production data.
- Replace side-effect Services with `sleep` in that overlay: schedulers, off-Server Backups, the tunnel connector, mail and webhook senders.
- Use test-mode payment and API keys.
- Cold start: stop only the rehearsal App's containers, selected by all three ownership labels (`com.docker.compose.project=yoho-<app>-rehearsal`, `yoho.app=<app>`, `yoho.destination=rehearsal`), and leave other Apps and shared infrastructure such as `yoho-proxy` running, then run `yoho deploy -d rehearsal`. That reproduces the ordering of the real cutover (stateful Services first, release commands, health gate, Proxy).
- Fix what the rehearsal finds in the repo files, then reuse them unchanged for production.

