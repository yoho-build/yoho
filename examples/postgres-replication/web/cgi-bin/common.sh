# shellcheck shell=sh
# Sourced by the CGI scripts. The Connection env names the current primary.
export PGHOST="$YOHO_CONNECTION_PRIMARY_HOST" PGPORT="$YOHO_CONNECTION_PRIMARY_PORT" PGUSER=postgres PGDATABASE=app
PGPASSWORD=$(cat /run/secrets/POSTGRES_PASSWORD)
export PGPASSWORD PGCONNECT_TIMEOUT=3
reply() { printf 'Content-Type: text/plain\r\n\r\n%s\n' "$1"; }
fail() { printf 'Status: 503 Service Unavailable\r\nContent-Type: text/plain\r\n\r\n%s\n' "$1"; exit 0; }
