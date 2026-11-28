# shellcheck shell=sh
# Sourced by the CGI scripts. The Connection env names the current primary.
DBHOST=$YOHO_CONNECTION_PRIMARY_HOST
# The client reads the password from MYSQL_PWD, so it is not in argv.
MYSQL_PWD=$(cat /run/secrets/MARIADB_PASSWORD)
export MYSQL_PWD
db() { mariadb -h "$DBHOST" -P "$YOHO_CONNECTION_PRIMARY_PORT" -u app --connect-timeout=3 -N -B app -e "$1"; }
reply() { printf 'Content-Type: text/plain\r\n\r\n%s\n' "$1"; }
fail() { printf 'Status: 503 Service Unavailable\r\nContent-Type: text/plain\r\n\r\n%s\n' "$1"; exit 0; }
# query SQL: sets $out to the result. Client warnings (stderr) stay out of a
# successful reply; on failure they are the error message.
query() {
  err=$(mktemp) || fail "mktemp failed"
  # shellcheck disable=SC2034 # $out is read by the sourcing script
  if out=$(db "$1" 2>"$err"); then
    rm -f "$err"
    return 0
  fi
  msg=$(cat "$err")
  rm -f "$err"
  fail "${msg:-query failed}"
}
