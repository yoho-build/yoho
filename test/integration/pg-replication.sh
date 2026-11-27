#!/usr/bin/env bash
# Deploys examples/postgres-replication as shipped (only Server addresses, the
# secrets and the Proxy host are filled in) to two nested SSH + Docker Servers,
# then walks the documented procedures: password rotation and promotion.
# See docs/site/postgres-replication.md.
set -euo pipefail
repo=$(pwd)
prefix="yoho-pg-ci-$$"
server_a="$prefix-a"
server_b="$prefix-b"
outsider="$prefix-outsider"
project=yoho-pg-production
servers_extra_diagnose() {
  local id
  for id in $(docker exec "$1" docker ps -aq --filter "label=com.docker.compose.project=$project"); do
    docker exec "$1" docker logs --tail 40 "$id" 2>&1 || true
  done
}
# shellcheck source=test/integration/servers.sh
. "$(dirname "$0")/servers.sh"
servers_setup "$prefix"
servers_run "$server_a"
servers_run "$server_b"
servers_track "$outsider"
a_ip=$(servers_ip "$server_a")
b_ip=$(servers_ip "$server_b")
servers_wait
servers_ssh_agent "$a_ip" "$b_ip"
docker run -d --name "$outsider" --network "$network" --entrypoint /bin/sleep "$image" 1800 >/dev/null

cp -a "$repo/examples/postgres-replication" "$work/app"
cd "$work/app"
# Fill in only what the guide tells a user to fill in. The Proxy host is a test
# name served over plain HTTP, because the runner has no public DNS for ACME.
sed -i -e "s/203.0.113.10/$a_ip/; s/203.0.113.11/$b_ip/; s/10.0.0.10/$a_ip/; s/10.0.0.11/$b_ip/" yoho.yml
sed -i -e 's/hosts: \[pg.example.com\]/hosts: [pg-ci.test]\n        ssl: false/' compose.yaml
grep -F "ssh: root@$a_ip, private_address: $a_ip" yoho.yml >/dev/null
grep -F 'ssl: false' compose.yaml >/dev/null
umask 077
printf 'POSTGRES_PASSWORD=%s\nREPLICATOR_PASSWORD=%s\n' "$(openssl rand -hex 24)" "$(openssl rand -hex 24)" > .yoho/secrets
umask 022

# Helpers. sql SERVER SERVICE QUERY runs psql in that Service's container over
# its local socket; nothing here passes a password.
cid() { docker exec "$1" docker ps -q --filter "label=com.docker.compose.project=$project" --filter "label=com.docker.compose.service=$2" | head -n1; }
sql() { local id; id=$(cid "$1" "$2"); [[ -n "$id" ]] || { echo "no $2 container on $1" >&2; return 1; }; docker exec "$1" docker exec "$id" psql -U postgres -d app -XtAc "$3"; }
web() { curl -sS --max-time 10 -H 'Host: pg-ci.test' "http://$a_ip/cgi-bin/$1"; }
eventually() {
  for _ in $(seq 1 60); do
    if "$@"; then return 0; fi
    sleep 2
  done
  echo "timed out: $*" >&2; return 1
}
has_row() { [[ "$(sql "$1" "$2" "select count(*) from items where v='$3'" 2>/dev/null)" == 1 ]]; }
streaming() { [[ "$(sql "$1" "$2" "select state from pg_stat_replication where application_name='$3'" 2>/dev/null)" == streaming ]]; }
clones() { local id; id=$(cid "$1" "$2"); docker exec "$1" docker logs "$id" 2>&1 | grep -c 'yoho-pg: empty data directory' || true; }
# A file in PGDATA that a new base backup would wipe (the entrypoint clears
# partial data before cloning): its survival proves no re-clone happened.
mark() { docker exec "$1" docker exec "$(cid "$1" "$2")" touch /var/lib/postgresql/data/yoho-ci-marker; }
marked() { docker exec "$1" docker exec "$(cid "$1" "$2")" test -e /var/lib/postgresql/data/yoho-ci-marker; }

"$yoho_bin" config check
"$yoho_bin" deploy --version v1

# Replication: a row written through web reaches the standby.
web 'write?row1' | grep -F 'wrote row1'
eventually has_row "$server_b" db-standby row1
# The standby refuses writes.
if out=$(sql "$server_b" db-standby "insert into items(v) values ('nope')" 2>&1); then echo "standby accepted a write: $out" >&2; exit 1; fi
grep -F 'read-only transaction' <<<"$out" >/dev/null
eventually streaming "$server_a" db db-standby
[[ "$(sql "$server_b" db-standby 'select pg_is_in_recovery()')" == t ]]
# No password is stored in the standby's configuration.
if docker exec "$server_b" docker exec "$(cid "$server_b" db-standby)" grep -i 'password=' /var/lib/postgresql/data/postgresql.auto.conf; then echo 'password in primary_conninfo' >&2; exit 1; fi

# Firewall: a permissive Tailscale/ufw-style FORWARD ACCEPT must not let an
# unlisted host reach the primary's Connection port.
docker exec "$server_a" iptables -I FORWARD 1 -j ACCEPT
if docker exec "$outsider" nc -z -w 2 "$a_ip" 5432; then echo 'outsider reached db on A' >&2; exit 1; fi

