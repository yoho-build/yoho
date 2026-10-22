---
name: yoho-migrate-coolify
description: Move a running Coolify app (Docker Compose or Dockerfile) to Yoho on the same or a new Server, keeping its data volumes, secrets and Cloudflare Tunnel. Use when the user wants to migrate from Coolify, replace Coolify with Yoho, or take a Coolify app over with yoho.yml. Covers read-only inventory, compose conversion, secrets without exposing values, shared-Server safety, cutover with rollback, tunnel switch and cleanup.
---

# Coolify to Yoho

Playbook for moving one Coolify app to Yoho with its data. It assumes the `yoho` skill (or `yoho --help`) for general commands and config fields; read `skills/yoho/references/config-reference.md` when writing `yoho.yml` and `x-yoho`.

Ground rules:

- Ask before every change to the Server, Coolify, DNS or Cloudflare. Inventory is read-only.
- Never print, echo or copy secret values into the chat, the repo, argv or logs. Inspect keys and lengths only.
- The Server may run Coolify and unrelated apps. Touch only the app being moved and Yoho's own resources (`yoho-*` containers, the `yoho` network, the Yoho root).
- Never delete volumes. Never `docker compose down -v`. Stop the Coolify app; delete it only after the user has verified Yoho for some days.

## 1. Inventory (read-only)

Use only the commands below and `docker ps` / `docker logs`. Never run `docker buildx history` (any subcommand) on the Server: on Docker 28.x it can crash `dockerd` and stop every container there, including the other apps. Read build history from the Coolify UI instead.

Find the app's uuid (in the Coolify UI URL, or the `com.docker.compose.project` label of its containers), then on the Server:

```sh
ls -la /data/coolify/applications/<uuid>/          # docker-compose.yaml, .env (services: /data/coolify/services/<uuid>/)
docker ps -a --filter label=com.docker.compose.project=<uuid> --format '{{.Names}}\t{{.Image}}\t{{.Status}}'
docker inspect <container> --format '{{range .Mounts}}{{.Type}} {{.Name}} -> {{.Destination}}{{"\n"}}{{end}}'
docker inspect <container> --format '{{json .HostConfig.PortBindings}}'
docker image inspect <image-id> --format '{{json .RepoTags}} {{json .RepoDigests}}'
docker inspect <container> --format '{{.HostConfig.RestartPolicy.Name}}'
# env keys and entry lengths, never values (a multiline value stays inside one entry):
docker inspect <container> --format '{{range .Config.Env}}{{index (split . "=") 0}} {{len .}}{{"\n"}}{{end}}'
# count .env lines that are not KEY='single-line value' or KEY=plain-word (prints a number, never a line; grep exits 1 when it is 0):
grep -vcE "^([A-Za-z_][A-Za-z0-9_]*=('[^']*'|[A-Za-z0-9_./:@%+,=-]*))?$" /data/coolify/applications/<uuid>/.env
```

Do not print the `.env` file or filter it line by line (`sed 's/=.*//'` and the like): a multiline value spills secret lines. Do not `source` or `eval` it unless that count is 0: dotenv is not shell, and an unquoted value with a space or a `$(...)` would leak part of a value in an error or run a command. Coolify writes values single-quoted on one line, and plain names (such as `SERVICE_NAME_*`) unquoted; both pass.

Record:

- Services, images and the exact volume names in use (Coolify often names them `<uuid>_<volume>`, or keeps names from an earlier Dokploy/compose deploy).
- Host port bindings (often `127.0.0.1:<port>` for DB access over SSH).
- Ingress: Coolify's Traefik on 80/443, or a `cloudflared` Service. For a tunnel, read its routes from the connector log: `docker logs <cloudflared> 2>&1 | grep -o 'config=.*' | tail -1`.
- Build: Dockerfile path, build args. Build-time variables set in the Coolify UI are not in `.env`; ask the user where they come from.
- Existing backups: Coolify's scheduled database backups are often not set up for compose apps. Say so; Yoho will add them.
- Disk space, memory, Docker and Compose versions, and whether the repo's compose file matches what Coolify runs.

Note pre-existing problems (unhealthy containers, routes to missing Services) and report them; do not fix them during the move.

## 2. Compose for Yoho

Start from the repo's compose file (Coolify's stored copy has image IDs and injected labels) and write `compose.yoho.yaml` next to `yoho.yml`:

- Remove Coolify labels, the `coolify` network, `SERVICE_FQDN_*`, `SERVICE_URL_*`, `SERVICE_NAME_*`, `COOLIFY_*`, and the `version:` key.
- Pin third-party images to what Coolify runs: `name:tag@sha256:<RepoDigest>`. This avoids a silent upgrade (`latest`) during the move.
- Reuse data by declaring the existing volumes as external with their exact names:

  ```yaml
  volumes:
    db_data:
      external: true
      name: <existing-volume-name>
  ```

