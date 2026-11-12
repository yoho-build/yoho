# Defer scale to zero until upstream support or a concrete need

Status: Accepted.

## Context

Railway can sleep idle services. On a fixed-price Server, sleeping saves memory rather than reducing the bill: more idle Apps can share one Server. Issue [#55](https://github.com/yoho-build/yoho/issues/55) asks whether Yoho should offer this.

This is a P3 research item with no user currently blocked on memory; the benefit is memory savings, while the cost is a resident Docker controller.

As checked on 2026-11-12, Yoho pins `basecamp/kamal-proxy:v0.10.2` (`internal/proxy/proxy.go`), also the [newest upstream tag](https://github.com/basecamp/kamal-proxy/tags); upstream publishes tags, with no GitHub Releases. That version has no idle stop or wake-on-request. Its [`pause`](https://github.com/basecamp/kamal-proxy/blob/v0.10.2/internal/cmd/pause.go) and [`resume`](https://github.com/basecamp/kamal-proxy/blob/v0.10.2/internal/cmd/resume.go) commands drain and hold requests up to `--max-pause`, then resume forwarding when explicitly told; they do not start or stop Docker containers.

## Considered Options

- **A. Yoho-owned waker.** A resident container could take over sleeping routes, hold the first request, start the Service, wait for health and route back. An in-path variant stays behind kamal-proxy for opted-in Services and counts requests: it avoids route switching but adds a hop and still needs Docker access. kamal-proxy v0.10.2 exposes no per-route activity to detect idleness otherwise.
- **B. Upstream kamal-proxy support.** [PR #228](https://github.com/basecamp/kamal-proxy/pull/228) is open and unmerged; [draft PR #197](https://github.com/basecamp/kamal-proxy/pull/197) is closed and unmerged. Both propose deploy flags `--idle-timeout` and `--idle-wake-timeout`, plus run flag `--docker-socket`. #228 continues #197, holding requests during wake and checking health before forwarding. Its current design uses direct Docker socket access. [Discussion #222](https://github.com/basecamp/kamal-proxy/discussions/222) covers concurrency, deploys while asleep, Proxy restarts and the socket security tradeoff. None of this has shipped in the pinned version.
- **C. Sablier or another third-party idler.** Sablier is not a reverse proxy: its [blocking strategy](https://github.com/sablierapp/sablier/blob/main/docs/content/concepts/strategies.md) needs a proxy middleware plugin that kamal-proxy lacks, so integration means forking kamal-proxy or adding a second proxy. Its [Docker provider](https://github.com/sablierapp/sablier/blob/main/docs/content/providers/docker.md) starts and stops containers through the Docker socket, directly or through a socket proxy.
- **D. Keep Services running.** Document packing many idle Apps onto one Server in [#49](https://github.com/yoho-build/yoho/issues/49): `mem_limit` caps memory but frees none by itself; swap (`setup.swap`) lets idle pages be paged out. This needs no resident lifecycle controller.

## Decision

Do not build a waker now. Track upstream (B) and keep Services running (D); the sizing guide is tracked in #49. Revisit when kamal-proxy ships idle stop/wake support, or when a concrete user needs it.

Any later implementation needs its own ADR answering at least:

- How per-Service opt-in works, rather than Server-wide opt-in.
- How a resident, public-facing process gets Docker lifecycle control, which [ADR 0003](0003-no-resident-daemon-by-default.md) and the Access Service in [ADR 0011](0011-scoped-access-through-a-gate.md) avoid (no Docker socket), without being able to stop the Proxy, Stateful Services, other Apps or old Releases.
- How to limit sleeping to proxied Services that can wake on a request; databases and workers stay running.
- How deploys and rollbacks, including Gate deploys, work while a Service sleeps.
- How Backups and Scheduled Jobs handle stopped containers.
- How Swarm is handled, since it restarts stopped tasks.
- What happens when many Apps wake at once on a small Server.

## Consequences

Yoho provides no automatic idle sleeping or wake-on-request today. Use `mem_limit` to cap memory and `setup.swap` to let idle pages be paged out, with measured packing guidance tracked in #49; a memory cap alone frees none. Leave headroom for concurrent activity and deploys. A shipped upstream implementation or a concrete user need reopens this decision.