# A standby restart resumes streaming without a new base backup.
[[ "$(clones "$server_b" db-standby)" == 1 ]]
mark "$server_b" db-standby
docker exec "$server_b" docker restart "$(cid "$server_b" db-standby)" >/dev/null
eventually streaming "$server_a" db db-standby
marked "$server_b" db-standby
web 'write?row2' | grep -F 'wrote row2'
eventually has_row "$server_b" db-standby row2

# Documented REPLICATOR_PASSWORD rotation: new secret, deploy, set-passwords.
sed -i "s/^REPLICATOR_PASSWORD=.*/REPLICATOR_PASSWORD=$(openssl rand -hex 24)/" .yoho/secrets
"$yoho_bin" deploy --version v2
"$yoho_bin" app exec db -- sh /etc/yoho-pg/entrypoint.sh set-passwords
eventually streaming "$server_a" db db-standby
marked "$server_b" db-standby
web 'write?row3' | grep -F 'wrote row3'
eventually has_row "$server_b" db-standby row3

# Promotion, steps 1-5 of the guide.
# 1. Fence the old primary.
"$yoho_bin" app exec db -- psql -U postgres -c 'ALTER SYSTEM SET default_transaction_read_only = on'
"$yoho_bin" app exec db -- psql -U postgres -c 'SELECT pg_reload_conf()'
"$yoho_bin" app exec db -- psql -U postgres -c "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE backend_type = 'client backend' AND pid <> pg_backend_pid()"
web 'write?fenced' | grep -F 'read-only transaction'
lsn=$(sql "$server_a" db 'select pg_current_wal_lsn()')
caught_up() { [[ "$(sql "$server_b" db-standby "select pg_last_wal_replay_lsn() >= '$lsn'::pg_lsn")" == t ]]; }
eventually caught_up
# 2. Promote the standby.
"$yoho_bin" app exec db-standby -- psql -U postgres -c 'SELECT pg_promote()'
promoted() { [[ "$(sql "$server_b" db-standby 'select pg_is_in_recovery()')" == f ]]; }
eventually promoted
# 3. Switch the Connection and its consumers.
sed -i 's/primary: {service: db, port: 5432, consumers: \[web, db-standby\]}/primary: {service: db-standby, port: 5432, consumers: [web, db]}/' yoho.yml
grep -F 'primary: {service: db-standby, port: 5432, consumers: [web, db]}' yoho.yml >/dev/null
# 4. Apply.
"$yoho_bin" apply --auto-approve --version v3
eventually bash -c "curl -sS --max-time 10 -H 'Host: pg-ci.test' 'http://$a_ip/cgi-bin/write?row4' | grep -F 'wrote row4 id=' | grep -F 'via $b_ip'"
has_row "$server_b" db-standby row1
# The Connection firewall now protects B, and A is a permitted consumer.
docker exec "$server_b" iptables -I FORWARD 1 -j ACCEPT
if docker exec "$outsider" nc -z -w 2 "$b_ip" 5432; then echo 'outsider reached db-standby on B' >&2; exit 1; fi
docker exec "$server_a" nc -z -w 2 "$b_ip" 5432
if docker exec "$outsider" nc -z -w 2 "$a_ip" 5432; then echo 'old primary port still published on A' >&2; exit 1; fi
# 5. Rebuild the old db as a standby: remove its volume by hand, apply again.
old=$(cid "$server_a" db)
volume=$(docker exec "$server_a" docker inspect -f '{{range .Mounts}}{{if eq .Destination "/var/lib/postgresql/data"}}{{.Name}}{{end}}{{end}}' "$old")
[[ -n "$volume" ]]
docker exec "$server_a" docker rm -f "$old" >/dev/null
docker exec "$server_a" docker volume rm "$volume" >/dev/null
"$yoho_bin" apply --auto-approve --version v3
eventually streaming "$server_b" db-standby db
eventually has_row "$server_a" db row4
[[ "$(sql "$server_a" db 'select pg_is_in_recovery()')" == t ]]
[[ "$(clones "$server_a" db)" == 1 ]]
# The new primary is not demoted by a restart.
docker exec "$server_b" docker restart "$(cid "$server_b" db-standby)" >/dev/null
eventually promoted
eventually bash -c "curl -sS --max-time 10 -H 'Host: pg-ci.test' 'http://$a_ip/cgi-bin/write?row5' | grep -F 'wrote row5'"
eventually has_row "$server_a" db row5

"$yoho_bin" remove -y
for server in "$server_a" "$server_b"; do
  test -z "$(docker exec "$server" docker ps -aq --filter "label=com.docker.compose.project=$project")"
  docker exec "$server" test ! -d /var/lib/yoho/apps/pg/production
done
echo 'PASS Postgres replication recipe: example deployed as shipped, streaming, read-only standby, firewall bypass resistance, restart without re-clone, password rotation, fenced promotion, rebuild from new primary, no demotion on restart, removal on both Servers'
