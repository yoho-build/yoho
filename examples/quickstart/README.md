# Quickstart example

A small Go App — **web**, **worker**, and **Postgres** — used by the [5-minute quickstart](../../docs/site/quickstart.md).

## On a Server

Edit `servers.primary.ssh` in `yoho.yml`. Yoho names each Release after the git commit, so in a copy of this directory run `git init -q && git add -A && git commit -qm Quickstart` first. Then:

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
docker compose -f compose.yaml -f compose.local.yaml up --build
curl -fsS http://127.0.0.1:8080/up
curl -fsS -X POST --data 'from-laptop' http://127.0.0.1:8080/jobs
```

`APP_GREETING` and `POSTGRES_PASSWORD` stay out of `compose.yaml`. Yoho delivers them as files at `/run/secrets`; the local overlay uses environment variables.
