#!/usr/bin/env bash
# Deploys examples/postgres-replication as shipped (only Server addresses, the
# secrets and the Proxy host are filled in) to two nested SSH + Docker Servers,
# then walks the documented procedures: password rotation, app stop/start
# and promotion with yoho db promote.
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
# Secret values go through files and the printf builtin, never an exec'd argv.
umask 077
openssl rand -hex 24 > "$work/token"
printf 'POSTGRES_PASSWORD=%s\nREPLICATOR_PASSWORD=%s\nDEMO_WRITE_TOKEN=%s\n' "$(openssl rand -hex 24)" "$(openssl rand -hex 24)" "$(cat "$work/token")" > .yoho/secrets
umask 022

# Helpers. sql SERVER SERVICE QUERY runs psql in that Service's container over
# its local socket; nothing here passes a password.
cid() { docker exec "$1" docker ps -q --filter "label=com.docker.compose.project=$project" --filter "label=com.docker.compose.service=$2" | head -n1; }
sql() { local id; id=$(cid "$1" "$2"); [[ -n "$id" ]] || { echo "no $2 container on $1" >&2; return 1; }; docker exec "$1" docker exec "$id" psql -U postgres -d app -XtAc "$3"; }
web() { curl -sS --max-time 10 -H 'Host: pg-ci.test' "http://$a_ip/cgi-bin/$1"; }
# write VALUE posts the demo write token from a file (never in argv).
write() { curl -sS --max-time 10 -H 'Host: pg-ci.test' --data-binary @"$work/token" "http://$a_ip/cgi-bin/write?$1"; }
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
mark() { docker exec "$1" docker exec "$(cid "$1" "$2")" touch /var/lib/postgresql/data/pgdata/yoho-ci-marker; }
marked() { docker exec "$1" docker exec "$(cid "$1" "$2")" test -e /var/lib/postgresql/data/pgdata/yoho-ci-marker; }

"$yoho_bin" config check
"$yoho_bin" deploy --version v1

# Replication: a row written through web reaches the standby.
# The demo write endpoint refuses requests without the token.
web 'write?nope' | grep -F 'POST the write token'
curl -sS --max-time 10 -H 'Host: pg-ci.test' --data-binary wrong "http://$a_ip/cgi-bin/write?nope" | grep -F forbidden
write row1 | grep -F 'wrote row1'
eventually has_row "$server_b" db-standby row1
# The standby refuses writes.
if out=$(sql "$server_b" db-standby "insert into items(v) values ('nope')" 2>&1); then echo "standby accepted a write: $out" >&2; exit 1; fi
grep -F 'read-only transaction' <<<"$out" >/dev/null
eventually streaming "$server_a" db db-standby
[[ "$(sql "$server_b" db-standby 'select pg_is_in_recovery()')" == t ]]
# No password is stored in the standby's configuration.
if docker exec "$server_b" docker exec "$(cid "$server_b" db-standby)" grep -i 'password=' /var/lib/postgresql/data/pgdata/postgresql.auto.conf; then echo 'password in primary_conninfo' >&2; exit 1; fi

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
write row2 | grep -F 'wrote row2'
eventually has_row "$server_b" db-standby row2

# Documented REPLICATOR_PASSWORD rotation: new secret, deploy, set-passwords.
umask 077
{ grep -v '^REPLICATOR_PASSWORD=' .yoho/secrets; printf 'REPLICATOR_PASSWORD=%s\n' "$(openssl rand -hex 24)"; } > "$work/secrets.new"
mv "$work/secrets.new" .yoho/secrets
umask 022
"$yoho_bin" deploy --version v2
"$yoho_bin" app exec db -- sh /etc/yoho-pg/entrypoint.sh set-passwords
eventually streaming "$server_a" db db-standby
marked "$server_b" db-standby
write row3 | grep -F 'wrote row3'
eventually has_row "$server_b" db-standby row3

# yoho app stop: the standby stays down through deploy, rollback and a Docker
# restart, keeps its volume, and comes back with yoho app start.
absent() { [[ -z "$(docker exec "$1" docker ps -aq --filter "label=com.docker.compose.project=$project" --filter "label=com.docker.compose.service=$2")" ]]; }
"$yoho_bin" app stop db-standby --reason ci
absent "$server_b" db-standby
"$yoho_bin" app stop db-standby | grep -F 'already stopped'
"$yoho_bin" app ps | grep -F 'db-standby: stopped by'
"$yoho_bin" plan --version v2 | tee "$work/plan.out"
grep -F 'service db-standby [b]: stopped' "$work/plan.out" >/dev/null
"$yoho_bin" deploy --version v2-stop
absent "$server_b" db-standby
"$yoho_bin" rollback v2
absent "$server_b" db-standby
# Restart the nested Server's Docker daemon: nothing revives the Service.
docker exec "$server_b" sh -c 'pkill -x dockerd; while pgrep -x dockerd >/dev/null; do sleep 1; done'
docker exec -d "$server_b" sh -c 'dockerd --host=unix:///var/run/docker.sock > /var/log/dockerd.log 2>&1'
docker_up() { docker exec "$server_b" docker info >/dev/null 2>&1; }
eventually docker_up
sleep 5
absent "$server_b" db-standby
write row-stopped | grep -F 'wrote row-stopped'
"$yoho_bin" app start db-standby
"$yoho_bin" app start db-standby | grep -F 'it is running'
eventually streaming "$server_a" db db-standby
# The volume was kept: no new base backup, and the standby catches up.
marked "$server_b" db-standby
[[ "$(clones "$server_b" db-standby)" == 1 ]]
eventually has_row "$server_b" db-standby row-stopped

