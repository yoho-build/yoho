# shellcheck shell=sh
# Sourced by the CGI scripts. The Connection env names the current primary.
DBHOST=$YOHO_CONNECTION_PRIMARY_HOST
# The client reads the password from MYSQL_PWD, so it is not in argv.
MYSQL_PWD=$(cat /run/secrets/MARIADB_PASSWORD)
export MYSQL_PWD
db() { mariadb -h "$DBHOST" -P "$YOHO_CONNECTION_PRIMARY_PORT" -u app --skip-ssl-verify-server-cert --connect-timeout=3 -N -B app -e "$1"; }
reply() { printf 'Content-Type: text/plain\r\n\r\n%s\n' "$1"; }
fail() { printf 'Status: 503 Service Unavailable\r\nContent-Type: text/plain\r\n\r\n%s\n' "$1"; exit 0; }
