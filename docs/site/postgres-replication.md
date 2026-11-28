# Postgres primary and standby

This recipe runs a Postgres primary on one Server and a streaming standby on
another, with [Roles](roles.md). It is a recipe, not a Yoho feature. Yoho does
not fail over. You promote the standby when you decide to, with
`yoho db promote`. CI runs
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

`yoho db promote` does a planned switchover, with both Servers up. It is
config first: you edit the Connection, and it does the rest.

1. **Switch the Connection.** In `yoho.yml`, make the standby the provider and
   the old primary a consumer:

   ```yaml
   primary: {service: db-standby, port: 5432, consumers: [web, db]}
   ```

   Do not run `yoho apply` yet: `yoho plan` names `yoho db promote` for this
   change.

2. **Promote.**

   ```sh
   yoho db promote
   ```

   It reads the old primary (`db`) from the current Release and checks,
   read-only, that `db-standby` is streaming from the same cluster (same
   `system_identifier`) and at most `--max-lag` (16MiB) behind. It shows the plan and asks `yes` (`--auto-approve` skips the
   question, `--plan` stops there). Then it:

   - fences `db` with `yoho app stop db`: its container is stopped and
     removed, and it stays stopped through every deploy until
     `yoho app start db`. Between stop and removal it runs `pg_controldata`
     on `db`'s data directory, which must say `shut down`; the latest
     checkpoint location (the shutdown checkpoint, the end of `db`'s WAL) is
     the target;
   - waits until `db-standby` has replayed the target;
   - runs `SELECT pg_promote(true, 60)` on `db-standby`;
   - runs `yoho deploy`. `web` now connects to `db-standby`, and the
     firewall now protects port 5432 on Server `b`.

   If it stops before the promotion, nothing was promoted and `db` is
   stopped: `yoho app start db` restores the old primary, or rerun
   `yoho db promote` to continue. Every step is journaled on Server `b`, and
   a rerun resumes. A rerun checks `db`'s Server again: if you started `db`,
   it is fenced again. If the promotion worked but the deploy failed, a
   rerun resumes at the deploy. The journal is deleted once the deploy
   succeeds.

3. **Rebuild the old primary as a standby.** Yoho never deletes volumes, so
   remove the old data by hand on Server `a` (`yoho db promote` prints the
   volume name; check it with `docker volume ls`):

   ```sh
   docker volume rm yoho-pg-production_pgdata
   ```

   Then run `yoho app start db`. `db` starts with an empty data directory as
   a consumer, and clones `db-standby`.

The roles are now reversed. To switch back later, repeat the steps with the
names swapped.

Replication is asynchronous. The fence is a clean shutdown of `db`, which
sends its last WAL to the standby before it exits; a container that does not
exit 0, or a data directory not `shut down`, stops the promotion, because its
last writes may not have reached the standby. The journal remembers it: a
rerun refuses until you run `yoho app start db` (and promote again later), or
pass `--old-primary-down` to promote at the position `db-standby` received and
accept that writes it did not receive are lost. psql runs in the containers over the local socket as `POSTGRES_USER`
(from the Service's tag environment, Compose environment or secrets) or
`--user`, so `pg_hba.conf` needs a `local` rule for that role, as the
example's has.

### By hand

Without `yoho db promote` (CI tests the command, not these steps):

1. Fence: `yoho app stop db`. For a planned switchover, first compare
   `select pg_current_wal_lsn()` on `db` with
   `select pg_last_wal_replay_lsn()` on `db-standby`.
2. Promote: `yoho app exec db-standby -- psql -U postgres -c "SELECT pg_promote()"`.
   Do not run `pg_ctl promote`: it cannot run as root.
3. Switch the Connection as above and run `yoho apply`. `db` stays stopped.
4. Rebuild: remove the volume and `yoho app start db`, as above.

### Server a is lost

A Server you cannot reach over SSH is not fenced. It may still run and accept
writes from clients you cannot see. Before you promote, make sure the old
primary cannot accept writes: power Server `a` off in your provider's console
and confirm that it stays off. Detaching only its external network is not
enough, because `web` and the old primary may still write to each other on
Server `a`.

Then move everything to Server `b`:

1. In `yoho.yml`, remove `a` from the top-level `servers` (this also removes
   it from the mesh) and from the Destination's `servers`.
2. Drop the old primary's Role, place `web` on `b`, make `db-standby` the
   Connection's provider, and leave `web` as the only consumer. `db` cannot
   be a consumer: it has no Server any more.

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

3. Delete the `db` Service from `compose.yaml`.
4. Run:

   ```sh
   yoho db promote --old-primary-down
   ```

   Add `--version` if the App directory is not a Git checkout. It reads the
   old primary from the Release on Server `b`. There is no fence:
   `db-standby` is promoted once its replay is stable and matches what it
   received (or its WAL receiver stopped), and writes it did not receive are
   lost. Then it deploys the edited Yoho file to Server `b`.

Without `--old-primary-down`, a promotion whose old primary is not on a
Server of the Destination is refused; with it, a promotion whose old
primary's Server is still in the Destination is refused. If DNS, a Cloudflare Tunnel
or a load balancer still sends traffic to Server `a`, point it at Server `b`.
The App now runs on one Server with no standby. To add a standby again, add a
new Server, a Service for it, and make it a consumer of `primary`. Never start
the old Server `a` with its data again; wipe its disk first.

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
- `yoho db promote` supports Postgres only, and rebuilds the old primary with a
  full base backup (no pg_rewind).
