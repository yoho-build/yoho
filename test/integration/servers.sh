# shellcheck shell=bash
# Shared harness: real SSH + Docker "Servers" as privileged nested-dind
# containers on an isolated outer Docker network. No host Docker socket is
# mounted; all App containers run in the nested daemons. Source it, then:
#
#   servers_setup PREFIX           # work dir, key, image, network, EXIT trap
#   servers_run NAME [SSH_PORT]    # start a Server (optionally 127.0.0.1:PORT->22)
#   servers_wait                   # wait for every nested Docker daemon
#   servers_ip NAME                # outer-network IP
#   servers_ssh_agent TARGET...    # ssh-agent + known_hosts; TARGET is IP or 127.0.0.1:PORT
#
# Before servers_setup a script may set SERVERS_EXTRA_PACKAGES, and define
# servers_extra_cleanup (always run first on exit) and servers_extra_diagnose
# (run per Server on failure). Variables set: work, network, image, yoho_bin.
SERVERS_EXTRA_PACKAGES=${SERVERS_EXTRA_PACKAGES:-}
servers_names=()
servers_containers=()
if ! declare -F servers_extra_cleanup >/dev/null; then servers_extra_cleanup() { :; }; fi
if ! declare -F servers_extra_diagnose >/dev/null; then servers_extra_diagnose() { :; }; fi

servers_cleanup() {
  local status=$?
  servers_extra_cleanup || true
  if (( status != 0 )); then
    local server
    for server in "${servers_names[@]}"; do
      echo "--- $server"
      docker logs "$server" 2>&1 | tail -n 50 || true
      docker exec "$server" docker ps -a || true
      docker exec "$server" iptables -t mangle -S || true
      servers_extra_diagnose "$server" || true
    done
  fi
  if (( ${#servers_containers[@]} > 0 )); then docker rm -fv "${servers_containers[@]}" >/dev/null 2>&1 || true; fi
  docker network rm "$network" >/dev/null 2>&1 || true
  docker image rm "$image" >/dev/null 2>&1 || true
  if [[ -n "${SSH_AGENT_PID:-}" ]]; then ssh-agent -k >/dev/null || true; fi
  rm -rf "$work"
  exit "$status"
}

servers_setup() {
  local prefix=$1
  work=$(mktemp -d)
  network="$prefix-net"
  image="$prefix-server"
  yoho_bin=${YOHO_BIN:-"$work/yoho"}
  trap servers_cleanup EXIT
  if [[ ! -x "$yoho_bin" ]]; then go build -o "$yoho_bin" ./cmd/yoho; fi
  ssh-keygen -q -t ed25519 -N '' -f "$work/key"
  cp "$work/key.pub" "$work/authorized_keys"
  cat > "$work/Dockerfile" <<DOCKER
FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl gnupg openssh-server iptables iproute2 netcat-openbsd util-linux systemd python3 $SERVERS_EXTRA_PACKAGES && \\
    install -m 0755 -d /etc/apt/keyrings && \\
    curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc && \\
    echo 'deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu noble stable' > /etc/apt/sources.list.d/docker.list && \\
    apt-get update && apt-get install -y --no-install-recommends docker-ce docker-ce-cli containerd.io docker-compose-plugin docker-buildx-plugin && \\
    rm -rf /var/lib/apt/lists/* && mkdir -p /run/sshd /root/.ssh && ssh-keygen -A && \\
    printf '\\nPasswordAuthentication no\\nKbdInteractiveAuthentication no\\nPermitRootLogin prohibit-password\\n' >> /etc/ssh/sshd_config
COPY authorized_keys /root/.ssh/authorized_keys
RUN chmod 700 /root/.ssh && chmod 600 /root/.ssh/authorized_keys
COPY systemctl /usr/local/bin/systemctl
RUN chmod 755 /usr/local/bin/systemctl
CMD ["/bin/sh", "-ec", "dockerd --host=unix:///var/run/docker.sock > /var/log/dockerd.log 2>&1 & exec /usr/sbin/sshd -D -e"]
DOCKER
  # A container has no systemd PID 1. Enable/disable units for real (offline,
  # --root); no-op only daemon-reload and start/stop/restart, with SKIP lines.
  # Packet filtering is never stubbed.
  cat > "$work/systemctl" <<'SHIM'
#!/bin/sh
case "$1" in
daemon-reload|start|stop|restart) echo "SKIP systemctl $*: nested Server PID 1 is not systemd" >&2; exit 0;;
enable|disable|is-enabled) exec /usr/bin/systemctl --root=/ "$@";;
*) exec /usr/bin/systemctl "$@";;
esac
SHIM
  docker build -t "$image" "$work"
  docker network create "$network" >/dev/null
}

servers_run() {
  local name=$1 port=${2:-}
  local publish=()
  if [[ -n "$port" ]]; then publish=(-p "127.0.0.1:$port:22"); fi
  # Keep inner image layers off the outer container's overlay filesystem.
  docker run -d --privileged -v /var/lib/docker --network "$network" --name "$name" "${publish[@]}" "$image" >/dev/null
  servers_names+=("$name")
  servers_containers+=("$name")
}

# servers_track NAME registers an extra container (e.g. an outsider) for removal.
servers_track() { servers_containers+=("$1"); }

servers_ip() { docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$1"; }

servers_wait() {
  local server ready
  for server in "${servers_names[@]}"; do
    ready=0
    for _ in $(seq 1 90); do
      if docker exec "$server" docker info >/dev/null 2>&1; then ready=1; break; fi
      sleep 1
    done
    [[ "$ready" == 1 ]] || { echo "nested Docker on $server did not start" >&2; return 1; }
  done
}

# Only authentication setup is local to this test's agent; no private key is
# passed in argv, Compose, containers, logs, or App configuration.
servers_ssh_agent() {
  local target
  eval "$(ssh-agent -s)" >/dev/null
  ssh-add "$work/key" >/dev/null
  mkdir -p "$HOME/.ssh"
  for target in "$@"; do
    if [[ "$target" == *:* ]]; then
      ssh-keyscan -p "${target##*:}" "${target%:*}" >> "$HOME/.ssh/known_hosts" 2>/dev/null
    else
      ssh-keyscan "$target" >> "$HOME/.ssh/known_hosts" 2>/dev/null
    fi
  done
}
