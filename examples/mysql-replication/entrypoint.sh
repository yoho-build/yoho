#!/bin/sh
# One entrypoint for db and db-replica. It runs as root, prepares the
# replication settings from the Yoho secrets, then hands over to the image's
# entrypoint.
#
#  - empty data and YOHO_CONNECTION_PRIMARY_HOST set (a consumer of the
#    `primary` Connection): initialize, copy the primary's `app` database with
#    mariadb-dump and replicate it with GTID (initdb/10-replication.sh).
#  - a replica (master.info exists) that is a consumer: follow the Connection
#    host with the current REPLICATOR_PASSWORD, read-only.
#  - a former primary that is now a consumer: start read-only, no replication.
#  - otherwise: a primary (the image initializes empty data).
#
# Admin commands, run with `yoho app exec SERVICE -- sh /etc/yoho-mariadb/entrypoint.sh CMD`:
#   mariadb [ARGS]  the mariadb client as root on the local socket
#   set-passwords   primary: set the replicator and app passwords from the secret files
#   fence           primary: read-only now and after a restart; end App sessions
#   gtid            primary: print the binary log GTID position
#   promote [GTID]  replica: wait until GTID is applied, stop replicating, accept writes
set -eu
datadir=/var/lib/mysql
run=/run/yoho-mariadb
fenced=$datadir/yoho-fenced
marker=$datadir/yoho-clone-in-progress
# SQL string literal escaping. Values travel over stdin, never in argv.
q() { sed -e 's/\\/\\\\/g' -e "s/'/\\\\'/g"; }
# root@localhost over the socket; the client reads the password from MYSQL_PWD.
root_pw() { MYSQL_PWD=$(cat /run/secrets/MARIADB_ROOT_PASSWORD); export MYSQL_PWD; }
sql() { root_pw; mariadb --protocol=socket -uroot -N -B "$@"; }

case "${1:-}" in
mariadb)
  shift
  root_pw
  exec mariadb --protocol=socket -uroot "$@";;
set-passwords)
  rp=$(q < /run/secrets/REPLICATOR_PASSWORD)
  ap=$(q < /run/secrets/MARIADB_PASSWORD)
  # Binary logged, so the replica gets the same passwords.
  printf "ALTER USER 'replicator'@'%%' IDENTIFIED BY '%s';\nALTER USER 'app'@'%%' IDENTIFIED BY '%s';\n" "$rp" "$ap" | sql
  exit 0;;
fence)
  touch "$fenced"
  chown mysql:mysql "$fenced"
  kill_sessions() {
    sql -e "SELECT id FROM information_schema.processlist WHERE user NOT IN ('root', 'replicator', 'healthcheck', 'system user', 'event_scheduler', 'unauthenticated user') AND id <> CONNECTION_ID()" |
      while read -r id; do echo "KILL $id;" | sql 2>/dev/null || true; done
  }
  # End sessions first so an open transaction cannot hold up read_only.
  kill_sessions
  printf 'SET SESSION lock_wait_timeout = 30;\nSET GLOBAL read_only = ON;\n' | sql
  kill_sessions
  exit 0;;
gtid)
  sql -e 'SELECT @@gtid_binlog_pos'
  exit 0;;
promote)
  if [ -n "${2:-}" ]; then
    case "$2" in *[!0-9,-]*) echo "yoho-mariadb: bad GTID position $2" >&2; exit 2;; esac
    if [ "$(sql -e "SELECT MASTER_GTID_WAIT('$2', 120)")" != 0 ]; then
      echo "yoho-mariadb: the replica has not applied $2 within 120s; not promoting" >&2
      exit 1
    fi
  fi
  printf 'STOP SLAVE;\nRESET SLAVE ALL;\nSET GLOBAL read_only = OFF;\n' | sql
  rm -f "$fenced"
  exit 0;;
esac

umask 077
mkdir -p "$run"
cp /run/secrets/REPLICATOR_PASSWORD "$run/replicator-password"
chown -R mysql:mysql "$run"

host=${YOHO_CONNECTION_PRIMARY_HOST:-}
port=${YOHO_CONNECTION_PRIMARY_PORT:-3306}
# Every node needs its own server_id; derive it from the Service name.
sid=$(printf '%s' "${YOHO_SERVICE:-db}" | cksum | cut -d' ' -f1)
[ "$sid" -gt 0 ] || sid=1
set -- "$@" --server-id="$sid"

# The marker exists from the start of a clone until the replica is
# configured, so an interrupted clone is wiped and cloned again.
if [ -n "$host" ] && { [ ! -d "$datadir/mysql" ] || [ -e "$marker" ]; }; then
  echo "yoho-mariadb: empty data directory; cloning primary $host:$port" >&2
  find "$datadir" -mindepth 1 -delete
  touch "$marker"
  chown mysql:mysql "$marker"
  set -- "$@" --read-only
elif [ -n "$host" ] && [ -f "$datadir/master.info" ]; then
  # Follow the Connection if the primary moved, and pick up a new
  # REPLICATOR_PASSWORD. The file is rewritten on every start.
  h=$(printf '%s' "$host" | q)
  p=$(q < /run/secrets/REPLICATOR_PASSWORD)
  case "$port" in ''|*[!0-9]*) echo "yoho-mariadb: bad port $port" >&2; exit 1;; esac
  printf "CHANGE MASTER TO MASTER_HOST='%s', MASTER_PORT=%s, MASTER_USER='replicator', MASTER_PASSWORD='%s', MASTER_USE_GTID=slave_pos, MASTER_CONNECT_RETRY=5;\nSTART SLAVE;\n" "$h" "$port" "$p" > "$run/follow.sql"
  chown mysql:mysql "$run/follow.sql"
  set -- "$@" --read-only --skip-slave-start --init-file="$run/follow.sql"
elif [ -n "$host" ]; then
  # A former primary that is now a consumer: never accept writes. Remove its
  # volume to rebuild it as a replica (see the promotion guide).
  echo "yoho-mariadb: former primary is a consumer of $host; starting read-only. Remove the volume to rebuild it as a replica." >&2
  set -- "$@" --read-only
fi
if [ -e "$fenced" ]; then
  echo "yoho-mariadb: fenced; starting read-only" >&2
  set -- "$@" --read-only
fi
exec docker-entrypoint.sh "$@"
