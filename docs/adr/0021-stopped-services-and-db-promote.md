# Stopped Services and manual database promotion

Status: Accepted. A bounded exception to ADR 0008.

## Context

ADR 0008 makes the Yoho file and compose files the desired state: apply
converges everything to them. Two operations do not fit that model. Fencing a
database (stopping it so it cannot accept writes) must survive the next
deploy, or a routine `yoho deploy` would restart a fenced primary next to a
promoted standby. And promoting a Postgres standby is an ordered, one-time
procedure (fence, wait for replay, promote, switch the Connection) that the
[Postgres primary and standby](../site/postgres-replication.md) recipe did by
hand with `ALTER SYSTEM`, `pg_promote()` and an edited Connection.

## Decision

1. **`yoho app stop` is Server state.** `yoho app stop SERVICE` records the
   Service in `stopped.json` in the App directory on each Server hosting it
   (who, when, why), then stops and removes its containers. Removal, not only
   stop, keeps `restart: always` and a Docker restart from reviving them.
   It touches only containers labeled with this App and Destination. Volumes
   are kept. Deploy, apply and rollback leave a stopped Service down:
   they pass a derived Compose override (`stopped.compose.yaml`, zero
   replicas, only Services of the Release being run) and skip its release
   command and readiness wait. Release files never change. `yoho plan` shows
   the Service as stopped, with who stopped it; a config change of a stopped
   Service is still a change, so apply records the new Release and the
   Service stays stopped. Only `yoho app start SERVICE` ends the state: after
   the same ownership check as deploy, it starts the Service from the current
   Release and removes the record once Compose started it; a failed start
   removes the containers again and keeps the record. Stop and start cover
   Services without `x-yoho.proxy` on the compose runtime, with operator
   access; a Release that proxies a stopped Service is refused until it is
   started, and `app start` of a Service no longer in the Release clears its
   orphaned record.

2. **`yoho db promote` is config first.** The operator edits the Connection so
   the standby is its provider and the old primary a consumer; promote reads
   the old primary from the current Release. It is Postgres only and manual:
   it runs only when the operator runs it. Preflight checks that the standby
   streams from the old primary, belongs to its cluster (same
   `system_identifier`) and receives its current timeline; the checks run
   again under the locks before the fence. It refuses to promote without a fence: the old
   primary is stopped with `yoho app stop`, then, before its container is
   removed, `pg_controldata` on its data directory must report `shut down`;
   its latest checkpoint (the shutdown checkpoint, the end of its WAL) is the
   position the standby must replay. A container that does not exit 0 or a
   cluster not shut down stops the promotion, and the journal remembers it:
   a rerun refuses until the operator accepts the loss with
   `--old-primary-down`. Otherwise `--old-primary-down` is only for a Server
   the operator powered off and removed from the Destination. Each step is
   journaled on the new primary's Server, so a rerun resumes; the fence is
   re-verified from Server state on every run against the stop record the
   journal recorded (a restarted old primary is fenced again; one started
   and stopped again, or whose containers an earlier attempt removed before
   the check was journaled, is unclean), and a promoted journal whose new
   primary is in recovery is stale. The journal is only a retry marker: it is deleted after the
   follow-up apply succeeds, and a rerun of a promoted journal resumes at the
   apply. It never deletes volumes and never rewrites the Yoho file. It ends
   with the ordinary deploy of the Yoho file, which moves the Connection, its
   firewall and its consumers.

## Consequences

- There is still no automatic failover (ADR 0003): nothing watches the
  primary, and promote does not run unless an operator runs it.
- A Stateful Service still has one owner (ADR 0005). Promote changes which
  Service is a Connection's provider; it moves no Service and no data. The old
  primary is rebuilt as a standby only after its volume is removed by hand and
  it is started with `yoho app start`.
- Server state now outlives deploys in one bounded place: `stopped.json`. A
  plan that shows "No changes" can still list stopped Services. A stopped
  Connection provider makes its consumers' deploys fail with a message naming
  it; `yoho doctor` warns instead of checking its bindings, and `service move`
  refuses it.
- Replication stays asynchronous. The fence relies on a clean shutdown sending
  the last WAL to the standby; `--old-primary-down` loses what the standby did
  not receive.
- Promote checks `system_identifier` and timeline, but not the standby's
  upstream host or full WAL history, so a standby following a different
  branch with the same timeline ID (two copies promoted independently) is not
  detected; the recipe's single Connection makes that require manual
  misconfiguration.
- Stopping a proxied Service (removing its route and keeping it down) and
  stop on runtime swarm are future work; today they use `deploy.replicas: 0`.
