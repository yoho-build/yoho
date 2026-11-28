# MariaDB primary and replica

This recipe runs a MariaDB primary on one Server and a replica on another,
with [Roles](roles.md). It is a recipe, not a Yoho feature. Yoho does not fail
over. You promote the replica by hand, with the steps below. CI runs this
exact example and every step on this page
(`test/integration/mysql-replication.sh`).

The files are in
[`examples/mysql-replication`](https://github.com/yoho-build/yoho/tree/main/examples/mysql-replication).

It uses MariaDB 11.4 LTS. MariaDB always writes GTIDs, so there is no
`gtid_mode` to switch on, and a replica resumes from its own
`gtid_slave_pos` with `MASTER_USE_GTID=slave_pos`. `mariadb-dump --gtid`
records that position in the copy it makes. MySQL 8.4 needs
`gtid_mode=ON` and `enforce_gtid_consistency=ON`, `SOURCE_AUTO_POSITION=1`,
`super_read_only`, and its own replica commands (`STOP REPLICA`,
`RESET REPLICA ALL`, `WAIT_FOR_EXECUTED_GTID_SET`). The structure of this
recipe carries over, but CI tests only MariaDB.

## How it works

- `db` (Server `a`) and `db-replica` (Server `b`) are two Stateful Services.
  Each has one Server, as [ADR 0005](https://github.com/yoho-build/yoho/blob/main/docs/adr/0005-stateful-services-are-pinned.md)
  requires. They use the same image, entrypoint and `replication.cnf`. The
  entrypoint gives each one its own `server_id`, from the Service name.
- The Connection `primary` decides which one is the primary:

  ```yaml
  connections:
    primary: {service: db, port: 3306, consumers: [web, db-replica]}
  ```

- A consumer of `primary` with an empty data directory initializes, copies
  the primary's `app` database with `mariadb-dump --single-transaction --gtid`,
  and replicates from the position in that copy. It runs read-only. A Service
  that is not a consumer and has an empty data directory initializes as the
  primary.
- `web` connects to `YOHO_CONNECTION_PRIMARY_HOST` and
  `YOHO_CONNECTION_PRIMARY_PORT`, not to the name `db`. After a promotion the
  same variables point at the new primary.
- Replication is asynchronous. A write the primary acknowledged can be lost if
  the primary's Server dies before the replica received it. For fewer lost
  writes, enable semi-synchronous replication (`rpl_semi_sync_master_enabled`
  on the primary, `rpl_semi_sync_slave_enabled` on the replica); then each
  commit waits until the replica has received it.

## Set it up

1. Copy the example directory. In `yoho.yml`, set each Server's `ssh` and
   `private_address` (or use the [WireGuard mesh](mesh.md)). In `compose.yaml`,
   set the Proxy host of `web`.
2. Create `.yoho/secrets` from `.yoho/secrets.example` with long random
   values:

   ```sh
   MARIADB_ROOT_PASSWORD=...
   MARIADB_PASSWORD=...
   REPLICATOR_PASSWORD=...
   DEMO_WRITE_TOKEN=...
   ```

   These are user secrets, not `x-yoho.generate`. A generated secret has one
   owning Service ([ADR 0019](https://github.com/yoho-build/yoho/blob/main/docs/adr/0019-roles-safety.md)).
   After a promotion that owner would be a read-only replica, and rotation
   would fail. Both database Services receive all three passwords as files.
   `MARIADB_PASSWORD` is the password of the `app` user that `web` uses.
   Each password must be one line: the entrypoint writes them into SQL, one
   statement per line. A single trailing newline in a secret file is ignored
   (the shell and the image's `*_FILE` reading strip it the same way); any
   other newline or a carriage return makes the entrypoint refuse to start,
   naming the secret but never its value.
3. Run `yoho deploy`. Yoho starts `db` first, then `db-replica` (provider
   before consumer). The replica copies the primary and starts replicating.

Check replication on the replica:

```sh
yoho app exec db-replica -- sh /etc/yoho-mariadb/entrypoint.sh mariadb -e "SHOW REPLICA STATUS\G"
```

`Slave_IO_Running` and `Slave_SQL_Running` should both be `Yes`. The
entrypoint's `mariadb` command runs the client as root on the local socket and
reads the root password from its secret file.

The `web` Service is a demo. It reads a row count at `/cgi-bin/read`. Its
write endpoint accepts only a POST whose body is `DEMO_WRITE_TOKEN`:

```sh
curl --data-binary @token-file https://mariadb.example.com/cgi-bin/write?hello
```

Replace it with your App. The `app` user has privileges on the `app` database
only, so `read_only` stops its writes.

## What protects the database

- The Connection firewall admits only consumer Servers to port 3306 on the
  primary's private address. Other hosts are refused, even when another rule
  accepts all forwarded traffic.
- `root` exists only as `root@localhost`. The `replicator` user can replicate
  and read the `app` database, for the copy; nothing else. Every network login
  needs a password.
- The replica runs with `read_only`. Replication still applies changes; the
  `app` user cannot write.
- A replica copy keeps a marker in the data directory until it is complete and
  configured. If the container stops mid-copy, the next start wipes the
  partial data and copies again.
- MariaDB stores the replication password in `master.info` in the replica's
  data directory, readable only by the `mysql` user. The entrypoint updates it
  from the secret file on every start. No password is in a command line. A
  failed statement that carries a password (creating the `replicator` user,
  `CHANGE MASTER TO`, `set-passwords`) prints only its name and exit code,
  never the client's error, which can quote the statement.
- The copy holds the `app` database only. Users are created on each node from
  the secrets at initialization. If your App uses stored routines, grant
  `replicator` `SHOW CREATE ROUTINE` on `app` and add `--routines` to the
  `mariadb-dump` line in `initdb/10-replication.sh`.

## Promote the replica

Do this for a planned switchover, with both Servers up. Do the steps in order.
If Server `a` is lost, read [Server a is lost](#server-a-is-lost) first.

1. **Fence the old primary.** Stop new writes:

   ```sh
   yoho app exec db -- sh /etc/yoho-mariadb/entrypoint.sh fence
   ```

   This ends the App's sessions, sets `read_only`, and leaves a marker so `db`
   also starts read-only after a restart. Then read the primary's last GTID:

   ```sh
   yoho app exec db -- sh /etc/yoho-mariadb/entrypoint.sh gtid
   ```

2. **Promote.** Pass that GTID position:

   ```sh
   yoho app exec db-replica -- sh /etc/yoho-mariadb/entrypoint.sh promote 0-2100691912-42
   ```

   It waits up to 120 seconds until the replica has applied that position
   (`MASTER_GTID_WAIT`) and refuses to promote if it has not. Then it runs
   `STOP SLAVE`, `RESET SLAVE ALL` and `SET GLOBAL read_only = OFF`. Do
   steps 3 and 4 right away: until then `db-replica` is still a consumer, and
   a restart brings it back read-only.
3. **Switch the Connection.** In `yoho.yml`:

   ```yaml
   primary: {service: db-replica, port: 3306, consumers: [web, db]}
   ```

4. **Apply.** Run `yoho apply`. `web` now connects to `db-replica`. The
   firewall now protects port 3306 on Server `b`. The old `db` restarts
   read-only: it is fenced, and its entrypoint sees a consumer with old
   primary data.
5. **Rebuild the old primary as a replica.** Yoho never deletes volumes, so
   remove the old data by hand on Server `a`:

   ```sh
   id=$(docker ps -q --filter label=com.docker.compose.project=yoho-mariadb-production --filter label=com.docker.compose.service=db)
   docker rm -f "$id"
   docker volume rm yoho-mariadb-production_mariadb
   ```

   Check the volume name with `docker volume ls` first. Then run `yoho apply`
   again. `db` starts with an empty data directory as a consumer, and copies
   `db-replica`.

The roles are now reversed. To switch back later, repeat the steps with the
names swapped.

### Server a is lost

A Server you cannot reach over SSH is not fenced. It may still run and accept
writes from clients you cannot see. Before you promote, make sure the old
primary cannot accept writes: power Server `a` off in your provider's console
and confirm that it stays off. Detaching only its external network is not
enough, because `web` and the old primary may still write to each other on
Server `a`.

Then promote `db-replica` without a GTID (`entrypoint.sh promote --no-wait`;
there is no primary to compare with, and a plain `promote` needs a GTID) and
move everything to Server `b`. In `yoho.yml`, remove `a` from the top-level `servers` (this also removes it from the mesh)
and from the Destination, drop the old Role, place `web` on `b`, and leave
`web` as the only consumer:

```yaml
servers:
  b: {ssh: root@203.0.113.11, private_address: 10.0.0.11}
destinations:
  production:
    servers: [b]
    roles:
      app: {services: [web], servers: [b]}
      replica: {services: [db-replica], servers: [b]}
    connections:
      primary: {service: db-replica, port: 3306, consumers: [web]}
```

Delete the `db` Service from `compose.yaml`, then run `yoho apply` (add
`--version` if the App directory is not a Git checkout). If DNS, a Cloudflare
Tunnel or a load balancer still sends traffic to Server `a`, point it at
Server `b`. The App now runs on one Server with no replica. To add a replica
again, add a new Server, a Service for it, and make it a consumer of
`primary`. Never start the old Server `a` with its data again; wipe its disk
first.

## Rotate the passwords

**REPLICATOR_PASSWORD** (tested in CI):

1. Put the new value in `.yoho/secrets`.
2. Run `yoho deploy`. Both database Services restart with the new secret
   files, and the replica's entrypoint gives the new password to
   `CHANGE MASTER TO`.
3. Set the new password on the primary:

   ```sh
   yoho app exec db -- sh /etc/yoho-mariadb/entrypoint.sh set-passwords
   ```

   It sets the `replicator` and `app` passwords from the secret files, over
   stdin. The change is replicated, so the replica has the same users. The
   replica retries and reconnects with the new password.

Between steps 2 and 3, the replica cannot log in and replication waits. Run
step 3 right after the deploy. After a promotion, run it on `db-replica`.

**MARIADB_PASSWORD** (web uses it, tested in CI) and
**MARIADB_ROOT_PASSWORD**: put the new value in `.yoho/secrets` and run
`yoho deploy`. The image reads the secret files only when it initializes an
empty data directory, so on every start the entrypoint passes the server an
`--init-file` that sets the `app` and `root@localhost` passwords from the
secret files before it serves clients. The init file runs as the server
itself, so a changed root secret cannot lock the entrypoint out. It is not
binary logged: each node sets its own users when it restarts, and the deploy
restarts both, before `web`, because their secrets changed. The init file is
in the container's `/run/yoho-mariadb`, readable only by `mysql`. Root
rotation is not covered by CI.

## Backups

Replication copies mistakes too: a dropped table is dropped on the replica.
Keep off-Server [Backups](backups.md) of the primary. After a promotion, move
the Backup job to the Service that is now the primary.

## Limits

- Yoho does not detect a failed primary and does not promote anything.
  Automatic failover needs a resident leader election, which
  [ADR 0003](https://github.com/yoho-build/yoho/blob/main/docs/adr/0003-no-resident-daemon-by-default.md)
  rules out.
- One replica. More replicas work the same way: add a Service, a Role and a
  consumer.
- Binary logs are kept for a week (`replication.cnf`). A replica that was down
  longer cannot catch up; rebuild it (step 5).
- A planned helper (`yoho db promote`) is in the backlog.
