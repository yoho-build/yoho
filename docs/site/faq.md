# FAQ

**Is there a 5-minute demo?** Yes. [5-minute quickstart](quickstart.md) takes a fresh Server to web + worker + Postgres, then a zero-downtime redeploy and a secret change. The App lives in `examples/quickstart`.

**Do I need a registry?** No. Images are pushed over SSH. A registry is optional.

**Does it run anything on my Server?** Only Docker, kamal-proxy, and your App. Scheduled Jobs (systemd timers) and the tunnel connector are opt-in.

**Customer domains?** Yes. `x-yoho.proxy.custom_domains.allow` tells kamal-proxy to issue a Let's Encrypt certificate only after the App (or an HTTP URL) returns 200 for that hostname. No redeploy per domain. Cloudflare for SaaS is the alternative when TLS stays at Cloudflare; see [Customer-owned domains](custom-domains.md).

**Can I still use `docker compose up` locally?** Yes. `x-yoho` is an ordinary compose extension.

**What happens to my data on deploy or rollback?** Volumes are never removed and Yoho never runs `down -v`. Rollback does not revert migrations; take a Backup first.

**Where are secrets stored on the Server?** As files under the App's state directory (0700 dirs, 0444 files), mounted at `/run/secrets`.

**Windows?** The CLI targets macOS and Linux; Servers must be Linux.

**Is there a web UI?** Yes, read-only and local: `yoho ui` serves a dashboard on `127.0.0.1` from your machine and reads the Servers over SSH; `yoho tui` is the same view in the terminal. Nothing is installed on the Servers. See [Dashboards](dashboards.md). A Server-side dashboard API with alerts is a later, opt-in item.

**Which OS for `yoho setup`?** Debian and Ubuntu.

**Status?** Alpha. Pin the version you use.
