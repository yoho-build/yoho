#!/bin/sh
# Runs once, as mysql, while the image initializes an empty data directory
# (a temporary server on the local socket only). Passwords travel over stdin
# and in MYSQL_PWD, never in argv. The image sources this file when it is not
# executable, so the body runs in a subshell: its options and exits stay here.
(
  set -eu
  export MYSQL_PWD="$MARIADB_ROOT_PASSWORD"
  local_sql() { mariadb --protocol=socket -uroot; }
  # Runs SQL from stdin. On failure, prints only LABEL and the exit code: the
  # client may echo the statement, with its passwords, to stderr.
  run_sql() {
    err=$(mktemp)
    rc=0
    local_sql 2>"$err" || rc=$?
    rm -f "$err"
    if [ "$rc" -ne 0 ]; then
      echo "yoho-mariadb: $1 failed (exit $rc)" >&2
      exit "$rc"
    fi
  }
  q() { sed -e 's/\\/\\\\/g' -e "s/'/\\\\'/g"; }
  pwfile=/run/yoho-mariadb/replicator-password

  # Not binary logged: each node creates its own users and table at init.
  printf "SET SESSION sql_log_bin = 0;
  CREATE USER 'replicator'@'%%' IDENTIFIED BY '%s';
  GRANT REPLICATION SLAVE, BINLOG MONITOR, RELOAD ON *.* TO 'replicator'@'%%';
  GRANT SELECT, SHOW VIEW, TRIGGER, EVENT, LOCK TABLES ON app.* TO 'replicator'@'%%';
  CREATE TABLE IF NOT EXISTS app.items (id int AUTO_INCREMENT PRIMARY KEY, v varchar(255) NOT NULL, at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP);
  " "$(q < "$pwfile")" | run_sql "create replication user"

  host=${YOHO_CONNECTION_PRIMARY_HOST:-}
  [ -n "$host" ] || exit 0

  # A replica: copy the primary's app database at a consistent GTID position.
  port=${YOHO_CONNECTION_PRIMARY_PORT:-3306}
  case "$port" in ''|*[!0-9]*) echo "yoho-mariadb: bad port $port" >&2; exit 1;; esac
  rp=$(cat "$pwfile")
  until MYSQL_PWD=$rp mariadb -h "$host" -P "$port" -u replicator --connect-timeout=3 -e 'SELECT 1' >/dev/null 2>&1; do
    echo "yoho-mariadb: waiting for primary $host:$port" >&2
    sleep 2
  done
  dump=/var/lib/mysql/yoho-clone.sql
  umask 077
  MYSQL_PWD=$rp mariadb-dump -h "$host" -P "$port" -u replicator \
    --databases app --single-transaction --gtid --master-data=1 > "$dump"
  # RESET MASTER drops this node's own init events from its binary log, so its
  # GTID history is the primary's. The dump sets gtid_slave_pos.
  { printf 'RESET MASTER;\nSET SESSION sql_log_bin = 0;\n'; cat "$dump"; } | local_sql
  rm -f "$dump"
  printf "CHANGE MASTER TO MASTER_HOST='%s', MASTER_PORT=%s, MASTER_USER='replicator', MASTER_PASSWORD='%s', MASTER_USE_GTID=slave_pos, MASTER_CONNECT_RETRY=5;\n" \
    "$(printf '%s' "$host" | q)" "$port" "$(q < "$pwfile")" | run_sql "configure replication"
  rm -f /var/lib/mysql/yoho-clone-in-progress
  echo "yoho-mariadb: cloned primary $host:$port; replication starts with the server" >&2
)
