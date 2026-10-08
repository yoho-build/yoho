# Scheduled jobs

Nothing runs on the Server by default. `yoho schedule install` opts a Server in: one systemd timer per job runs the `yoho` binary and exits. No daemon, no sidecar.

```sh
yoho schedule install   # upload binary, stored secrets, specs, and timers
yoho schedule status    # timer state, next/last run, last result
yoho schedule remove
```

Jobs come from `backups.jobs.<name>.schedule` (systemd `OnCalendar`, e.g. `*-*-* 03:00:00`). Jobs without a schedule are on demand only.

- Server-side jobs read secrets stored on the Server at install time, not your password manager.
- With `servers.<name>.sudo: true` units are system-wide (User= the deploy user); otherwise `systemctl --user` is used (enable linger; `install` warns when it is off).
- Unit names: `yoho-<app>-<destination>-<job>`.
