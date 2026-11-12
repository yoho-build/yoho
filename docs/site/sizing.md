# How big a Server?

Tiny idle Apps use little memory. Size a Server for busy Services, builds,
deploys and Backups, then measure your own App before adding more.

## Measured idle use

Measured **2026-11-12, 11:20–11:47 UTC** on Azure Standard_B2s (2 vCPU,
3.8 GiB usable RAM), Ubuntu 24.04, Docker, no swap, kamal-proxy v0.10.2,
and `builder.location: server`. Container values are medians of three
`docker stats` samples, 30 seconds apart, after 180 seconds idle.
The App is [examples/quickstart](https://github.com/yoho-build/yoho/tree/main/examples/quickstart),
with Redis added for the four-Service trials.

| What | Idle memory |
|---|---|
| Proxy alone, settled | 6.1 MiB; later samples 3.1–3.2 MiB |
| Go web Service | 2.4 MiB |
| Go worker Service | 2.6 MiB |
| `postgres:17-alpine`, defaults | 28.7 MiB |
| `redis:7-alpine` | 3.6 MiB |
| One App, all four containers | 41.0 MiB |
| Five Apps, all twenty containers | 191.7 MiB |
| Postgres tuning trial: default → tuned | 28.4 → 14.3 MiB |
| Server baseline: OS + Docker + Proxy | about 743 MiB used |
| Server with one → five Apps (`free -m` used) | 771 → 1017 MiB |

All five Apps answered HTTP 200. Idle CPU was about 0–0.06% per container,
with brief 3–5% healthcheck blips. These are idle observations, not load tests.
The four added Apps averaged about 38 MiB of container memory and 61 MiB
of Server memory each. The Server figure includes kernel, page-cache and
shared memory that `docker stats` does not show; use it for rough packing.

## A memory estimate, then a planning budget

Reserve about **750 MiB for the OS, Docker and Proxy**, then roughly
**40 MiB in containers / 60 MiB on the Server per tiny idle App** of this
shape, including Redis. Using the measured 61 MiB increment:

| Server RAM (binary units) | Idle-only arithmetic | Whole Apps, rounded down |
|---|---|---|
| 2 GiB | (2048 − 750) / 61 | 21 |
| 4 GiB | (4096 − 750) / 61 | 54 |
| 8 GiB | (8192 − 750) / 61 | 122 |

**Plan far lower.** Only five Apps were tested. Memory grows under load;
`builder.location: server` needs RAM for Go/npm builds; zero-downtime deploys
run old and new proxied containers side by side; Postgres caches grow with
data; Backups need resources too. CPU, disk and traffic can limit you first.

Budget each App at the sum of its Services' `mem_limit` values, reserve the
baseline and build/deploy/Backup peaks, and keep **25–30% of Server RAM free**
as headroom. Choose limits from measurements under representative load.
Run `docker stats --no-stream` on the Server and compare with `free -m`;
repeat during builds, deploys, Backups and busy periods.

## Swap and Service limits

On 1–4 GiB Servers, enable swap in the Yoho file, then run `yoho setup`:

```yaml
setup:
  swap: 2G
```

[Swap](setup.md) lets idle pages move out of RAM. It is slower than RAM and
does not replace headroom. Compose `mem_limit` caps a Service's container;
it does not free memory by itself. A measured small Go web Service stayed
healthy with this setting, and `docker stats` showed `/ 64MiB`:

```yaml
services:
  web:
    mem_limit: 64m
  db:
    image: postgres:17-alpine
    command: ["postgres", "-c", "shared_buffers=16MB", "-c", "max_connections=20"]
```

Merge these settings into your existing Services. The Postgres settings cut
idle memory to 14.3 MiB in the tuning trial, but restrict cache and connections;
check your workload before adopting them. Too little memory can cause an OOM
kill; restart behavior follows the Service's restart policy.
Yoho keeps idle Services running; automatic sleeping is
[deferred (ADR 0014)](https://github.com/yoho-build/yoho/blob/main/docs/adr/0014-scale-to-zero.md).
For many customer hostnames, see the [custom-domain scale results](custom-domains.md#scale-test):
the 300-certificate cold run grew Proxy memory from 9.7 to 18.3 MiB.

## Railway estimate for the quickstart

[Railway plans](https://docs.railway.com/pricing/plans) and
[pricing](https://railway.com/pricing), retrieved **2026-11-12 at 11:29 UTC**:
RAM $10/GB-month, CPU $20/vCPU-month, volume $0.15/GB-month, egress $0.05/GB.
For always-on web + worker + Postgres (no Redis), measured idle RAM totals
33.7 MiB (about 0.033 GB for this estimate). **Assume 0.02 vCPU per container**
and a 1 GB volume, with no egress: RAM $0.33 + CPU (3 × 0.02 × $20) $1.20
+ volume $0.15 = **$1.68 usage/month**. CPU dominates; that CPU input is an
assumption, not a measurement of Railway usage.

Hobby's $5/month and Pro's $20/month fees count toward usage: this estimate
would be **$5 on Hobby / $20 on Pro**. Five such Apps total $8.40 usage,
so about **$8.40 on Hobby / $20 on Pro**, assuming no other usage.
This is an **estimate, not a bill**; a busier App costs more.
