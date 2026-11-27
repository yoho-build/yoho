#!/usr/bin/env bash
# Three real SSH + Docker Servers on an isolated outer Docker network, joined by
# Yoho's opt-in WireGuard mesh (ADR 0018), then a Roles App (ADR 0017/0019)
# whose database connection runs over mesh addresses. Server c is NATed
# (`endpoint: none`): it drops inbound new UDP, so every mesh packet to it must
# ride the tunnel it opened to the hub (a, first public Server by name).
# Requires a Linux host with the wireguard kernel module loaded and SSH access
# to Docker bridge IPs (GitHub ubuntu runners). No host Docker socket is mounted.
set -euo pipefail
repo=$(pwd)
prefix="yoho-mesh-ci-$$"
server_a="$prefix-a"
server_b="$prefix-b"
server_c="$prefix-c"
servers=("$server_a" "$server_b" "$server_c")
project=yoho-mesh-ci-production
servers_extra_diagnose() {
  docker exec "$1" sh -c 'wg show yoho0 | grep -v -i "private"; ip -4 addr show yoho0; ip -4 route; iptables -S; ufw show added' || true
}
if ! grep -qw '^wireguard' /proc/modules 2>/dev/null && [[ ! -d /sys/module/wireguard ]]; then
  echo 'wireguard kernel module is not loaded on this host; run: sudo modprobe wireguard' >&2
  exit 1
fi
# Mesh prerequisites are preinstalled so the run does not depend on apt
# mirrors mid-test; Yoho's prerequisite script then finds them and skips apt.
SERVERS_EXTRA_PACKAGES="wireguard-tools iputils-ping ufw procps"
# shellcheck source=test/integration/servers.sh
. "$(dirname "$0")/servers.sh"
servers_setup "$prefix"
for name in "${servers[@]}"; do servers_run "$name"; done
a_ip=$(servers_ip "$server_a"); b_ip=$(servers_ip "$server_b"); c_ip=$(servers_ip "$server_c")
servers_wait
# NAT simulation for c: no unsolicited inbound UDP on its outer interface.
# Replies to tunnels c opened (to the hub) are ESTABLISHED and still pass.
docker exec "$server_c" iptables -I INPUT 1 -i eth0 -p udp -m conntrack --ctstate NEW -j DROP
servers_ssh_agent "$a_ip" "$b_ip" "$c_ip"
mkdir "$work/app"
# SSH targets are the Servers' bridge IPs, so the default mesh endpoint (SSH
# host + network.port) is the real reachable address and c's SSH host is its
# real egress IP for the hub's peer allowlist. No private_address is set: the
# Roles connection must take mesh addresses.
python3 - "$work/app" "$a_ip" "$b_ip" "$c_ip" <<'PY'
import json,sys,pathlib
p=pathlib.Path(sys.argv[1])
web='''import os,socket,struct
from http.server import BaseHTTPRequestHandler,HTTPServer
def readn(s,n):
 b=b''
 while len(b)<n:
  x=s.recv(n-len(b))
  if not x: raise RuntimeError('db closed')
  b+=x
 return b
def frame(s):
 t=readn(s,1);n=struct.unpack('!I',readn(s,4))[0];return t,readn(s,n-4)
def query():
 with socket.create_connection(('db',5432),3) as s:
  s.settimeout(3)
  body=struct.pack('!I',196608)+b'user\\0postgres\\0database\\0postgres\\0\\0'
  s.sendall(struct.pack('!I',len(body)+4)+body)
  while True:
   t,b=frame(s)
   if t==b'E':raise RuntimeError('db startup failed')
   if t==b'Z':break
  q=b'select 42\\0';s.sendall(b'Q'+struct.pack('!I',len(q)+4)+q)
  found=False
  while True:
   t,b=frame(s)
   if t==b'E':raise RuntimeError('query failed')
   if t==b'D':found=b[6:]==b'42'
   if t==b'Z':break
  if not found:raise RuntimeError('incorrect database result')
class Handler(BaseHTTPRequestHandler):
 def do_GET(self):
  try:
   query();self.send_response(200);self.end_headers();self.wfile.write(('mesh db=42 via '+socket.gethostbyname('db')).encode())
  except Exception as e:
   self.send_response(503);self.end_headers();self.wfile.write(str(e).encode())
 def log_message(self,*args):pass
HTTPServer(('0.0.0.0',8000),Handler).serve_forever()
'''
worker="import socket,time\nwhile True:\n s=socket.create_connection(('db',5432),3);s.close();print('db reachable',flush=True);time.sleep(1)"
compose={'services':{
 'web':{'image':'python:3.13-alpine','command':['python','-u','-c',web],'restart':'unless-stopped','healthcheck':{'test':['CMD','python','-c',"import urllib.request;urllib.request.urlopen('http://127.0.0.1:8000/')"],'interval':'1s','timeout':'5s'},'x-yoho':{'proxy':{'hosts':['mesh-ci.test'],'ssl':False,'port':8000,'health_path':'/'}}},
 'worker':{'image':'python:3.13-alpine','command':['python','-u','-c',worker],'restart':'unless-stopped'},
 'db':{'image':'postgres:17-alpine','restart':'unless-stopped','environment':{'POSTGRES_HOST_AUTH_METHOD':'trust'},'volumes':['data:/var/lib/postgresql/data'],'healthcheck':{'test':['CMD','pg_isready','-h','127.0.0.1'],'interval':'1s'},'x-yoho':{'stateful':True}}},'volumes':{'data':{}}}