- Databases and caches with volumes: `x-yoho.stateful: true`. Add `backup.dump`/`restore_dump` (for Postgres: `["pg_dump","-U","<user>","-Fc","<db>"]` and `["pg_restore","-U","<user>","-d","<db>","--clean","--if-exists","--no-owner"]`).
- The web Service: `x-yoho.proxy` with `hosts` (every hostname that reaches it), `port`, `health_path`, and a compose `healthcheck` using a tool the image has (curl or wget).
- Non-secret config goes literally in `environment:`. Keep empty values as `""` when the app sets them today, so env validation behaves the same.
- Keep host ports on non-proxied Services (often `127.0.0.1:<port>` for a database) if the user relies on them; they free up only once the Coolify containers stop. A proxied Service must not publish ports or set `container_name` (`yoho config check` rejects both): drop them from the web Service and reach it through the Proxy or `yoho app exec`.

## 3. Secrets without exposing values

Coolify's `.env` holds runtime values, usually single-quoted. Pick a source with the user:

- A password manager: `KEY=$(op read op://Vault/item/field)` in `.yoho/secrets`. Moving values there is the user's job or needs their session.
- Or keep them on the Server (only when the count above is 0): copy the file on the Server (never through the chat) to a root-only path outside Coolify, so deleting the Coolify app later cannot lose them:

  ```sh
  install -d -m 700 /root/yoho-secrets
  install -m 600 /data/coolify/applications/<uuid>/.env /root/yoho-secrets/<app>.env
  ```

  Then in `.yoho/secrets`, read the file once over SSH and evaluate it locally for each key. Earlier keys are in the environment of later `$(...)` commands. `${KEY?missing}` makes a missing key fail the deploy instead of yielding an empty value:

  ```sh
  # One SSH read; no Service declares _SERVER_ENV, so it never reaches a container.
  _SERVER_ENV=$(ssh user@host 'cat /root/yoho-secrets/<app>.env')
  POSTGRES_PASSWORD=$(eval "$_SERVER_ENV"; printf %s "${POSTGRES_PASSWORD?missing}")
  ```

  Re-run the format count whenever someone edits that file. One `ssh` per key also works, but each opens a new connection, and resolving runs serially.

- Values built from other secrets use `${VAR}` inside `.yoho/secrets`, e.g. `DB_URI=postgres://app:${DB_PASSWORD}@db:5432/app`.
- Already-initialized databases keep their existing password. Do not use `x-yoho.generate` for them.
- Declare per Service in `x-yoho.secrets` (`NAME:KEY` to rename) with `secrets_as_env: true` when the app reads env. Coolify gave every container every variable; give each Service only what it needs.
- Build secrets (tokens passed as build args): list them in `builder.secrets` and read them with `RUN --mount=type=secret,id=KEY,required=false`. Keep the old `ARG` for now, so a Coolify rebuild before the cutover still works.
- Check: `yoho secrets list -d <dest>` must resolve every key. Each length should equal the `.env` value length without its quotes. Compare lengths, not values.

## 4. yoho.yml on a shared Server

```yaml
app: <app>
compose: [compose.yoho.yaml]
servers:
  main: { ssh: root@host }
destinations:
  production: { servers: [main] }
builder:
  location: server        # build on the Server like Coolify did; needs rsync
  secrets: [BUILD_TOKEN]
proxy:                     # Coolify's Traefik keeps 80/443
  http_port: 0
  https_port: 0
  tunnel:
    token_secret: TUNNEL_TOKEN
setup:
  user: root               # when connecting as root; avoids a new deploy user
  authorized_keys: []      # authorize no extra keys (unset = your ~/.ssh/*.pub)
  firewall: false          # do not enable ufw on a Server with other apps
  auto_updates: false
backups:
  targets:
    local: { type: archive, repository: /var/lib/yoho/backup-archives/<app>, format: tar.gz, keep_last: 7 }
  jobs:
    nightly: { destination: production, services: [<db>], target: local, schedule: "*-*-* 04:20:00" }
```

- Without a tunnel and with 80/443 taken, publish the Proxy on another port (`http_port: 8080`) and route to it from Traefik or DNS, or plan the Traefik removal separately.
- Run `yoho setup --plan` and read every step. On a shared Server, decline steps that change accounts, keys or the firewall; with root, `authorized_keys: []` and `firewall: false`, only Yoho directories, linger (for backup timers) and, for server builds when it is missing, the rsync package should remain.
- `yoho config check`, then `yoho plan -d <dest>`. The plan should only add `yoho-*` resources. Anything that touches other projects is a stop.
- A local `.env` that compose loads triggers secret warnings. Make sure git ignores it, so `builder.location: server` does not ship it.

## 5. Cutover

Rehearse first when the app matters: a second Destination on another box (a LAN machine is fine) with no cutover Hook, an overlay that points data at a restored copy, side-effect Services (schedulers, off-Server Backups, the tunnel) replaced with `sleep`, test-mode payment keys, and a cold-start run (stop everything, then `yoho deploy -d <rehearsal>`) to reproduce the cutover ordering. Details in `skills/yoho/references/migrate-coolify.md`. Keep the one-time Hook out of the rehearsal Destination.

