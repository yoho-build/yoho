# Scheduled jobs

Nothing runs on the Server by default. `yoho schedule install` opts a Server in: one systemd timer per job runs the `yoho` binary and exits. No daemon, no sidecar.

```sh
yoho schedule install [JOB...]   # upload binary, stored secrets, specs, and timers
yoho schedule status             # timer state, next/last run, last result
yoho schedule remove [JOB...]
```

Without `JOB` they act on every Backup job of the Destination (install: those with a schedule). On a multi-Server Destination, Scheduled Jobs are installed, checked, and removed only on the first Server (the Swarm manager). `install --binary PATH` supplies the Linux binary yourself.

Jobs come from `backups.jobs.<name>.schedule` (systemd `OnCalendar`, e.g. `*-*-* 03:00:00`). Jobs without a schedule are on demand only.

- Server-side jobs read secrets stored on the Server at install time, not your password manager.
- With `servers.<name>.sudo: true` units are system-wide (User= the deploy user); otherwise `systemctl --user` is used (enable linger; `install` warns when it is off).
- Unit names: `yoho-<app>-<destination>-<job>`.

## Where the binary comes from

`yoho schedule install` and `yoho apply` put a Linux yoho binary on the Server. Resolution order:

1. `--binary` (`yoho schedule install` only), when you pass one.
2. This binary, when it already matches the Server.
3. The GitHub Release for this version (`release v0.1.0`), except for a dev build.
4. A cross-build from the Yoho source tree.

The step detail names the source: `release v0.1.0`, `cross-built from source`, or `this binary`.

## Apply

`yoho apply` makes Scheduled Jobs match config. A Backup job of this Destination with a schedule is created. An installed job is updated when its schedule, Backup Target, or Services differ (`schedule "*-*-* 03:00:00" → "*-*-* 04:00:00"`). An installed job removed from config is deleted. Jobs without a schedule stay on demand only.
