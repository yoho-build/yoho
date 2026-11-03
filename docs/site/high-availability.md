# High availability & recovery

Yoho deploys to Servers you own. It does not make a database highly available, and it does not pretend to. This page says what fails when a Server fails, and how you get back.

## Compose: one Server per Destination

The compose runtime (the default) deploys a Destination to exactly one Server. That Server is a **single point of failure**: if it dies, the App is down until you rebuild it somewhere else. Zero-downtime deploys ([kamal-proxy](zero-downtime.md)) protect against bad releases, not against a lost machine.

Two Destinations (production, staging) on two Servers are two independent copies of the App, not a failover pair. Yoho never moves traffic between them.

## What Swarm adds, and what it doesn't

With `runtime: swarm` ([Swarm](swarm.md)) a Destination spans several Servers:

- **Stateless Services** (Web, workers) run replicas on any node. If a worker node dies, Swarm reschedules its tasks on the others, and the Proxy on every node keeps routing to the service VIP.
- **Stateful Services stay pinned to the first Server** ([ADR 0005](https://github.com/yoho-build/yoho/blob/main/docs/adr/0005-stateful-services-are-pinned.md)). Their volume lives on that disk. If the first Server dies, the database is gone with it, and Swarm will not (and must not) start an empty copy elsewhere.
- **The first Server is also the only manager** that `yoho swarm init` creates. Losing it loses the control plane until you rebuild it.

So Swarm removes the single point of failure for stateless capacity. The data, and therefore the App, still depends on one Server.

## Database replication is out of scope

Replicated or clustered databases (PostgreSQL streaming replication with Patroni, MySQL group replication, managed databases such as RDS, Cloud SQL, Neon or Supabase) are the way to survive a lost database Server without data loss. Yoho does not set them up, fail them over, or reconcile split-brain. If you need that:

- use a managed database and point the App at it with a secret, or
- run the cluster yourself and treat it as an external dependency of the App.

Yoho's job then is deploying the stateless Services against it.

## Off-Server Backups are the recovery path

For a database Yoho runs, recovery after losing its Server is **restore from a Backup**. That only works if the Backup is not on the lost Server:

- Use an off-Server [Backup Target](backups.md#targets-and-jobs-yohoyml): restic to S3/B2/SFTP/`rclone:`, or an encrypted `archive` on an rclone remote. A local path on the same Server is lost with it.
- Or mirror archives to another machine with [`yoho backup pull`](backups.md#mirror-archives-locally-backup-pull).
- Use an encrypted Target so generated secrets (`x-yoho.generate`, such as the database password) are in the Backup too; otherwise keep copies of them elsewhere.
- Schedule Backups ([Scheduled Jobs](scheduled-jobs.md)). Your recovery point objective (RPO) is the Backup interval: data written after the last Backup is lost.

The steps and measured timings are in the [Losing a Server](backups.md#losing-a-server) runbook.

## Know your recovery time: drills

A Backup you have never restored is a hope. `yoho backup drill` restores the newest Backup into a throwaway project next to the running App, waits for the healthchecks, runs your `x-yoho.backup.verify` command and prints how long download, restore and health took: a recovery time (RTO) estimate for the data part of a recovery. Run it on a schedule you trust, after schema changes, and before you need it. See [Restore drills](backups.md#restore-drills).

`yoho backup restore --from production -d staging` exercises the full path on a second Destination, including the deployed App starting against restored data.

## Summary

| Failure | Compose Destination | Swarm Destination |
| --- | --- | --- |
| Bad release | Rollback, zero-downtime cutover | Same |
| App container crash | Docker restart policy (`restart: unless-stopped`) | Swarm reschedules |
| Worker node lost | (n/a: one Server) | Stateless tasks move; Stateful Services unaffected |
| Server with Stateful Services lost | Down until rebuilt from Backup | Down until rebuilt from Backup |
| Backup Target on the lost Server | Data lost | Data lost |
