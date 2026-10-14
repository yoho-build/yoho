# Local read-only dashboards: `yoho tui` and `yoho ui`

## Context

Operators want to see an App at a glance: which Servers are up, which Services are healthy, how loaded the hosts are, which Release is current, and what a container just logged. Today that takes `yoho app ps`, `app stats`, `app logs`, `releases` and `proxy status`, one Server at a time.

Comparable tools put the dashboard on the Server: Dozzle, Dockge and Beszel mount the Docker socket (with an agent per host for several hosts), and Coolify, Dokploy and Openship run a web platform with its own database. ADR 0003 rules out a resident process on the Server by default, VISION lists a web control plane as not planned, and Kamal and Uncloud have no UI at all. ONCE ships a TUI that runs on its single host.

## Decision

1. **Runs on the operator's machine.** `yoho tui` and `yoho ui` load the Yoho file like every other command, connect to the Destination's Servers over the existing SSH ControlMaster, and read. Nothing is installed, started or left running on a Server, so ADR 0003 holds.

2. **One collector, two views.** `internal/dash` runs one POSIX sh script per Server: host facts from `/proc`, `uname` and `df` (the Yoho root and Docker's root), `docker ps` filtered to the App's compose project or swarm stack and `yoho-proxy`, `docker stats --no-stream` for those containers, and the Release records. `dash.Live` caches a Snapshot so several viewers share one collection per interval. Release secret fingerprints are dropped from the Snapshot.

3. **Read-only and Yoho-owned only.** The script and the API only read. Container logs are served only for containers in the latest Snapshot of that Server, so other projects on a shared Server (the QA Server runs production services) are never listed or read.

4. **`yoho ui` is local and authenticated.** It listens on `127.0.0.1` (port 0 by default), accepts only loopback `Host` headers (DNS rebinding), only GET and HEAD, and sends a strict CSP. The CLI prints a one-time login link and opens it through a 0600 redirect file, so the code never appears in a process argv that other local users can read. Visiting it redirects to `/#session=TOKEN`; the page moves the token into its origin's storage, strips it from the URL, and sends it as a Bearer header. No cookie is used: cookies are shared by every port of `127.0.0.1`, so they would leak to other local services and let two `yoho ui` instances overwrite each other. The web app (Vite, React, Tailwind, shadcn components on Base UI, in `web/`) is built into `internal/webui/dist` and embedded with `go:embed`, so the binary stays self-contained and `go build` needs no Node. CI rebuilds it and fails when the committed build is stale.

5. **Actions come later and go through the CLI.** A future deploy, rollback or backup button runs the same local CLI flow with a confirmation and streams its steps; the UI never gets a Docker or shell endpoint. The per-Server dashboard API for unattended alerting stays the opt-in item of ADR 0003.

## Consequences

- No new Go dependencies; `golang.org/x/term` was already used. The web app adds npm dev tooling under `web/` only.
- Collection costs about `docker stats --no-stream` (1–2 s) per refresh and per Server; the default interval is 5 s.
- Container logs are shown as Docker returns them. Yoho does not resolve secrets for these views, so it cannot redact values an application prints.
- The dashboards show one Destination; switch with `-d`.