Get the user's OK on a downtime window. The old and new stacks must never run on the same data volume at once: two Postgres servers on one data directory corrupt it.

1. Back up: `pg_dump -Fc` each database from the running Coolify containers into a dated directory on the Server, and check it with `pg_restore -l`.
2. Ask the user to turn off auto-deploy for the app in the Coolify UI (or its API, if the user has a token and you have checked the current API). Do not edit Coolify's database. Then confirm no deployment is running for the app (the Coolify deployments list; no `coolify-helper` build in `docker ps`). Otherwise a git push restarts the old stack on the same volumes.
3. Check each old container's restart policy (`{{.HostConfig.RestartPolicy.Name}}`). `unless-stopped` and `no` stay down after `docker stop`. Change `always` to `unless-stopped` first (`docker update --restart=unless-stopped <id>`), or a Docker restart would bring the old database back.
4. Stop the old stack after the build, not before it. A one-time `.yoho/hooks/pre-deploy` hook runs after the image is built and shipped, and before Yoho starts any Service, so the downtime does not include the build:

   ```sh
   #!/bin/sh
   # One-time Coolify cutover; delete this hook afterwards.
   set -eu
   # The remote side runs in its own POSIX shell with set -eu, so a failing docker command fails the hook.
   ssh user@host sh -s <<'EOF'
   set -eu
   ids=$(docker ps -q --filter label=com.docker.compose.project=<uuid>)
   if [ -n "$ids" ]; then docker stop $ids >/dev/null; fi
   for v in <data-volume-1> <data-volume-2>; do
     if [ -n "$(docker ps -q --filter volume=$v)" ]; then echo "volume $v is still in use" >&2; exit 1; fi
   done
   EOF
   ```

   `chmod +x` it, then run `yoho deploy -d <dest>`. The volume check also makes a later deploy fail if the hook is left in place, because Yoho's own containers then use the volumes. Yoho starts the stateful Services, runs release commands, health-gates the web Service, switches the Proxy and starts the tunnel connector. Remove the hook once the deploy succeeds.
5. If it fails and the fix is not quick, roll back:
   - Stop every Yoho container of the app by all three ownership labels: `docker ps -q --filter label=com.docker.compose.project=yoho-<app>-<dest> --filter label=yoho.app=<app> --filter label=yoho.destination=<dest>`, then `docker stop` those IDs if there are any (none is normal when the deploy failed before starting a Service; `docker stop` with no IDs exits 1, so do not stop there). Do not match on names (a Service with `container_name` would not match), and do not rely on the project name alone (`foo-bar`/`prod` and `foo`/`bar-prod` share it).
   - Confirm that nothing else uses the data volumes: `docker ps --filter volume=<name>` must be empty. Also list stopped users with `docker ps -a --filter volume=<name>`: set any Yoho container there with restart policy `always` to `docker update --restart=no`, or a Docker restart brings it back.
   - If the new version ran migrations the old code cannot handle (check the app log and the migrations in the diff), the database has to go back to the step 1 dump. Ask the user before restoring.
   - Only then `docker start` the Coolify containers.

## 6. Verify

- Every hostname returns 200 on the health path, over the public URL.
- The app logs show migrations completing. Row counts or the newest records match the pre-migration dump.
- `yoho app ps`, `yoho releases`.
- `yoho backup run` then `yoho backup list`. `systemctl --user list-timers | grep yoho` shows the nightly job.
- Background workers (queues, job engines) connect and register.

## 7. Cloudflare Tunnel

`proxy.tunnel` runs `yoho-tunnel` with the same token. Its public hostnames must point at `http://yoho-proxy:80`, so deploys are zero-downtime.

- Until then, old routes such as `http://web:3000` may keep working, because proxied Services join the `yoho` network under their service name. Do not rely on it: another app on that network with the same service name makes it ambiguous.
- Change the routes in the Cloudflare dashboard, or with the API for remotely managed tunnels. `wrangler tunnel` cannot edit routes.

  ```sh
  # account and tunnel id are in the token (base64 JSON: a = account, t = tunnel); never print the s field
  GET/PUT https://api.cloudflare.com/client/v4/accounts/<account>/cfd_tunnel/<tunnel>/configurations
  ```

  Save the GET result first. Change only the `service` of the app's hostnames and keep the other rules (other hostnames, the final `http_status:404`). PUT `{"config": ...}`. A token with Cloudflare Tunnel edit permission works, including a `wrangler login` OAuth token with connectivity scope.
- Verify: the connector log shows the new config version, and the `yoho-proxy` logs show requests for each hostname.

## 8. After the move

- Report: downtime window, data checks, backup location, what changed in the repo (`yoho.yml`, `compose.yoho.yaml`, `.yoho/secrets` with refs only, Dockerfile secret mount), and pre-existing issues found.
- Deploys now come from `yoho deploy` (or CI). Coolify's git webhook no longer applies.
- After the user has verified for some days: delete the Coolify app with its volume deletion option off, because Yoho uses those volumes. Remove the pre-migration dumps when they are no longer needed. Remove the old `ARG` from the Dockerfile once Coolify is gone.