config={'app':'mesh-ci','network':{'mesh':'wireguard'},
 'servers':{'a':{'ssh':'root@'+sys.argv[2]},'b':{'ssh':'root@'+sys.argv[3]},'c':{'ssh':'root@'+sys.argv[4],'endpoint':'none'}},
 'destinations':{
  # web on b, db on NATed c: the connection b -> c is forwarded by hub a.
  'production':{'servers':['b','c'],'roles':{'app':{'services':['web','worker'],'servers':['b']},'data':{'services':['db'],'servers':['c']}},'connections':{'database':{'service':'db','port':5432,'consumers':['web','worker']}}}}}
(p/'compose.yaml').write_text(json.dumps(compose));(p/'yoho.json').write_text(json.dumps(config))
PY
cd "$work/app"
"$yoho_bin" config check
mesh_changes() { python3 -c 'import json,sys
n=0
for l in sys.stdin:
  try: e=json.loads(l)
  except ValueError: continue
  if e.get("event")=="change" and e.get("kind")=="mesh": n+=1; print(json.dumps(e),file=sys.stderr)
print(n)'; }
# 1. plan, before any mesh exists, on the Roles Destination itself: it reports
# the mesh and planned Connection addresses and installs nothing.
plan_rc=0
"$yoho_bin" plan --json --detailed-exitcode > "$work/plan1.json" || plan_rc=$?
[[ "$plan_rc" == 2 ]] || { echo "plan exit $plan_rc, want 2 (changes)" >&2; cat "$work/plan1.json" >&2; exit 1; }
before=$(mesh_changes < "$work/plan1.json")
(( before > 0 )) || { echo 'plan reported no mesh changes before mesh up' >&2; exit 1; }
grep -F 'assign address 10.210.0.3' "$work/plan1.json" >/dev/null
grep -F 'mesh address 10.210.0.3 is planned' "$work/plan1.json" >/dev/null
for server in "${servers[@]}"; do
  if docker exec "$server" ip link show yoho0 >/dev/null 2>&1; then echo "plan created yoho0 on $server" >&2; exit 1; fi
  docker exec "$server" test ! -e /etc/wireguard/yoho0.conf
done
# 2. mesh up: addresses, peers, hub-only state, firewall allowlist.
"$yoho_bin" mesh up
declare -A want=(["$server_a"]=10.210.0.1 ["$server_b"]=10.210.0.2 ["$server_c"]=10.210.0.3)
declare -A peers=(["$server_a"]=2 ["$server_b"]=1 ["$server_c"]=1)
for server in "${servers[@]}"; do
  docker exec "$server" ip -4 -o addr show dev yoho0 | grep -F "inet ${want[$server]}/24 " >/dev/null
  got=$(docker exec "$server" wg show yoho0 peers | wc -l)
  [[ "$got" == "${peers[$server]}" ]] || { echo "$server has $got peers, want ${peers[$server]}" >&2; exit 1; }
  docker exec "$server" systemctl is-enabled wg-quick@yoho0 >/dev/null
  docker exec "$server" sh -ec 'test "$(stat -c %a /etc/wireguard/yoho0.key)" = 600; test "$(stat -c %a /etc/wireguard/yoho0.conf)" = 600'
  # No UDP 51820 rule open to every source, on any Server.
  if docker exec "$server" ufw show added | grep -F 51820 | grep -vF ' from '; then echo "unrestricted mesh port rule on $server" >&2; exit 1; fi
done
# Hub-only state lives only on a.
docker exec "$server_a" grep -qx 'net.ipv4.ip_forward=1' /etc/sysctl.d/99-yoho-mesh.conf
test "$(docker exec "$server_a" sysctl -n net.ipv4.ip_forward)" = 1
docker exec "$server_a" iptables -S DOCKER-USER | grep -F yoho-mesh >/dev/null
docker exec "$server_a" ufw show added | grep -F 'ufw route allow in on yoho0 out on yoho0' >/dev/null
for server in "$server_b" "$server_c"; do
  docker exec "$server" test ! -e /etc/sysctl.d/99-yoho-mesh.conf
  if docker exec "$server" iptables -S | grep -F yoho-mesh; then echo "hub forwarding rule on $server" >&2; exit 1; fi
  if docker exec "$server" ufw show added | grep -F 'route allow'; then echo "hub route rule on $server" >&2; exit 1; fi
done
# The hub allowlist names c's real egress IP (its SSH host) on the mesh port.
docker exec "$server_a" ufw show added | grep -F "from $c_ip to any port 51820" >/dev/null
# c is outbound-only: no ListenPort, no inbound rule; b does not peer with c.
docker exec "$server_c" sh -ec '! grep -q ListenPort /etc/wireguard/yoho0.conf'
if docker exec "$server_c" ufw show added | grep -F yoho-mesh; then echo 'inbound mesh rule on NATed c' >&2; exit 1; fi
c_pub=$(docker exec "$server_c" wg show yoho0 public-key)
if docker exec "$server_b" wg show yoho0 peers | grep -qxF "$c_pub"; then echo 'b peers with NATed c directly' >&2; exit 1; fi
# 3. Reachability between every pair, NATed c through the hub.
mesh_ping() {
  local from=$1 to=$2
  for _ in $(seq 1 15); do
    if docker exec "$from" ping -c 1 -W 2 "$to" >/dev/null 2>&1; then return 0; fi
    sleep 1
  done
  echo "mesh ping $from -> $to failed" >&2; return 1
}
ping_all() {
  local pair
  # c first: it opens the NAT tunnel to the hub.
  for pair in "$server_c 10.210.0.1" "$server_a 10.210.0.3" "$server_b 10.210.0.3" "$server_c 10.210.0.2" \
              "$server_a 10.210.0.2" "$server_b 10.210.0.1"; do
    # shellcheck disable=SC2086
    mesh_ping $pair
  done
}
ping_all
"$yoho_bin" mesh status --json > "$work/status.json"
python3 - "$work/status.json" <<'PY'
import json,sys
rows=[json.loads(l) for l in open(sys.argv[1]) if l.startswith('{')]
peers=[r for r in rows if r.get('event')=='mesh_peer']
assert len(peers)==4, peers
for p in peers:
  assert p['handshake_age_seconds'] is not None, p
  assert p['rtt']!='unreachable', p
PY
# No private key material in status output (keys are compared, never printed).
if grep -qi 'private' "$work/status.json"; then echo 'status mentions private keys' >&2; exit 1; fi
for server in "${servers[@]}"; do
  if docker exec "$server" cat /etc/wireguard/yoho0.key | grep -qF -f - "$work/status.json"; then echo "status leaks $server private key" >&2; exit 1; fi
done
# 4. Idempotence: a second mesh up changes nothing; plan reports no mesh change.
snap() { local s; for s in "${servers[@]}"; do docker exec "$s" sh -c 'sha256sum /etc/wireguard/yoho0.conf /var/lib/yoho/mesh/state.json; cat /sys/class/net/yoho0/ifindex; wg show yoho0 public-key; iptables -S | grep yoho-mesh || true; ufw show added'; done; }
snap > "$work/snap1"
"$yoho_bin" mesh up
snap > "$work/snap2"
diff -u "$work/snap1" "$work/snap2"
"$yoho_bin" plan --json > "$work/plan2.json" || true
after=$(mesh_changes < "$work/plan2.json")
[[ "$after" == 0 ]] || { echo "plan reported $after mesh changes after mesh up" >&2; exit 1; }
# 5. Roles over the mesh: b's web/worker reach db on NATed c via hub a.
"$yoho_bin" deploy --version v1
web=$(docker exec "$server_b" docker ps -q --filter "label=com.docker.compose.project=$project" --filter label=com.docker.compose.service=web | head -n1)
[[ -n "$web" ]]
resolved=$(docker exec "$server_b" docker exec "$web" python -c "import socket;print(socket.gethostbyname('db'))")
[[ "$resolved" == 10.210.0.3 ]] || { echo "db resolves to $resolved, want mesh address 10.210.0.3" >&2; exit 1; }
curl -fsS --max-time 5 -H 'Host: mesh-ci.test' "http://$b_ip/" | grep -F 'mesh db=42 via 10.210.0.3'
# A mesh peer that is not a consumer (hub a) is refused at c's db port.
if docker exec "$server_a" nc -z -w 2 10.210.0.3 5432; then echo 'non-consumer mesh peer reached db' >&2; exit 1; fi
# db is published only on c's mesh address, not its outer interface.
if docker exec "$server_a" nc -z -w 2 "$c_ip" 5432; then echo 'db reachable on c outer address' >&2; exit 1; fi
# The whole Destination now matches the plan.
plan_rc=0
"$yoho_bin" plan --detailed-exitcode > "$work/plan3.txt" 2>&1 || plan_rc=$?
[[ "$plan_rc" == 0 ]] || { echo "plan after deploy exit $plan_rc, want 0" >&2; cat "$work/plan3.txt" >&2; exit 1; }
# 6. Key rotation: every public key changes, the mesh and the App keep working.
pubs() { local s; for s in "${servers[@]}"; do docker exec "$s" wg show yoho0 public-key; done; }
pubs > "$work/pub1"
"$yoho_bin" mesh rotate-keys
pubs > "$work/pub2"
if [[ -n "$(comm -12 <(sort "$work/pub1") <(sort "$work/pub2"))" ]]; then echo 'a public key survived rotate-keys' >&2; exit 1; fi
ping_all
for _ in $(seq 1 20); do
  if curl -fsS --max-time 5 -H 'Host: mesh-ci.test' "http://$b_ip/" | grep -F 'mesh db=42 via 10.210.0.3'; then break; fi
  sleep 1
done
curl -fsS --max-time 5 -H 'Host: mesh-ci.test' "http://$b_ip/" | grep -F 'mesh db=42 via 10.210.0.3'
# 7. Teardown.
"$yoho_bin" remove -y
for server in "$server_b" "$server_c"; do
  test -z "$(docker exec "$server" docker ps -aq --filter "label=com.docker.compose.project=$project")"
  docker exec "$server" test ! -d /var/lib/yoho/apps/mesh-ci/production
done
"$yoho_bin" mesh down
for server in "${servers[@]}"; do
  docker exec "$server" sh -ec '! ip link show yoho0 >/dev/null 2>&1
test ! -e /etc/wireguard/yoho0.conf
test ! -e /etc/wireguard/yoho0.key
test ! -e /etc/wireguard/yoho0.owned
test ! -e /var/lib/yoho/mesh/state.json
test ! -e /etc/sysctl.d/99-yoho-mesh.conf
! iptables -S | grep -F yoho-mesh
! ufw show added | grep -F yoho-mesh
! systemctl is-enabled wg-quick@yoho0 >/dev/null 2>&1'
done
echo 'PASS Mesh CLI: plan before mesh, mesh up (3 Servers, NATed c via hub), hub-only state, allowlist, idempotent rerun, status without keys, Roles db over mesh, non-consumer refused, rotate-keys, remove, mesh down'
# 8. The package e2e test against the same Servers (fresh yoho0, own root).
# Its NAT deny uses `ufw insert 1`, which needs an existing rule on c.
docker exec "$server_c" ufw allow 22/tcp >/dev/null
(cd "$repo" && YOHO_E2E=1 YOHO_E2E_MESH_HOSTS="root@$a_ip,root@$b_ip,root@$c_ip" go test ./internal/mesh -run '^TestE2EMesh$' -count=1 -v)
echo 'PASS TestE2EMesh against the same three Servers'
echo 'SKIP systemd start/stop/daemon-reload and reboot persistence: nested Servers do not run systemd as PID 1 (units enabled offline; wg-quick drives the interface)'
echo 'SKIP UFW enforcement: UFW is installed and its yoho-mesh rules are asserted, but UFW stays inactive (Yoho never enables it); NAT on c is simulated with iptables'
