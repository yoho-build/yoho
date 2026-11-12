# Defer scale to zero until upstream support or a concrete need

Status: Accepted.

## Context

Railway can sleep idle services. On a fixed-price Server, sleeping saves memory rather than reducing the bill: more idle Apps can share one Server. Issue [#55](https://github.com/yoho-build/yoho/issues/55) asks whether Yoho should offer this.

As checked on 2026-11-12, Yoho pins `basecamp/kamal-proxy:v0.10.2` (`internal/proxy/proxy.go`), also the [newest upstream tag](https://github.com/basecamp/kamal-proxy/tags); upstream publishes tags, with no GitHub Releases. That version has no idle stop or wake-on-request. Its [`pause`](https://github.com/basecamp/kamal-proxy/blob/v0.10.2/internal/cmd/pause.go) and [`resume`](https://github.com/basecamp/kamal-proxy/blob/v0.10.2/internal/cmd/resume.go) commands drain and hold requests up to `--max-pause`, then resume forwarding when explicitly told; they do not start or stop Docker containers.

## Considered Options

- **A. Yoho-owned waker per Server.** A resident container would detect idle Services, switch their Proxy routes to itself and drain traffic before stopping them. On the first request it would hold the request, start the Service, wait for health, switch the route back and forward the held request. Concurrent requests must share one wake operation (singleflight). Stateful Services never sleep. This adds Docker socket access, idle detection and a lifecycle state machine that must coordinate with deploys.
- **B. Upstream kamal-proxy support.** [PR #228](https://github.com/basecamp/kamal-proxy/pull/228) is open and unmerged; [draft PR #197](https://github.com/basecamp/kamal-proxy/pull/197) is closed and unmerged. Both propose deploy flags `--idle-timeout` and `--idle-wake-timeout`, plus run flag `--docker-socket`. #228 continues #197, holding requests during wake and checking health before forwarding. Its current design uses direct Docker socket access. [Discussion #222](https://github.com/basecamp/kamal-proxy/discussions/222) covers concurrency, deploys while asleep, Proxy restarts and the socket security tradeoff. None of this has shipped in the pinned version.
- **C. Sablier or another third-party idler.** Sablier's [blocking strategy](https://github.com/sablierapp/sablier/blob/main/docs/content/concepts/strategies.md) holds a request through its reverse-proxy integration until instances are ready or the wait times out. Its [Docker provider](https://github.com/sablierapp/sablier/blob/main/docs/content/providers/docker.md) starts and stops containers through the Docker socket, directly or through a socket proxy. Adopting it would add a resident component and Proxy integration to maintain.
- **D. Keep Services running.** Document packing many idle Apps onto one Server, with measured sizing and per-Service `mem_limit` guidance in [#49](https://github.com/yoho-build/yoho/issues/49). This needs no resident lifecycle controller.

## Decision

Do not build a waker now. Track upstream (B) and keep Services running (D); the sizing guide is tracked in #49. Revisit when kamal-proxy ships idle stop/wake support, or when a concrete user needs it.

Any later implementation must be opt-in per Server, an explicit exception to [ADR 0003](0003-no-resident-daemon-by-default.md), as Scheduled Jobs and scoped access are explicit opt-ins ([ADR 0011](0011-scoped-access-through-a-gate.md); the Gate exits after each request, while the future Access Service is resident). Docker operations must go through a socket proxy restricted to start, stop and inspect of Yoho-owned `yoho-*` containers; a name prefix alone is not proof of ownership. Never sleep Stateful Services ([ADR 0005](0005-stateful-services-are-pinned.md)). Deploys while asleep must coordinate with wake operations so an old Release cannot be restarted or routed after replacement. Upstream shipping the feature triggers review of these constraints, not automatic adoption of its direct socket access.

## Consequences

Yoho provides no automatic idle sleeping or wake-on-request today. Use per-Service `mem_limit` and measure how many Apps fit, leaving headroom for concurrent activity and deploys; #49 will document that guidance. Memory limits do not themselves put a Service to sleep. We avoid a resident Docker controller for now and accept that idle Services keep using memory. A shipped upstream implementation or a concrete user need reopens this decision, including cold-start timeouts and deploy/restart recovery.
