#!/bin/sh
# Helpers for the integration workflow. Source this file; do not execute it.
set -eu

# assert_http URL CODE exits non-zero when URL does not return CODE.
assert_http() {
  url=$1
  want=$2
  body=$(mktemp)
  got=$(curl -sS -o "$body" -w '%{http_code}' --max-time 15 "$url" || true)
  if [ "$got" != "$want" ]; then
    echo "assert_http $url: want $want got $got" >&2
    cat "$body" >&2 || true
    rm -f "$body"
    return 1
  fi
  rm -f "$body"
}

# loop_requests URL SECONDS OUTFILE curls URL every 50ms for SECONDS.
# Successful bodies are appended one per line. The file ends with
# "ok N" and "fail M" count lines.
loop_requests() {
  url=$1
  seconds=$2
  outfile=$3
  ok=0
  fail=0
  end=$(date +%s)
  end=$((end + seconds))
  : >"$outfile"
  while [ "$(date +%s)" -lt "$end" ]; do
    body=$(curl -fsS --max-time 2 "$url" 2>/dev/null) && rc=0 || rc=$?
    if [ "$rc" -eq 0 ]; then
      ok=$((ok + 1))
      printf '%s\n' "$body" >>"$outfile"
    else
      fail=$((fail + 1))
    fi
    sleep 0.05
  done
  printf 'ok %s\nfail %s\n' "$ok" "$fail" >>"$outfile"
}

# fail_count FILE prints the last fail count written by loop_requests.
fail_count() {
  awk '/^fail / { n=$2 } END { print n }' "$1"
}

# ok_count FILE prints the last ok count written by loop_requests.
ok_count() {
  awk '/^ok / { n=$2 } END { print n }' "$1"
}

# setup_localhost_ssh installs sshd if needed and checks key auth to this user.
# GitHub-hosted Ubuntu runners log in as runner, so the check is
# ssh runner@127.0.0.1.
setup_localhost_ssh() {
  if ! command -v sshd >/dev/null 2>&1 && [ ! -x /usr/sbin/sshd ]; then
    sudo DEBIAN_FRONTEND=noninteractive apt-get update
    sudo DEBIAN_FRONTEND=noninteractive apt-get install -y openssh-server
  fi
  sudo systemctl enable --now ssh
  mkdir -p "$HOME/.ssh"
  chmod 700 "$HOME/.ssh"
  if [ ! -f "$HOME/.ssh/id_ed25519" ]; then
    ssh-keygen -t ed25519 -N "" -f "$HOME/.ssh/id_ed25519"
  fi
  touch "$HOME/.ssh/authorized_keys"
  key=$(cat "$HOME/.ssh/id_ed25519.pub")
  if ! grep -qxF "$key" "$HOME/.ssh/authorized_keys"; then
    printf '%s\n' "$key" >>"$HOME/.ssh/authorized_keys"
  fi
  chmod 600 "$HOME/.ssh/authorized_keys"
  i=0
  while [ "$i" -lt 30 ]; do
    ssh-keyscan 127.0.0.1 >>"$HOME/.ssh/known_hosts" 2>/dev/null || true
    if ssh -o BatchMode=yes "$USER@127.0.0.1" docker version; then
      return 0
    fi
    i=$((i + 1))
    sleep 1
  done
  echo "ssh -o BatchMode=yes $USER@127.0.0.1 docker version failed" >&2
  return 1
}
