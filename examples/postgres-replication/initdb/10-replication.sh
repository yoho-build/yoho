#!/bin/sh
# Runs once, as postgres, when the primary initializes an empty data directory.
# The password is read by psql itself; it never appears in argv.
set -eu
psql -v ON_ERROR_STOP=1 --username postgres --dbname "${POSTGRES_DB:-postgres}" <<'SQL'
\set pw `cat /var/lib/postgresql/.yoho-replicator-password`
CREATE ROLE replicator WITH REPLICATION LOGIN PASSWORD :'pw';
CREATE TABLE IF NOT EXISTS items (id serial PRIMARY KEY, v text NOT NULL, at timestamptz NOT NULL DEFAULT now());
SQL
