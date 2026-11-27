# Postgres primary and standby

This recipe runs a Postgres primary on one Server and a streaming standby on
another, with [Roles](roles.md). It is a recipe, not a Yoho feature. Yoho does
not fail over. You promote the standby by hand, with the steps below. CI runs
this exact example and every step on this page
(`test/integration/pg-replication.sh`).

The files are in
[`examples/postgres-replication`](https://github.com/yoho-build/yoho/tree/main/examples/postgres-replication).

## How it works

- `db` (Server `a`) and `db-standby` (Server `b`) are two Stateful Services.
  Each has one Server, as [ADR 0005](https://github.com/yoho-build/yoho/blob/main/docs/adr/0005-stateful-services-are-pinned.md)
  requires. They use the same image, entrypoint and `pg_hba.conf`.
- The Connection `primary` decides which one is the primary:

  ```yaml
  connections:
    primary: {service: db, port: 5432, consumers: [web, db-standby]}
  ```

- A consumer of `primary` with an empty data directory clones the primary with
  `pg_basebackup` and becomes a standby. A Service that is not a consumer and
  has an empty data directory runs `initdb` and becomes the primary.
- `web` connects to `YOHO_CONNECTION_PRIMARY_HOST` and
  `YOHO_CONNECTION_PRIMARY_PORT`, not to the name `db`. After a promotion the
  same variables point at the new primary.
- Replication is asynchronous. A write the primary acknowledged can be lost if
  the primary's Server dies before the standby received it. For synchronous
  replication, set `synchronous_standby_names` on the primary; then writes stop
  when the standby is down.

## Set it up

1. Copy the example directory. In `yoho.yml`, set each Server's `ssh` and
   `private_address` (or use the [WireGuard mesh](mesh.md)). In `compose.yaml`,
   set the Proxy host of `web`.
2. Create `.yoho/secrets` from `.yoho/secrets.example` with two long random
   passwords:

   ```sh
   POSTGRES_PASSWORD=...
   REPLICATOR_PASSWORD=...
   ```

   These are user secrets, not `x-yoho.generate`. A generated secret has one
   owning Service ([ADR 0019](https://github.com/yoho-build/yoho/blob/main/docs/adr/0019-roles-safety.md)).
   After a promotion that owner would be a read-only standby, and rotation
   would fail. Both database Services receive both secrets as files.
3. Run `yoho deploy`. Yoho starts `db` first, then `db-standby` (provider
   before consumer). The standby clones the primary and starts streaming.

Check replication from the primary:

```sh
yoho app exec db -- psql -U postgres -c "select application_name, state from pg_stat_replication"
```

`db-standby` should show `streaming`.

## What protects the database

- The Connection firewall admits only consumer Servers to port 5432 on the
  primary's private address. Other hosts are refused, even when another rule
  accepts all forwarded traffic.
- `pg_hba.conf` requires `scram-sha-256` for every network login. It allows
  replication from any address, because the firewall is the network control.
  The same file then works on both nodes after a promotion.
- The replication password is in `/var/lib/postgresql/.pgpass`, outside the
  data directory. The entrypoint rewrites it from the secret files on every
  start. `primary_conninfo` names the `passfile`; it never holds a password.

## Promote the standby

Do this when the primary's Server is lost, or for a planned switchover. Do the
steps in order.

1. **Fence the old primary.** If Server `a` is still up, stop new writes:

   ```sh
   yoho app exec db -- psql -U postgres -c "ALTER SYSTEM SET default_transaction_read_only = on"
   yoho app exec db -- psql -U postgres -c "SELECT pg_reload_conf()"
   yoho app exec db -- psql -U postgres -c "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE backend_type = 'client backend' AND pid <> pg_backend_pid()"
   ```

   For a planned switchover, wait until the standby has replayed everything.
   Compare `select pg_current_wal_lsn()` on `db` with
   `select pg_last_wal_replay_lsn()` on `db-standby`.

   If Server `a` is lost, it is already fenced. Remove `a` from the
   Destination, and remove `db` from its Role and from `compose.yaml`. Never
   let a lost primary come back as a primary.
2. **Promote.**

   ```sh
   yoho app exec db-standby -- psql -U postgres -c "SELECT pg_promote()"
   ```

   Do not run `pg_ctl promote`: it cannot run as root.
3. **Switch the Connection.** In `yoho.yml`:

   ```yaml
   primary: {service: db-standby, port: 5432, consumers: [web, db]}
   ```

4. **Apply.** Run `yoho apply`. `web` now connects to `db-standby`. The
   firewall now protects port 5432 on Server `b`. The old `db` restarts
   read-only: its entrypoint sees that it is a consumer with old primary data.
5. **Rebuild the old primary as a standby.** Yoho never deletes volumes, so
   remove the old data by hand on Server `a`:

   ```sh
   id=$(docker ps -q --filter label=com.docker.compose.project=yoho-pg-production --filter label=com.docker.compose.service=db)
   docker rm -f "$id"
   docker volume rm yoho-pg-production_pgdata
   ```

   Check the volume name with `docker volume ls` first. Then run `yoho apply`
   again. `db` starts with an empty data directory as a consumer, and clones
   `db-standby`.

The roles are now reversed. To switch back later, repeat the steps with the
names swapped.

## Rotate the passwords

1. Put the new value in `.yoho/secrets`.
2. Run `yoho deploy`. Both database Services restart with the new secret files,
   and the entrypoint rewrites `.pgpass`.
3. Set the new passwords on the primary:

   ```sh
   yoho app exec db -- sh /etc/yoho-pg/entrypoint.sh set-passwords
   ```

   psql reads the values from the secret files. They never appear in a command
   line. The standby retries and reconnects with the new password.

Between steps 2 and 3, the standby cannot log in and replication waits. Run
step 3 right after the deploy. After a promotion, run it on `db-standby`.

## Backups

Replication copies mistakes too: a dropped table is dropped on the standby.
Keep off-Server [Backups](backups.md) of the primary. After a promotion, move
the Backup job to the Service that is now the primary.

## Limits

- Yoho does not detect a failed primary and does not promote anything. Automatic
  failover needs a resident leader election, which
  [ADR 0003](https://github.com/yoho-build/yoho/blob/main/docs/adr/0003-no-resident-daemon-by-default.md)
  rules out.
- One standby. More standbys work the same way: add a Service, a Role and a
  consumer.
- A planned helper (`yoho db promote`) is in the backlog.
