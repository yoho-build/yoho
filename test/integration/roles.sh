#!/usr/bin/env bash
# Two real SSH + Docker Servers on an isolated outer Docker network. No host
# Docker socket is mounted; all App containers run in the nested daemons.
set -euo pipefail
work=$(mktemp -d)
prefix="yoho-roles-ci-$$"
network="$prefix-net"
server_a="$prefix-a"
server_b="$prefix-b"
outsider="$prefix-outsider"
image="$prefix-server"
yoho_bin=${YOHO_BIN:-"$work/yoho"}
loop_pid=
cleanup() {
  status=$?
  if [[ -n "$loop_pid" ]]; then kill "$loop_pid" 2>/dev/null || true; wait "$loop_pid" 2>/dev/null || true; fi
  if (( status != 0 )); then
    for server in "$server_a" "$server_b"; do
      docker logs "$server" || true
      docker exec "$server" docker ps -a || true
      docker exec "$server" iptables -t mangle -S || true
    done
  fi
  docker rm -fv "$server_a" "$server_b" "$outsider" >/dev/null 2>&1 || true
  docker network rm "$network" >/dev/null 2>&1 || true
  docker image rm "$image" >/dev/null 2>&1 || true
  if [[ -n "${SSH_AGENT_PID:-}" ]]; then ssh-agent -k >/dev/null || true; fi
  rm -rf "$work"
  exit "$status"
}
trap cleanup EXIT
if [[ ! -x "$yoho_bin" ]]; then go build -o "$yoho_bin" ./cmd/yoho; fi
ssh-keygen -q -t ed25519 -N '' -f "$work/key"
cp "$work/key.pub" "$work/authorized_keys"
cat > "$work/Dockerfile" <<'DOCKER'
FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl gnupg openssh-server iptables iproute2 netcat-openbsd util-linux systemd python3 && \
    install -m 0755 -d /etc/apt/keyrings && \
    curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc && \
    echo 'deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu noble stable' > /etc/apt/sources.list.d/docker.list && \
    apt-get update && apt-get install -y --no-install-recommends docker-ce docker-ce-cli containerd.io docker-compose-plugin docker-buildx-plugin && \
    rm -rf /var/lib/apt/lists/* && mkdir -p /run/sshd /root/.ssh && ssh-keygen -A && \
    printf '\nPasswordAuthentication no\nKbdInteractiveAuthentication no\nPermitRootLogin prohibit-password\n' >> /etc/ssh/sshd_config
COPY authorized_keys /root/.ssh/authorized_keys
RUN chmod 700 /root/.ssh && chmod 600 /root/.ssh/authorized_keys
# A container has no systemd PID 1. Enable the real unit offline, but skip
# daemon reload/boot-order execution only. Packet filtering is never stubbed.
RUN printf '%s\n' '#!/bin/sh' 'case "$1" in' 'daemon-reload) echo "SKIP systemd boot/reload assertion: nested Server PID 1 is not systemd" >&2; exit 0;;' 'enable|is-enabled) exec /usr/bin/systemctl --root=/ "$@";;' '*) exec /usr/bin/systemctl "$@";;' 'esac' > /usr/local/bin/systemctl && chmod 755 /usr/local/bin/systemctl
CMD ["/bin/sh", "-ec", "dockerd --host=unix:///var/run/docker.sock > /var/log/dockerd.log 2>&1 & exec /usr/sbin/sshd -D -e"]
DOCKER
docker build -t "$image" "$work"
docker network create "$network" >/dev/null
for spec in "$server_a:2222" "$server_b:2223"; do
  name=${spec%:*}; port=${spec##*:}
  # Keep inner image layers off the outer container's overlay filesystem.
  docker run -d --privileged -v /var/lib/docker --network "$network" --name "$name" -p "127.0.0.1:$port:22" "$image" >/dev/null
done
a_ip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$server_a")
b_ip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$server_b")
for server in "$server_a" "$server_b"; do
  ready=0
  for _ in $(seq 1 90); do
    if docker exec "$server" docker info >/dev/null 2>&1; then ready=1; break; fi
    sleep 1
  done
  [[ "$ready" == 1 ]]
done
# Only authentication setup is local to this test's agent; no private key is
# passed in argv, Compose, containers, logs, or App configuration.
eval "$(ssh-agent -s)" >/dev/null
ssh-add "$work/key" >/dev/null
mkdir -p "$HOME/.ssh"
for port in 2222 2223; do ssh-keyscan -p "$port" 127.0.0.1 >> "$HOME/.ssh/known_hosts"; done
mkdir "$work/app"
python3 - "$work/app" "$a_ip" "$b_ip" <<'PY'
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
   query();self.send_response(200);self.end_headers();self.wfile.write((os.environ.get('MARKER','v1')+' db=42').encode())
  except Exception as e:
   self.send_response(503);self.end_headers();self.wfile.write(str(e).encode())
 def log_message(self,*args):pass
HTTPServer(('0.0.0.0',8000),Handler).serve_forever()
'''
worker="import socket,time\nwhile True:\n s=socket.create_connection(('db',5432),3);s.close();print('db reachable',flush=True);time.sleep(1)"
compose={'services':{
 'web':{'image':'python:3.13-alpine','command':['python','-u','-c',web],'restart':'unless-stopped','environment':{'MARKER':'v1'},'healthcheck':{'test':['CMD','python','-c',"import urllib.request;urllib.request.urlopen('http://127.0.0.1:8000/')"],'interval':'1s','timeout':'5s'},'x-yoho':{'proxy':{'hosts':['roles-ci.test'],'ssl':False,'port':8000,'health_path':'/'}}},
 'worker':{'image':'python:3.13-alpine','command':['python','-u','-c',worker],'restart':'unless-stopped'},
 'db':{'image':'postgres:17-alpine','restart':'unless-stopped','environment':{'POSTGRES_HOST_AUTH_METHOD':'trust'},'volumes':['data:/var/lib/postgresql/data'],'healthcheck':{'test':['CMD','pg_isready','-h','127.0.0.1'],'interval':'1s'},'x-yoho':{'stateful':True}}},'volumes':{'data':{}}}
config={'app':'roles-ci','servers':{'a':{'ssh':'root@127.0.0.1:2222','private_address':sys.argv[2]},'b':{'ssh':'root@127.0.0.1:2223','private_address':sys.argv[3]}},'destinations':{'production':{'servers':['a','b'],'roles':{'app':{'services':['web','worker'],'servers':['a']},'data':{'services':['db'],'servers':['b']}},'connections':{'database':{'service':'db','port':5432,'consumers':['web','worker']}}}}}
(p/'compose.yaml').write_text(json.dumps(compose));(p/'yoho.json').write_text(json.dumps(config))
PY
cd "$work/app"
"$yoho_bin" config check
"$yoho_bin" deploy --version v1
curl -fsS --max-time 5 -H 'Host: roles-ci.test' "http://$a_ip/" | grep -F 'v1 db=42'
# Simulate a permissive Tailscale/ufw-style ACCEPT before Docker in FORWARD.
# The mangle PREROUTING policy must still reject an unlisted third Server.
docker exec "$server_b" iptables -I FORWARD 1 -j ACCEPT
docker run -d --name "$outsider" --network "$network" --entrypoint /bin/sleep "$image" 600 >/dev/null
if docker exec "$outsider" nc -z -w 2 "$b_ip" 5432; then echo 'outsider reached db' >&2; exit 1; fi
if timeout 3 bash -c 'exec 3<>/dev/tcp/'"$a_ip"'/5432'; then echo 'db reachable through Server A' >&2; exit 1; fi
# Requests validate the database query, not just an nginx/static response.
(
  while [[ ! -f "$work/requests-stop" ]]; do
    curl -fsS --max-time 5 -H 'Host: roles-ci.test' "http://$a_ip/" | grep -F 'db=42' >/dev/null
    sleep 0.1
  done
) > "$work/requests.log" 2>&1 &
loop_pid=$!
python3 - <<'PY'
import json,pathlib
p=pathlib.Path('compose.yaml');c=json.loads(p.read_text());c['services']['web']['environment']['MARKER']='v2';p.write_text(json.dumps(c))
PY
"$yoho_bin" deploy --version v2
curl -fsS --max-time 5 -H 'Host: roles-ci.test' "http://$a_ip/" | grep -F 'v2 db=42'
touch "$work/requests-stop"
wait "$loop_pid"
loop_pid=
"$yoho_bin" remove -y
for server in "$server_a" "$server_b"; do
  test -z "$(docker exec "$server" docker ps -aq --filter label=com.docker.compose.project=yoho-roles-ci-production)"
  docker exec "$server" test ! -d /var/lib/yoho/apps/roles-ci/production
done
echo 'PASS Roles: two SSH/dind Servers, real db DNS/query, firewall bypass resistance, zero failed redeploy requests, removal on both Servers'
echo 'SKIP systemd boot-order execution assertion only: nested Servers do not run systemd as PID 1 (units enabled offline)'
