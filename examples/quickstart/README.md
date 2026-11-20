# Quickstart example

A small Go App — **web**, **worker**, and **Postgres** — used by the [5-minute quickstart](../../docs/site/quickstart.md).

## On a Server

Edit `servers.primary.ssh` in `yoho.yml`. Yoho names each Release after the git commit, so in a copy of this directory run `git init -q && git add -A && git commit -qm Quickstart` first. Backups are optional; leave the commented example disabled for the initial demo. Then:

```sh
yoho config check
yoho setup
yoho deploy
yoho tunnel status
```

## Locally

Needs Docker. This overlay is not loaded by Yoho:

```sh
docker compose -f compose.yaml -f compose.local.yaml run --rm --build web /app migrate
docker compose -f compose.yaml -f compose.local.yaml up -d --build
curl -fsS http://127.0.0.1:8080/up
curl -fsS -X POST --data 'from-laptop' http://127.0.0.1:8080/jobs
docker compose -f compose.yaml -f compose.local.yaml down
```

`APP_GREETING` and `POSTGRES_PASSWORD` stay out of `compose.yaml`. Yoho delivers them as files at `/run/secrets`; the local overlay uses environment variables.

## From Railway

This App is the sample for [Migrating from Railway](../../docs/site/migrating-from-railway.md). `railway/inventory.json` is names only. `yoho migrate draft --from railway/inventory.json` prints the files checked in as `railway/compose.draft.yaml` and `railway/yoho.draft.yml`. Deploy with this directory's `yoho.yml` first (no `migrate` block). The drafted Yoho file is the switch, after Postgres is restored.

The example includes an opt-in nightly restic Backup. Before uncommenting it, configure
`backups.targets.recovery.repository` to an off-Server disk or remote repository,
and add its `RESTIC_PASSWORD` reference to `.yoho/secrets`. The example path
`/mnt/off-server/yoho-backups` must be externally backed storage owned by the
deploy user; on replacement, reassign ownership to the replacement deploy user. After losing the
Server: point SSH at the replacement, reattach the Backup Target, then run
`yoho setup --yes`, `yoho apply --auto-approve`,
`yoho backup restore latest --yes --report`, `yoho apply --auto-approve`.
Keep ingress closed until `/up`, known jobs, worker completion and
`yoho schedule status` pass. See [the recovery walkthrough](../../docs/site/backups.md#losing-a-server).
