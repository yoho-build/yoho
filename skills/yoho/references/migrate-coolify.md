# Migrate from Coolify

The full playbook is the `yoho-migrate-coolify` skill: `skills/yoho-migrate-coolify/SKILL.md` (installed next to this skill by `yoho skill install`), or https://raw.githubusercontent.com/yoho-build/yoho/main/skills/yoho-migrate-coolify/SKILL.md (read it with `curl -sL`).

In short:

1. Inventory read-only: `/data/coolify/applications/<uuid>/` (`docker-compose.yaml`, `.env`), containers by `com.docker.compose.project=<uuid>`, exact volume names, image digests, host ports, tunnel routes. Keys only, never values.
2. Write `compose.yoho.yaml`: drop Coolify labels, network and `SERVICE_*`/`COOLIFY_*` env; pin images to the running digests; declare the existing volumes `external: true` with their names; `stateful` + `backup.dump` for databases; `x-yoho.proxy` for the web Service.
3. Secrets: copy Coolify's `.env` on the Server to a root-only file (or move values into a password manager) and reference each key from `.yoho/secrets`; build args become `builder.secrets`. Compare `yoho secrets list` lengths with the originals.
4. On a Server shared with Coolify: `proxy.http_port: 0`/`https_port: 0` with a tunnel (Traefik keeps 80/443), `setup.firewall: false`, read `yoho setup --plan` before changing anything.
5. Cut over: dump databases, turn off Coolify auto-deploy, stop (not remove) the Coolify containers, `yoho deploy`. Never run both stacks on one volume. Roll back with `docker start` on the Coolify containers.
6. Point tunnel hostnames at `http://yoho-proxy:80`; delete the Coolify app later without deleting volumes.
