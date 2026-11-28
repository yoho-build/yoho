#!/usr/bin/env bash
# Deploys examples/mysql-replication as shipped (only Server addresses, the
# secrets and the Proxy host are filled in) to two nested SSH + Docker Servers,
# then walks the documented procedures: password rotation and promotion.
# See docs/site/mysql-replication.md.
set -euo pipefail
repo=$(pwd)
prefix="yoho-mdb-ci-$$"
server_a="$prefix-a"
server_b="$prefix-b"
outsider="$prefix-outsider"
project=yoho-mariadb-production
servers_extra_diagnose() {
  local id
  for id in $(docker exec "$1" docker ps -aq --filter "label=com.docker.compose.project=$project"); do
    docker exec "$1" docker logs --tail 40 "$id" 2>&1 || true
    echo "--- replication state of $id"
    docker exec "$1" docker exec "$id" sh /etc/yoho-mariadb/entrypoint.sh mariadb -e 'SHOW REPLICA STATUS\G SELECT @@gtid_current_pos, @@gtid_slave_pos, @@read_only; SELECT * FROM app.items' 2>&1 || true
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

cp -a "$repo/examples/mysql-replication" "$work/app"
cd "$work/app"
# Fill in only what the guide tells a user to fill in. The Proxy host is a test
# name served over plain HTTP, because the runner has no public DNS for ACME.
sed -i -e "s/203.0.113.10/$a_ip/; s/203.0.113.11/$b_ip/; s/10.0.0.10/$a_ip/; s/10.0.0.11/$b_ip/" yoho.yml
sed -i -e 's/hosts: \[mariadb.example.com\]/hosts: [mariadb-ci.test]\n        ssl: false/' compose.yaml
grep -F "ssh: root@$a_ip, private_address: $a_ip" yoho.yml >/dev/null
grep -F 'ssl: false' compose.yaml >/dev/null
# Secret values go through files and the printf builtin, never an exec'd argv.
umask 077
openssl rand -hex 24 > "$work/token"
printf 'MARIADB_ROOT_PASSWORD=%s\nMARIADB_PASSWORD=%s\nREPLICATOR_PASSWORD=%s\nDEMO_WRITE_TOKEN=%s\n' "$(openssl rand -hex 24)" "$(openssl rand -hex 24)" "$(openssl rand -hex 24)" "$(cat "$work/token")" > .yoho/secrets
umask 022

# Helpers. sql SERVER SERVICE QUERY runs the mariadb client as root in that
# Service's container over its local socket (the recipe's `mariadb` command
# reads the root password file); nothing here passes a password.
cid() { docker exec "$1" docker ps -q --filter "label=com.docker.compose.project=$project" --filter "label=com.docker.compose.service=$2" | head -n1; }
sql() { local id; id=$(cid "$1" "$2"); [[ -n "$id" ]] || { echo "no $2 container on $1" >&2; return 1; }; docker exec "$1" docker exec "$id" sh /etc/yoho-mariadb/entrypoint.sh mariadb -N -B app -e "$3"; }
web() { curl -sS --max-time 10 -H 'Host: mariadb-ci.test' "http://$a_ip/cgi-bin/$1"; }
# write VALUE posts the demo write token from a file (never in argv).
write() { curl -sS --max-time 10 -H 'Host: mariadb-ci.test' --data-binary @"$work/token" "http://$a_ip/cgi-bin/write?$1"; }
eventually() {
  for _ in $(seq 1 60); do
    if "$@"; then return 0; fi
    sleep 2
  done
  echo "timed out: $*" >&2; return 1
}
has_row() { [[ "$(sql "$1" "$2" "select count(*) from items where v='$3'" 2>/dev/null)" == 1 ]]; }
# replicating SERVER SERVICE: both replication threads of that replica run.
replicating() { [[ "$(sql "$1" "$2" 'SHOW REPLICA STATUS\G' 2>/dev/null | grep -cE '^ *Slave_(IO|SQL)_Running: Yes$')" == 2 ]]; }
read_only() { [[ "$(sql "$1" "$2" 'select @@read_only')" == 1 ]]; }
writable() { [[ "$(sql "$1" "$2" 'select @@read_only')" == 0 ]]; }
clones() { local id; id=$(cid "$1" "$2"); docker exec "$1" docker logs "$id" 2>&1 | grep -c 'yoho-mariadb: empty data directory' || true; }
# A file in the data directory that a new clone would wipe (the entrypoint
# clears it before cloning): its survival proves no re-clone happened.
mark() { docker exec "$1" docker exec "$(cid "$1" "$2")" touch /var/lib/mysql/yoho-ci-marker; }
marked() { docker exec "$1" docker exec "$(cid "$1" "$2")" test -e /var/lib/mysql/yoho-ci-marker; }

"$yoho_bin" config check
"$yoho_bin" deploy --version v1

# Replication: a row written through web reaches the replica.
# The demo write endpoint refuses requests without the token.
web 'write?nope' | grep -F 'POST the write token'
curl -sS --max-time 10 -H 'Host: mariadb-ci.test' --data-binary wrong "http://$a_ip/cgi-bin/write?nope" | grep -F forbidden
write row1 | grep -F 'wrote row1'
eventually has_row "$server_b" db-replica row1
eventually replicating "$server_b" db-replica
# The replica refuses writes from the App's user.
read_only "$server_b" db-replica
if out=$(docker exec "$server_b" docker exec "$(cid "$server_b" db-replica)" sh -c 'MYSQL_PWD=$(cat /run/secrets/MARIADB_PASSWORD) mariadb -h 127.0.0.1 -u app app -e "insert into items(v) values (\"nope\")"' 2>&1); then echo "replica accepted a write: $out" >&2; exit 1; fi
grep -F 'read-only' <<<"$out" >/dev/null
writable "$server_a" db

# Firewall: a permissive Tailscale/ufw-style FORWARD ACCEPT must not let an
# unlisted host reach the primary's Connection port.
docker exec "$server_a" iptables -I FORWARD 1 -j ACCEPT
if docker exec "$outsider" nc -z -w 2 "$a_ip" 3306; then echo 'outsider reached db on A' >&2; exit 1; fi

# A replica restart resumes replication without a new clone.
[[ "$(clones "$server_b" db-replica)" == 1 ]]
mark "$server_b" db-replica
docker exec "$server_b" docker restart "$(cid "$server_b" db-replica)" >/dev/null
eventually replicating "$server_b" db-replica
marked "$server_b" db-replica
read_only "$server_b" db-replica
write row2 | grep -F 'wrote row2'
eventually has_row "$server_b" db-replica row2

# Documented REPLICATOR_PASSWORD rotation: new secret, deploy, set-passwords.
umask 077
{ grep -v '^REPLICATOR_PASSWORD=' .yoho/secrets; printf 'REPLICATOR_PASSWORD=%s\n' "$(openssl rand -hex 24)"; } > "$work/secrets.new"
mv "$work/secrets.new" .yoho/secrets
umask 022
"$yoho_bin" deploy --version v2
"$yoho_bin" app exec db -- sh /etc/yoho-mariadb/entrypoint.sh set-passwords
eventually replicating "$server_b" db-replica
marked "$server_b" db-replica
write row3 | grep -F 'wrote row3'
eventually has_row "$server_b" db-replica row3

# Promotion, steps 1-5 of the guide.
# 1. Fence the old primary.
"$yoho_bin" app exec db -- sh /etc/yoho-mariadb/entrypoint.sh fence
write fenced | grep -F 'read-only'
pos=$(sql "$server_a" db 'select @@gtid_binlog_pos')
[[ "$pos" =~ ^[0-9,-]+$ ]]
# 2. Promote the replica once it has applied everything the primary wrote.
"$yoho_bin" app exec db-replica -- sh /etc/yoho-mariadb/entrypoint.sh promote "$pos"
writable "$server_b" db-replica
[[ -z "$(sql "$server_b" db-replica 'SHOW REPLICA STATUS')" ]]
# 3. Switch the Connection and its consumers.
sed -i 's/primary: {service: db, port: 3306, consumers: \[web, db-replica\]}/primary: {service: db-replica, port: 3306, consumers: [web, db]}/' yoho.yml
grep -F 'primary: {service: db-replica, port: 3306, consumers: [web, db]}' yoho.yml >/dev/null
# 4. Apply.
"$yoho_bin" apply --auto-approve --version v3
wrote_via_b() { write row4 | grep -F 'wrote row4 id=' | grep -F "via $b_ip"; }
eventually wrote_via_b
has_row "$server_b" db-replica row1
# The old primary restarted read-only, fenced and a consumer with old data.
read_only "$server_a" db
# The Connection firewall now protects B, and A is a permitted consumer.
docker exec "$server_b" iptables -I FORWARD 1 -j ACCEPT
if docker exec "$outsider" nc -z -w 2 "$b_ip" 3306; then echo 'outsider reached db-replica on B' >&2; exit 1; fi
docker exec "$server_a" nc -z -w 2 "$b_ip" 3306
if docker exec "$outsider" nc -z -w 2 "$a_ip" 3306; then echo 'old primary port still published on A' >&2; exit 1; fi
# 5. Rebuild the old db as a replica: remove its volume by hand, apply again.
old=$(cid "$server_a" db)
volume=$(docker exec "$server_a" docker inspect -f '{{range .Mounts}}{{if eq .Destination "/var/lib/mysql"}}{{.Name}}{{end}}{{end}}' "$old")
[[ -n "$volume" ]]
docker exec "$server_a" docker rm -f "$old" >/dev/null
docker exec "$server_a" docker volume rm "$volume" >/dev/null
"$yoho_bin" apply --auto-approve --version v3
eventually replicating "$server_a" db
eventually has_row "$server_a" db row4
has_row "$server_a" db row1
read_only "$server_a" db
[[ "$(clones "$server_a" db)" == 1 ]]
# The new primary is not demoted by a restart.
docker exec "$server_b" docker restart "$(cid "$server_b" db-replica)" >/dev/null
promoted() { writable "$server_b" db-replica; }
eventually promoted
wrote5() { write row5 | grep -F 'wrote row5'; }
eventually wrote5
eventually has_row "$server_a" db row5

"$yoho_bin" remove -y
for server in "$server_a" "$server_b"; do
  test -z "$(docker exec "$server" docker ps -aq --filter "label=com.docker.compose.project=$project")"
  docker exec "$server" test ! -d /var/lib/yoho/apps/mariadb/production
done
echo 'PASS MariaDB replication recipe: example deployed as shipped, GTID replication, read-only replica, firewall bypass resistance, restart without re-clone, password rotation, fenced promotion, rebuild from new primary, no demotion on restart, removal on both Servers'
