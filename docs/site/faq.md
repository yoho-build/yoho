# FAQ

**Do I need a registry?** No. Images are pushed over SSH. A registry is optional.

**Does it run anything on my Server?** Only Docker, kamal-proxy, and your App. Scheduled Jobs (systemd timers) and the tunnel connector are opt-in.

**Multiple Servers?** Use the Swarm runtime. Roles (Kamal-style placement) are planned.

**Can I still use `docker compose up` locally?** Yes. `x-yoho` is an ordinary compose extension.

**What happens to my data on deploy or rollback?** Volumes are never removed and Yoho never runs `down -v`. Rollback does not revert migrations; take a Backup first.

**Where are secrets stored on the Server?** As files under the App's state directory (0700 dirs, 0444 files), mounted at `/run/secrets`.

**Windows?** The CLI targets macOS and Linux; Servers must be Linux.

**Is there a web UI?** No. A read-only dashboard API is a later, opt-in item.

**Which OS for `yoho setup`?** Debian and Ubuntu.

**Status?** Alpha. Pin the version you use.
