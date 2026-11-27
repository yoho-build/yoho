#!/bin/sh
# One entrypoint for db and db-standby. It runs as root, prepares password
# files from the Yoho secrets, then hands over to the image's entrypoint.
#
#  - PG_VERSION exists: start Postgres (primary or standby as on disk).
#  - empty data and YOHO_CONNECTION_PRIMARY_HOST set (a consumer of the
#    `primary` Connection): clone the primary with pg_basebackup.
#  - empty data otherwise: initdb as a new primary (the image does this).
#
# `sh /etc/yoho-pg/entrypoint.sh set-passwords` (through `yoho app exec` on
# the primary) sets both role passwords from the current secret files.
set -eu
if [ "${1:-}" = set-passwords ]; then
  # psql reads the values itself; they never appear in argv.
  psql -X -v ON_ERROR_STOP=1 -U postgres -d postgres <<'SQL'
\set rp `cat /run/secrets/REPLICATOR_PASSWORD`
\set pp `cat /run/secrets/POSTGRES_PASSWORD`
ALTER ROLE replicator PASSWORD :'rp';
ALTER ROLE postgres PASSWORD :'pp';
SQL
  exit 0
fi
PGDATA=${PGDATA:-/var/lib/postgresql/data}
home=/var/lib/postgresql
passfile=$home/.pgpass
as_pg() { if command -v gosu >/dev/null 2>&1; then gosu postgres "$@"; else su-exec postgres "$@"; fi; }
umask 077
# Rewritten on every start from the current secrets, outside PGDATA, so a
# password rotation needs only a restart and pg_basebackup never copies it.
esc() { sed -e 's/\\/\\\\/g' -e 's/:/\\:/g'; }
{
  printf '*:*:replication:replicator:'; esc < /run/secrets/REPLICATOR_PASSWORD; echo
  printf '*:*:*:postgres:'; esc < /run/secrets/POSTGRES_PASSWORD; echo
} > "$passfile"
cp /run/secrets/REPLICATOR_PASSWORD "$home/.yoho-replicator-password"
chown postgres:postgres "$passfile" "$home/.yoho-replicator-password"
chmod 600 "$passfile" "$home/.yoho-replicator-password"

host=${YOHO_CONNECTION_PRIMARY_HOST:-}
port=${YOHO_CONNECTION_PRIMARY_PORT:-5432}
conninfo="host=$host port=$port user=replicator passfile=$passfile application_name=${YOHO_SERVICE:-standby}"
set_conninfo() {
  # No password in postgresql.auto.conf: libpq reads it from the passfile.
  auto=$PGDATA/postgresql.auto.conf
  touch "$auto"
  grep -v '^primary_conninfo' "$auto" > "$auto.tmp" || true
  printf "primary_conninfo = '%s'\n" "$conninfo" >> "$auto.tmp"
  mv "$auto.tmp" "$auto"
  chown postgres:postgres "$auto"
  chmod 600 "$auto"
}

if [ -n "$host" ] && [ ! -s "$PGDATA/PG_VERSION" ]; then
  echo "yoho-pg: empty data directory; cloning primary $host:$port" >&2
  mkdir -p "$PGDATA"
  # A base backup that stopped halfway has no PG_VERSION; start clean.
  find "$PGDATA" -mindepth 1 -delete
  chown postgres:postgres "$PGDATA"
  chmod 700 "$PGDATA"
  until as_pg pg_isready -q -h "$host" -p "$port"; do sleep 2; done
  as_pg pg_basebackup -d "$conninfo" -D "$PGDATA" -R -X stream --no-password --checkpoint=fast
  set_conninfo
elif [ -n "$host" ] && [ -f "$PGDATA/standby.signal" ]; then
  # Follow the Connection if the primary moved.
  set_conninfo
elif [ -n "$host" ] && [ -s "$PGDATA/PG_VERSION" ]; then
  # A former primary that is now a consumer: never accept writes. Remove its
  # volume to rebuild it as a standby (see the promotion guide).
  echo "yoho-pg: former primary is a consumer of $host; starting read-only. Remove the volume to rebuild it as a standby." >&2
  set -- "$@" -c default_transaction_read_only=on
fi
exec docker-entrypoint.sh "$@"
