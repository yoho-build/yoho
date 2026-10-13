# Scheduled jobs

Nothing runs on the Server by default. `yoho schedule install` opts a Server in: one systemd timer per job runs the `yoho` binary and exits. No daemon, no sidecar.

```sh
yoho schedule install [JOB...]   # upload binary, stored secrets, specs, and timers
yoho schedule status             # timer state, next/last run, last result
yoho schedule status --max-age 48h  # explicit last-success freshness check (nonzero on unhealthy)
yoho schedule remove [JOB...]
```

Without `JOB` they act on every Backup job of the Destination (install: those with a schedule). On a multi-Server Destination, Scheduled Jobs are installed, checked, and removed only on the first Server (the Swarm manager). `install --binary PATH` supplies the Linux binary yourself.

Jobs come from `backups.jobs.<name>.schedule` (systemd `OnCalendar`, e.g. `*-*-* 03:00:00`). Jobs without a schedule are on demand only.

- Server-side jobs read secrets stored on the Server at install time, not your password manager.
- With `servers.<name>.sudo: true` units are system-wide (User= the deploy user); otherwise `systemctl --user` is used which need linger to run while logged out. `yoho setup` enables it for the deploy user; `install` warns when it is off (fix: `yoho setup`, or `sudo loginctl enable-linger <user>` on the Server).
- Unit names: `yoho-<app>-<destination>-<job>`.

## Last-success checks and retries

Ordinary `schedule status` remains informational: a failed last run is reported but does not change its successful exit status. `--max-age` is an explicit positive Go duration (for example `48h`, or `192h` for a weekly job with margin). Yoho does **not** infer an interval from `OnCalendar`. It reports `healthy`, `stale`, `never-success`, or `future-success` based only on the last-success timestamp, the **Server's clock** and your threshold. The Server clock is read after status using GNU `date -u +%s.%N` (integer Unix seconds plus exactly nine nanosecond digits), so operator/Server clock skew and second truncation do not create false future timestamps. A failed or malformed clock query fails the explicit check; Yoho never falls back to the operator's clock. Ordinary status makes no additional clock query. An age exactly equal to the threshold is healthy; a future timestamp is invalid, not healthy. Last-success is retained when later runs fail. `--max-age` exits nonzero for any unhealthy job or no installed jobs. It measures success freshness, not timer activation, last-attempt success, Backup integrity or restore readiness; those remain separate facts. `--json` result rows add `last_success` (UTC RFC3339, or `-`), `success_health`, and `max_age`. Integrate these facts into your own monitoring; Yoho sends no external notifications.

Retries are **opt-in for Scheduled Backups only**:

```yaml
backups:
  jobs:
    nightly:
      destination: production
      target: offsite
      schedule: "*-*-* 03:00:00"
      retries: 2             # extra attempts; default 0, maximum 5
      retry_delay_sec: 30    # fixed delay; 0/unset means 30, maximum 300
```

Retries are allowed only for volume-only Backups whose selected Services have no `dump` or `pre_backup` commands; arbitrary commands are not assumed idempotent. `post_restore` and local Hooks are never run by Scheduled Backups. The runner holds one Destination lock through attempts, delays and state recording. The stored job timeout (default 6h, unchanged in the systemd unit) bounds the **whole run**, not each attempt, and caller cancellation/deadlines interrupt attempts and delays. Legacy specs with zero retry fields still make one attempt; zero `timeout_sec` uses the 6h default.

A failure is retried only before target storage begins and after safe unpause cleanup. No automatic retry follows cancellation, timeout, uncertain unpause, or target-storage/retention failure—even if a successful Backup ID is known—because repeating storage could create duplicate Backups. Use `backup list` to inspect a storage failure before rerunning manually. State retains the prior `last_success`/Backup ID through failed runs, records each attempt's number, start, duration, actual outcome and redacted error (plus any known stored ID), and records the final error. Recording uses bounded cancellation-independent cleanup; if state recording fails, the CLI exits nonzero rather than claiming the state is current. A hard-killed process cannot record state or perform cleanup.

Reinstall with `schedule install` or use `yoho apply` after changing retries/delay; they are part of the stored spec and plan diff. No daemon, automatic remote notification or additional systemd restart policy is introduced.

## Where the binary comes from

`yoho schedule install` and `yoho apply` put a Linux yoho binary on the Server. Resolution order:

1. `--binary` (`yoho schedule install` only), when you pass one.
2. This binary, when it already matches the Server.
3. The GitHub Release for this version (`release v0.1.0`), except for a dev build.
4. A cross-build from the Yoho source tree.

The step detail names the source: `release v0.1.0`, `cross-built from source`, or `this binary`.

## Apply

`yoho apply` makes Scheduled Jobs match config. A Backup job of this Destination with a schedule is created. An installed job is updated when its schedule, Backup Target, or Services differ (`schedule "*-*-* 03:00:00" → "*-*-* 04:00:00"`). An installed job removed from config is deleted. Jobs without a schedule stay on demand only.
