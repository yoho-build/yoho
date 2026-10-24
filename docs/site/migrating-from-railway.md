# Migrating from Railway

Railway runs off the Yoho Server. Yoho cannot `docker stop` those containers or reuse those volumes. Move Postgres with a logical dump, keep Railway running until the new App is checked, and switch the hostname once. The cutover is the existing [`migrate` block](configuration.md#migration-cutover-migrate) ([ADR 0012](https://github.com/yoho-build/yoho/blob/main/docs/adr/0012-one-shot-migration-cutover.md)). `migrate.from` stays `coolify`, `dokploy`, or `compose`; record Railway as `compose`.

The sample is `examples/quickstart`: web, worker, and Postgres. Its names-only inventory is `examples/quickstart/railway/inventory.json`. An agent can follow `skills/yoho/references/migrate-railway.md` after `yoho skill install`.

Railway-specific commands below are from Railway's docs, retrieved 2026-10-23. Anything not on those pages is in [What was not verified](#what-was-not-verified).

## What maps where

| Railway | Yoho |
|---|---|
| Service | A compose Service |
| Variable names | `x-yoho.secrets` plus a reference in `.yoho/secrets`, or `x-yoho.generate` for a new Postgres password |
| `RAILWAY_*` variables | Dropped. They name Railway's own network |
| Volume mount | A named volume. The bytes do not come with the name |
| Postgres | `stateful: true`, a volume, and `x-yoho.backup.dump` |
| `healthcheckPath` | A wget `healthcheck` and, when it is not `/up`, `x-yoho.proxy.health_path` |
| `preDeployCommand` | `x-yoho.release_command`. A string becomes `sh -c`, because the Railway schema stores one string, not an argv |
| `startCommand` | `command: ["sh", "-c", "..."]`, or an argv `command` in the inventory |
| `cronSchedule` | Not copied. A Yoho Scheduled Job is a Backup, not a Railway cron |
| `numReplicas` | Not copied. One compose project per Destination |
| Nixpacks, Railpack, Heroku, Paketo | Not converted. Set an image or a Dockerfile yourself |

## 1. Inventory, names only

Railway has no single export of services, volumes, and variable names without values. Two inputs work:

- A names-only inventory JSON. This is Yoho's format, not a Railway file. The sample is the shape.
- One service's `railway.json` or `railway.toml`. [Railway's config-as-code guide](https://docs.railway.com/guides/config-as-code) marks those files deprecated, with a hard cutoff of 2026-12-01, and each file describes one service's build and deploy. Yoho does not read `.railway/railway.ts` and does not run the Railway CLI.

`railway variable list` prints values ([Railway CLI](https://docs.railway.com/cli)). Read the names on screen. Do not redirect the output to a file. Create the items in the 1Password or Bitwarden app. `railway volume list` and `railway volume browse /` show mounts; volume backups and point-in-time recovery restore only inside Railway ([Postgres backups](https://docs.railway.com/guides/postgres-backups-restores)).

## 2. Draft the files

```sh
yoho migrate draft --from examples/quickstart/railway/inventory.json
yoho migrate draft --from examples/quickstart/railway/inventory.json --out /tmp/shop-draft
```

The first command prints `compose.yaml` and `yoho.yml`. `--out` writes those two names only when neither exists, and it does not write `.yoho/secrets`. For one service's `railway.json` or `railway.toml`, pass `--service` (default `web`) and `--project` (default `app`). Nothing is deployed and no Server is contacted.

The reviewed App you deploy is still `examples/quickstart/compose.yaml`. It matches the draft except for empty `proxy.hosts` (a Quick Tunnel catch-all), `stop_grace_period`, and a 2s Postgres health interval. The drafted `yoho.yml` is the switch file: it contains `migrate`. `examples/quickstart/yoho.yml` is the first deploy: no `migrate` block, Quick Tunnel, Railway keeps the hostname.

Checked output for the sample inventory is `examples/quickstart/railway/compose.draft.yaml` and `yoho.draft.yml`.

## 3. Secrets

::: warning
Never paste a secret value into a file, a command, or a log. `yoho migrate draft` refuses values, including `KEY=value`, `registryCredentials`, a URL with a password, and `${{ }}` references.
:::

References, same as [Secrets](secrets.md):

```sh
APP_GREETING=$(op read op://Production/shop/APP_GREETING)
# or
APP_GREETING=$(bw get password shop-greeting)
```

`POSTGRES_PASSWORD` is generated on the Server (`x-yoho.generate: password32`). Do not copy Railway's password. The draft does not emit an `op://` line for it. Web and worker still list `POSTGRES_PASSWORD` under `x-yoho.secrets`, so they receive the generated value. Yoho sets `POSTGRES_PASSWORD_FILE=/run/secrets/POSTGRES_PASSWORD`; the official Postgres image reads that, and the sample reads it too.

Do not copy `DATABASE_URL`. It contains a password and points at Railway. If the app needs a URL, store a new one whose host is the compose Service name `db`.

`yoho config check --offline` does not run `$(...)`. `yoho secrets list` shows keys and lengths.

## 4. Deploy once, while Railway still serves

Delete the `migrate` block for this deploy (the shipped `examples/quickstart/yoho.yml` already has none). Leave the live hostname on Railway. A Quick Tunnel (`proxy.http_port: 0`, `https_port: 0`, `tunnel: {}`) gives you a `*.trycloudflare.com` URL to check the App without joining the production tunnel.

```sh
yoho config check --offline
yoho setup --plan
yoho plan
yoho deploy
yoho app ps
```

Postgres starts empty. The image build is not downtime.

## 5. Move Postgres

Downtime starts when you freeze writes, not when the image was built. Stop writes in the Railway dashboard. `railway scale` exists on the [CLI page](https://docs.railway.com/cli), but that page does not document its flags, so this guide does not invent them.

Logical dumps are the portable layer ([Railway's backup guide](https://docs.railway.com/guides/postgres-backups-restores)). Volume backups and point-in-time recovery stay inside Railway. Clients: `brew install libpq` or `apt-get install postgresql-client`.

The guide's example service name is `postgres`. Substitute yours if it differs. `--tunnel-only` prints host, port, user, password, and database. Do not write that password into a file or into git.

```sh
railway connect postgres --tunnel-only
pg_dump "postgresql://postgres:<password>@localhost:<port>/railway" --format=custom --no-owner --file=backup.dump
```

Or, when the service exposes a public TCP proxy: `pg_dump "$DATABASE_PUBLIC_URL" --format=custom --no-owner --file=backup.dump`. The dump is data. Do not commit it (`*.dump`). `--no-owner` keeps Railway's roles out of the file. The new cluster password is the generated one.

`yoho app exec` does not forward stdin. Restore over SSH. The compose project is `yoho-<app>-<destination>` (`yoho-shop-production` for the sample). `--clean --if-exists` is required because the first deploy already ran `x-yoho.release_command` (`/app migrate`, which creates tables if they are missing). Those flags are [pg_restore](https://www.postgresql.org/docs/current/app-pgrestore.html).

```sh
ssh root@YOUR_SERVER 'docker compose -p yoho-shop-production exec -T db pg_restore -U postgres -d postgres --no-owner --exit-on-error --clean --if-exists' < backup.dump
yoho app exec db -- psql -U postgres -d postgres -c 'SELECT count(*) FROM jobs'
```

Compare that count with Railway before you switch. The switch deploy runs `release_command` again, so it must be safe to repeat. The sample's `/app migrate` is `CREATE TABLE IF NOT EXISTS`.

## 6. Rehearse first

For an App that matters, deploy to a second Destination before the real cutover: its own `servers` (a LAN box works), an overlay that points data at a restored copy, side-effect Services (schedulers, off-Server Backups, the tunnel) replaced with `sleep`, and test-mode payment keys. The rehearsal Destination has no `migrate` block, so it never touches Railway or the ingress. Set `proxy: {tunnel: null}` in the rehearsal overlay (`yoho.rehearsal.yml`): it would otherwise inherit `proxy.tunnel.token_secret` and join the live tunnel.

## 7. Switch

Restore and check the counts first. Then put the `migrate` block back. `from: compose` is a label. `stop.containers` is a name that is not running on this Server (`railway-<project>-<first public service>`, `railway-shop-web` for the sample), so the preflight needs `--cutover-allow-empty`. Do not set `claim`: Railway's volumes are not here. With hostnames, the draft also sets `tunnel.api_token_secret`, `proxy.http_port: 0`, `https_port: 0`, and `proxy.tunnel.token_secret`. The API token needs Account > Cloudflare Tunnel > Edit. Account and tunnel id come from the tunnel token.

```sh
yoho migrate plan --cutover-allow-empty
yoho deploy --skip-build --cutover-allow-empty
```

`yoho deploy` runs a pending cutover even when the compose file is unchanged. `yoho apply` returns before the cutover when the plan has no changes, so it is the wrong command for a migrate-only edit. Build and ship already happened; this deploy stops nothing that is running, runs `release_command`, waits for health, then (when `migrate.tunnel` is set) repoints those hostnames to `http://yoho-proxy:80`. A finished journal does not switch ingress again. Editing the tunnel after `done` does not run the cutover a second time.

DNS without a tunnel stays manual: point the record after the App is healthy. Publishing 80 and 443 is a later edit of `proxy.http_port` and `https_port`. Leave Railway up.

## 8. Roll back

The dump was a copy. Railway still has the data from the freeze, as long as you did not change it and did not remove the deployment.

`yoho rollback --cutover` stops this App's containers (compose project and `yoho.app` / `yoho.destination` labels; never `down -v`), starts the saved old container IDs (there are none, for Railway), and restores `tunnel_before` when ingress was switched. It refuses when the live ingress matches neither the saved config nor the one the cutover left; `--force` restores anyway. It does not revert volumes, database migrations, or DNS. The command warns: volumes and database migrations are not reverted; restore a dump if the new version migrated the data.

- Tunnel: `yoho rollback --cutover` puts the saved ingress back.
- DNS: point the record at Railway yourself, then `yoho rollback --cutover` if you also want the Yoho stack stopped.

`yoho migrate status` shows `rolled_back`. A later `yoho deploy --cutover-allow-empty` with the block still in place starts a new cutover. Do not run `railway down` until you are sure: the [CLI page](https://docs.railway.com/cli) describes it as removing the latest deployment.

## What was not verified

Retrieved 2026-10-23 from the Railway CLI page, the Postgres backup guide, the config-as-code guide, `https://railway.com/railway.schema.json`, the variables reference, and the infrastructure-as-code page.

- The JSON shape of `railway variable list --json`.
- Flags of `railway scale`.
- Dashboard clicks beyond what the backup guide describes.
- A live cutover against Railway and a Server. The draft, `yoho config check --offline` on the sample, and the cutover preflight flag are covered by tests in this repo.
- Parsing `.railway/railway.ts`.
- Whether `railway connect <name> --tunnel-only` accepts every service name. The backup guide's example is `postgres`.
