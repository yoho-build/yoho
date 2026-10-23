# 5-minute quickstart

A timed walkthrough: a fresh Debian or Ubuntu Server, a live App (web + worker + Postgres), a zero-downtime redeploy, then a secret change that the Proxy picks up without dropping requests.

The matching App is `examples/quickstart`. Prefer to let an agent do this? See [Set up with your AI agent](agents.md).

## What you need

- A new Debian or Ubuntu Server you can reach with `ssh root@HOST` (or a user with passwordless sudo).
- Docker on this machine, to build images. Apple `container` on Apple silicon also works; see [Builds](builds.md).
- About five minutes of operator time after `yoho setup` finishes. The first setup on a blank Server is mostly waiting for Docker.

## 1. Install Yoho

```sh
curl -fsSL https://yohobuild.com/install.sh | sh
yoho version
```

Details and provenance checks: [Getting started](getting-started.md#install).

## 2. Copy the example

From a clone of this repository:

```sh
cp -a examples/quickstart /tmp/shop
cd /tmp/shop
```

Or grab just that directory from GitHub. The App is one Go binary: `web` serves HTTP, `worker` drains a `jobs` table, `db` is Postgres 17 (a Stateful Service). `x-yoho.release_command` runs `/app migrate` before cutover.

## 3. Point it at your Server

In `yoho.yml`, set `servers.primary.ssh` to `root@YOUR_SERVER` (or `you@YOUR_SERVER` if that user already has sudo). Leave the rest for the demo:

```yaml
proxy:
  http_port: 0
  https_port: 0
  tunnel: {}
```

That starts a [Quick Tunnel](cloudflare-tunnel.md#quick-tunnel-no-account): a public `https://*.trycloudflare.com` URL, no DNS, no published ports. Leave `x-yoho.proxy.hosts` empty so the random hostname matches. After the demo, set a real hostname and `http_port: 80` if you want the Proxy on the Server's IP instead.

`.yoho/secrets` ships a demo greeting only:

```
APP_GREETING=hello-from-yoho
```

In a real App that line is `$(op read …)` or `$(bw get …)`. Postgres gets `POSTGRES_PASSWORD` from `x-yoho.generate` on the Server, once. See [Secrets](secrets.md).

Yoho names each Release after the git commit, so make the copy a repository (`.gitignore` keeps `.yoho/secrets` out of it):

```sh
git init -q && git add -A && git commit -qm "Quickstart"
```

If `git commit` asks who you are, set `git config user.name` and `user.email` first; without a commit there is no Version to deploy.

## 4. Check and provision

```sh
yoho config check
yoho setup            # per step: y, N, or a; or yoho setup -y
```

`yoho setup` installs Docker, the deploy user, keys, linger, and the firewall. Re-running only does what is still needed. [Server setup](setup.md).

## 5. Deploy

```sh
yoho plan             # read-only
yoho deploy           # build, ship, migrate, health-gated cutover, start the tunnel
yoho tunnel status    # Quick Tunnel URL
yoho app ps
```

`yoho deploy` is `apply --auto-approve`. On a terminal, `yoho apply` shows the plan and asks `yes`. Open the tunnel URL: you should see marker `hello-v1` and a greeting fingerprint (length + `sha256_8`), never the greeting itself.

Copy the `https://….trycloudflare.com` URL from the **URL** column of `yoho tunnel status` (or the `→` line `yoho deploy` printed). Then:

```sh
url=https://….trycloudflare.com   # paste, do not scrape CLI output
curl -fsS "$url/up"
curl -fsS "$url/version"
curl -fsS "$url/greeting"
curl -fsS -X POST --data 'from-the-demo' "$url/jobs"
sleep 2
curl -fsS "$url/jobs"
yoho app logs worker -n 20
```

The worker writes `greeting_len` onto each job. That is a length, not a secret.

## 6. Zero-downtime redeploy

Edit `marker` in `main.go` from `hello-v1` to `hello-v2`. In another terminal, loop the health URL:

```sh
while true; do curl -fsS -o /dev/null -w "%{http_code} %{time_total}\n" "$url/up" || echo FAIL; sleep 0.2; done
```

Then:

```sh
git commit -qam "hello-v2"
yoho deploy
curl -fsS "$url/version"   # marker hello-v2
```

`web` is proxied: new containers start next to the old ones, kamal-proxy health-checks `/up`, switches, and drains. The loop should stay on `200`. `worker` is not proxied, so it is replaced stop-first (a brief worker gap, not a web outage). `db` is stateful and stays put when only the image of `web`/`worker` changed. [Zero-downtime & proxy](zero-downtime.md).

## 7. Change a secret, no web downtime

Change the resolved value and redeploy. `--skip-build` reuses the images you just shipped (`.yoho/secrets` is git-ignored, so the Version stays the same). ([`yoho secrets rotate`](secrets.md#rotating-values) does the same from a checkout of the deployed Version and restarts only the Services whose values changed.)

```sh
# edit .yoho/secrets: APP_GREETING=hello-rotated
yoho secrets list          # LENGTH for APP_GREETING changes; values are not printed
yoho deploy --skip-build
curl -fsS "$url/greeting"  # sha256_8 changed
```

`yoho plan` reports `secrets changed` on Services that consume the key. `web` updates in place through the Proxy. `worker` is replaced stop-first again. Do not rotate `POSTGRES_PASSWORD` in this demo: that is a generated secret on the Stateful Service, and changing it is a planned database step, not a Proxy cutover.

Keep a request loop on `/up` during this deploy too. The greeting endpoint never returns the value.

## 8. Clean up

The Quick Tunnel URL is public and `POST /jobs` takes anyone's input. When the demo is over:

```sh
yoho tunnel down
```

## Monthly cost

Same shape of stack: one web Service, one worker, one Postgres. Figures below are **list prices retrieved 2026-10-22**, not a quote, and they exclude your traffic, backups, VAT (where the source excludes it), and anything you add later.

| Stack | Monthly | What that buys | Source |
|---|---|---|---|
| Yoho on one Hetzner Cloud CX23 (Germany / Finland) + one primary IPv4 | **€5.99 / $7.09 excl. VAT** | One Server running the whole App. CX23 is €5.49 / $6.49. Primary IPv4 is €0.50 / $0.60. IPv6 is free. | [Hetzner price adjustment (last change 2026-07-08)](https://docs.hetzner.com/general/infrastructure-and-availability/price-adjustment/); [Primary IPs](https://docs.hetzner.com/cloud/servers/primary-ips/overview/) |
| Render, smallest paid instances, Hobby workspace | **$20** | Web $7 (`0.5c-512mb`) + background worker $7 (`0.5c-512mb`) + Postgres $6 (`0.1c-256mb`). Hobby workspace is $0; compute is extra. Free instances spin down and are not this row. | [Render pricing](https://render.com/pricing) |
| Railway Hobby subscription | **$5** | Plan fee only. Includes $5 of usage. RAM $10/GB-month, CPU $20/vCPU-month, volume $0.15/GB-month, egress $0.05/GB. | [Railway plans](https://docs.railway.com/pricing/plans) |
| Railway always-on web + worker + Postgres | **TODO** | Usage-based; Railway does not publish a fixed price for this stack. Measure a month or leave this cell until someone records a bill with a screenshot. | — |

Yoho itself is free (MIT). The Server bill is the hosting bill. Hosted rows also buy managed Postgres, platform UI, and their bandwidth rules; they are not the same product. Recheck the linked pages before you publish a recording or a talk: providers change prices.

## Demo shot list

A short screen recording, about four minutes of A-roll. Use a large terminal font, never show `.yoho/secrets` after the greeting is rotated, and never run `yoho secrets print --reveal`.

| # | Shot | Says on screen | About |
|---|---|---|---|
| 1 | Title | "Fresh Server → live App in one sitting" | 5 s |
| 2 | `yoho version` then `cat yoho.yml` with `ssh:` visible | One file, one Server | 15 s |
| 3 | `yoho setup -y` (speed up Docker apt) | Provision, not a control plane | 30 s |
| 4 | `yoho deploy` to first healthy `/up` | Build, ship, cut over | 45 s |
| 5 | Browser on the Quick Tunnel URL | marker `hello-v1`, fingerprint, POST a job | 20 s |
| 6 | `yoho app logs worker -n 20` | Job processed, `greeting_len` only | 10 s |
| 7 | Split: request loop on `/up` + edit `marker` + `yoho deploy` | No `FAIL` lines; `/version` shows `hello-v2` | 40 s |
| 8 | Edit `APP_GREETING` off-camera; `yoho secrets list`; `yoho deploy --skip-build` | Length column changes; `/greeting` `sha256_8` changes; loop stays `200` | 30 s |
| 9 | Cost table from this page | One Server vs listed hosted instances | 15 s |
| 10 | End card | `curl -fsSL https://yohobuild.com/install.sh \| sh` and this URL | 5 s |

Optional voice-over beats: Compose stays the source of truth; secrets are files, not `docker inspect`; workers restart stop-first; `yoho secrets rotate` restarts only the Services whose values changed.

## Locally, without a Server

```sh
cd examples/quickstart
docker compose -f compose.yaml -f compose.local.yaml run --rm --build web /app migrate
docker compose -f compose.yaml -f compose.local.yaml up --build
```

Yoho does not load `compose.local.yaml`. That overlay is only for `docker compose up` on a laptop.

## Next

- [Getting started](getting-started.md) — install details, then day-two commands
- [Configuration](configuration.md) — Yoho file and `x-yoho`
- [Secrets](secrets.md) — 1Password, Bitwarden, generated keys
- [Zero-downtime & proxy](zero-downtime.md) — plan, apply, rollback
- [Backups & restore](backups.md) — add a Backup Target when you care about the volume
