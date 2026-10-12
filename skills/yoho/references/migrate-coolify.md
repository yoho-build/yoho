# Migrate from Coolify

The full playbook is the `yoho-migrate-coolify` skill: `skills/yoho-migrate-coolify/SKILL.md` (installed next to this skill by `yoho skill install`), or https://raw.githubusercontent.com/yoho-build/yoho/main/skills/yoho-migrate-coolify/SKILL.md (read it with `curl -sL`).

In short:

1. Inventory read-only: `/data/coolify/applications/<uuid>/` (`docker-compose.yaml`, `.env`), containers by `com.docker.compose.project=<uuid>`, exact volume names, image digests, host ports, restart policies, tunnel routes. Keys and lengths only, never values; never filter `.env` line by line (multiline values leak).
2. Write `compose.yoho.yaml`: drop Coolify labels, network and `SERVICE_*`/`COOLIFY_*` env; pin images to the running digests; declare the existing volumes `external: true` with their names; `stateful` + `backup.dump` for databases; `x-yoho.proxy` for the web Service (no published ports or `container_name` on it).
3. Secrets: copy Coolify's `.env` on the Server to a root-only file (or move values into a password manager); after checking every line is `KEY='single-line value'` or `KEY=plain-word` (a count, never the lines), read it once in `.yoho/secrets` (`_SERVER_ENV=$(ssh host 'cat file')`) and `eval` it per key with `${KEY?missing}`; build args become `builder.secrets`. Compare `yoho secrets list` lengths with the originals.
4. On a Server shared with Coolify: `proxy.http_port: 0`/`https_port: 0` with a tunnel (Traefik keeps 80/443), `setup.firewall: false`, `setup.user: root` + `authorized_keys: []` when connecting as root; read `yoho setup --plan` before changing anything.
5. Cut over: dump databases, have the user turn off Coolify auto-deploy (never edit Coolify's database), stop (not remove) the Coolify containers from a one-time `pre-deploy` hook so the build is not downtime, `yoho deploy`. Never run both stacks on one volume. Roll back: stop Yoho's containers by compose project plus `yoho.app` and `yoho.destination` labels, restore the dump if the new version migrated, then `docker start` the Coolify containers.
6. Point tunnel hostnames at `http://yoho-proxy:80`; delete the Coolify app later without deleting volumes.