# yoho db promote, config first. Without a config edit it refuses and says
# what to edit; without the old primary as a consumer it refuses too.
if out=$("$yoho_bin" db promote --auto-approve --version v3 2>&1); then echo "promote ran without a config edit: $out" >&2; exit 1; fi
grep -F 'connections.primary.service' <<<"$out" | grep -F 'to the standby' >/dev/null
sed -i 's/primary: {service: db, port: 5432, consumers: \[web, db-standby\]}/primary: {service: db-standby, port: 5432, consumers: [web]}/' yoho.yml
if out=$("$yoho_bin" db promote --auto-approve --version v3 2>&1); then echo "promote accepted a second primary: $out" >&2; exit 1; fi
grep -F 'never as a second primary' <<<"$out" >/dev/null
# Nothing was changed by the refusals.
[[ "$(sql "$server_b" db-standby 'select pg_is_in_recovery()')" == t ]]
[[ "$(sql "$server_a" db 'select pg_is_in_recovery()')" == f ]]
# Switch the Connection and its consumers, then promote.
sed -i 's/primary: {service: db-standby, port: 5432, consumers: \[web\]}/primary: {service: db-standby, port: 5432, consumers: [web, db]}/' yoho.yml
grep -F 'primary: {service: db-standby, port: 5432, consumers: [web, db]}' yoho.yml >/dev/null
"$yoho_bin" db promote --auto-approve --version v3 2>&1 | tee "$work/promote.out"
grep -F "docker volume rm ${project}_pgdata" "$work/promote.out" >/dev/null
promoted() { [[ "$(sql "$server_b" db-standby 'select pg_is_in_recovery()')" == f ]]; }
promoted
# The old primary is fenced: stopped, removed, and kept down.
absent "$server_a" db
wrote_via_b() { write row4 | grep -F 'wrote row4 id=' | grep -F "via $b_ip"; }
eventually wrote_via_b
has_row "$server_b" db-standby row1
has_row "$server_b" db-standby row-stopped
# The Connection firewall now protects B, and A is a permitted consumer.
docker exec "$server_b" iptables -I FORWARD 1 -j ACCEPT
if docker exec "$outsider" nc -z -w 2 "$b_ip" 5432; then echo 'outsider reached db-standby on B' >&2; exit 1; fi
docker exec "$server_a" nc -z -w 2 "$b_ip" 5432
if docker exec "$outsider" nc -z -w 2 "$a_ip" 5432; then echo 'old primary port still published on A' >&2; exit 1; fi
# The journal is gone after the apply: a rerun has nothing to resume, refuses
# (no Connection changed) and changes nothing.
if out=$("$yoho_bin" db promote --auto-approve --version v3 2>&1); then echo "rerun after a finished promotion did not refuse: $out" >&2; exit 1; fi
grep -F 'no Connection changed its provider' <<<"$out" >/dev/null
docker exec "$server_b" test ! -e /var/lib/yoho/apps/pg/production/promotions/primary.json
promoted
absent "$server_a" db
# Rebuild the old db as a standby: remove its volume by hand, then start it.
docker exec "$server_a" docker volume inspect "${project}_pgdata" >/dev/null
docker exec "$server_a" docker volume rm "${project}_pgdata" >/dev/null
"$yoho_bin" app start db
eventually streaming "$server_b" db-standby db
eventually has_row "$server_a" db row4
[[ "$(sql "$server_a" db 'select pg_is_in_recovery()')" == t ]]
[[ "$(clones "$server_a" db)" == 1 ]]
# The new primary is not demoted by a restart.
docker exec "$server_b" docker restart "$(cid "$server_b" db-standby)" >/dev/null
eventually promoted
wrote5() { write row5 | grep -F 'wrote row5'; }
eventually wrote5
eventually has_row "$server_a" db row5

"$yoho_bin" remove -y
for server in "$server_a" "$server_b"; do
  test -z "$(docker exec "$server" docker ps -aq --filter "label=com.docker.compose.project=$project")"
  docker exec "$server" test ! -d /var/lib/yoho/apps/pg/production
done
echo 'PASS Postgres replication recipe: example deployed as shipped, streaming, read-only standby, firewall bypass resistance, restart without re-clone, password rotation, app stop kept through deploy/rollback/Docker restart and app start without re-clone, db promote refusals, fenced promotion and refused rerun, rebuild from new primary, no demotion on restart, removal on both Servers'
