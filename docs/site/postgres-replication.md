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
2. Create `.yoho/secrets` from `.yoho/secrets.example` with long random
   values:

   ```sh
   POSTGRES_PASSWORD=...
   REPLICATOR_PASSWORD=...
   DEMO_WRITE_TOKEN=...
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

The `web` Service is a demo. It reads a row count at `/cgi-bin/read`. Its
write endpoint accepts only a POST whose body is `DEMO_WRITE_TOKEN`:

```sh
curl --data-binary @token-file https://pg.example.com/cgi-bin/write?hello
```

Replace it with your App, and give your App its own least-privilege role
instead of `postgres`.

## What protects the database

- The Connection firewall admits only consumer Servers to port 5432 on the
  primary's private address. Other hosts are refused, even when another rule
  accepts all forwarded traffic.
- `pg_hba.conf` requires `scram-sha-256` for every network login. It allows
  replication from any address, because the firewall is the network control.
  The same file then works on both nodes after a promotion.
- The data directory is `pgdata` inside the volume. A standby clone keeps a
  marker next to it until the clone is complete and configured. If the
  container stops mid-clone, the next start wipes the partial data and clones
  again.
- The replication password is in `/var/lib/postgresql/.pgpass`, outside the
  data directory. The entrypoint rewrites it from the secret files on every
  start. `primary_conninfo` names the `passfile`; it never holds a password.

## Promote the standby

Do this for a planned switchover, with both Servers up. Do the steps in order.
If Server `a` is lost, read [Server a is lost](#server-a-is-lost) first.

1. **Fence the old primary.** Stop new writes:

   ```sh
   yoho app exec db -- psql -U postgres -c "ALTER SYSTEM SET default_transaction_read_only = on"
   yoho app exec db -- psql -U postgres -c "SELECT pg_reload_conf()"
   yoho app exec db -- psql -U postgres -c "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE backend_type = 'client backend' AND pid <> pg_backend_pid()"
   ```

   For a planned switchover, wait until the standby has replayed everything.
   Compare `select pg_current_wal_lsn()` on `db` with
   `select pg_last_wal_replay_lsn()` on `db-standby`.

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

### Server a is lost

A Server you cannot reach over SSH is not fenced. It may still run and accept
writes from clients you cannot see. Before you promote, make sure the old
primary cannot accept writes: power Server `a` off in your provider's console
and confirm that it stays off. Detaching only its external network is not
enough, because `web` and the old primary may still write to each other on
Server `a`.

Then promote `db-standby` (step 2 above) and move everything to Server `b`.
In `yoho.yml`, remove `a` from the top-level `servers` (this also removes it
from the mesh) and from the Destination, drop the old Role, place `web` on `b`,
and leave `web` as the only consumer:

```yaml
servers:
  b: {ssh: root@203.0.113.11, private_address: 10.0.0.11}
destinations:
  production:
    servers: [b]
    roles:
      app: {services: [web], servers: [b]}
      standby: {services: [db-standby], servers: [b]}
    connections:
      primary: {service: db-standby, port: 5432, consumers: [web]}
```

Delete the `db` Service from `compose.yaml`, then run `yoho apply` (add
`--version` if the App directory is not a Git checkout). If DNS, a Cloudflare
Tunnel or a load balancer still sends traffic to Server `a`, point it at
Server `b`. The App now runs on one Server with no standby. To add a standby again, add a new
Server, a Service for it, and make it a consumer of `primary`. Never start the
old Server `a` with its data again; wipe its disk first.

## Rotate the passwords

**REPLICATOR_PASSWORD** (tested in CI):

1. Put the new value in `.yoho/secrets`.
2. Run `yoho deploy`. Both database Services restart with the new secret files,
   and the entrypoint rewrites `.pgpass`.
3. Set the new password on the primary:

   ```sh
   yoho app exec db -- sh /etc/yoho-pg/entrypoint.sh set-passwords
   ```

   psql reads the values from the secret files. They never appear in a command
   line. The standby retries and reconnects with the new password.

Between steps 2 and 3, the standby cannot log in and replication waits. Run
step 3 right after the deploy. After a promotion, run it on `db-standby`.

**POSTGRES_PASSWORD** (web uses it): change it in the database first, then
deploy, so web never holds a password the database rejects. With the new
value in the shell variable `NEW` (hex characters only):

```sh
printf "ALTER ROLE postgres PASSWORD '%s';\n" "$NEW" | yoho app exec -i db -- psql -U postgres -v ON_ERROR_STOP=1
```

`printf` is a shell builtin and the statement travels over stdin, so the value
is not in a command line. If psql reports an error, stop: do not change
`.yoho/secrets`. Then put the value in `.yoho/secrets` and run
`yoho deploy`. Connections that web opens before the deploy fail until it
finishes, so do this in a quiet moment. This order is not covered by CI.

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
