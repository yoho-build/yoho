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
   Volumes are kept. Deploy, apply and rollback leave a stopped Service down:
   they pass a derived Compose override (`stopped.compose.yaml`, zero
   replicas, only Services of the Release being run) and skip its release
   command and readiness wait. Release files never change. `yoho plan` shows
   the Service as stopped, with who stopped it. Only `yoho app start SERVICE`
   ends the state: it removes the record and starts the Service from the
   current Release. Stop and start cover Services without `x-yoho.proxy` on
   the compose runtime, with operator access.

2. **`yoho db promote` is config first.** The operator edits the Connection so
   the standby is its provider and the old primary a consumer; promote reads
   the old primary from the current Release. It is Postgres only and manual:
   it runs only when the operator runs it. It refuses to promote without a
   fence: the old primary is stopped with `yoho app stop` after its WAL
   position is recorded, and a container that does not exit 0 stops the
   promotion. The only way past the fence is `--old-primary-down`, for a
   Server the operator powered off and removed from the Destination. Each
   step is journaled on the new primary's Server, so a rerun resumes and a
   rerun after success is a no-op. It never deletes volumes and never
   rewrites the Yoho file. It ends with the ordinary deploy of the Yoho file,
   which moves the Connection, its firewall and its consumers.

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
- Stopping a proxied Service (removing its route and keeping it down) and
  stop on runtime swarm are future work; today they use `deploy.replicas: 0`.
