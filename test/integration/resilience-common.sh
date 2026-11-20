#!/usr/bin/env bash
# Source from recovery.sh/faults.sh on a disposable Linux machine only.
set -euo pipefail
[[ ${YOHO_E2E:-} == 1 ]] || { echo 'skip: set YOHO_E2E=1'; exit 0; }
[[ ${YOHO_E2E_DISPOSABLE:-} == 1 ]] || { echo 'requires YOHO_E2E_DISPOSABLE=1 (restarts Docker, changes firewall)' >&2; exit 1; }
REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
. "$REPO/test/integration/run.sh"
YOHO=${YOHO_BIN:-$REPO/qa/resilience/yoho}
WORK=$(mktemp -d /tmp/yoho-resilience.XXXXXX)
APP=$WORK/app
ROOT=$WORK/server
TARGET=$WORK/off-server
mkdir -p "$APP" "$ROOT" "$TARGET"
echo "Resilience fixture: $WORK"
cp -a "$REPO/examples/quickstart/." "$APP/"
mkdir -p "$APP/.yoho/hooks"
printf 'APP_GREETING=ci-recovery\nRESTIC_PASSWORD=ci-only-backup-password\n' > "$APP/.yoho/secrets"
chmod 600 "$APP/.yoho/secrets"
write_config() {
 cat > "$APP/yoho.yml" <<YAML
app: shop
servers:
  local:
    ssh: $1@127.0.0.1
    sudo: true
    root: $ROOT
destinations:
  production:
    servers: [local]
builder:
  location: local
proxy:
  http_port: 18080
  https_port: 0
setup:
  user: $1
  firewall: false
  auto_updates: false
backups:
  targets:
    off-server:
      type: restic
      repository: $TARGET
      password_secret: RESTIC_PASSWORD
  jobs:
    nightly:
      destination: production
      target: off-server
      schedule: '*-*-* 03:00:00'
YAML
}
write_config "$USER"
cd "$APP"
git init -q -b main
git config user.email yoho-ci@example.com
git config user.name yoho-ci
git add -A
git commit -qm 'recovery fixture'
yoho() { "$YOHO" "$@"; }
wait_until() {
 local i
 for ((i=0; i<120; i++)); do
  if "$@"; then return 0; fi
  sleep 1
 done
 echo "timed out: $*" >&2
 return 1
}
healthy() { curl -fsS --max-time 2 http://127.0.0.1:18080/up >/dev/null; }
job_done() {
 curl -fsS --max-time 2 http://127.0.0.1:18080/jobs | jq -e --arg p "$1" 'any(.[]; .payload == $p and .status == "done")' >/dev/null
}
assert_result() {
 local file=$1 code=$2 outcome=$3
 jq -se --arg outcome "$outcome" '[.[] | select(.event == "finished")] | length == 1 and .[0].outcome == $outcome' "$file" >/dev/null
 [[ $code == "$4" ]]
}
run_capture() {
 local file=$1; shift
 RC=0
 # RC is consumed by the sourcing recovery/fault harness.
 # shellcheck disable=SC2034
 yoho --json "$@" > "$file" 2> "$file.stderr" || RC=$?
 cat "$file"
}
records_valid() {
 local f
 while IFS= read -r f; do jq -e '.app == "shop" and .version != null' "$f" >/dev/null; done < <(find "$ROOT/apps/shop/production/releases" -name release.json)
}
services_healthy() {
 local ids id
 ids=$(docker ps -q --filter label=com.docker.compose.project=yoho-shop-production)
 [[ $(wc -w <<< "$ids") == 3 ]] || return 1
 for id in $ids; do
  [[ $(docker inspect -f '{{.State.Health.Status}}' "$id") == healthy ]] || return 1
 done
}
operation_status() {
 local id
 id=$(jq -r .operation_id "$ROOT/apps/shop/production/current/release.json")
 yoho status --operation "$id" --json > "$WORK/status.jsonl"
 jq -se 'any(.[]; .event == "operation_status" and .outcome == "success" and .lock_held == false)' "$WORK/status.jsonl" >/dev/null
}
converge() {
 yoho apply --auto-approve
 wait_until healthy
 wait_until services_healthy
 operation_status
 yoho plan --detailed-exitcode
 records_valid
}

collect_logs() {
 [[ -n ${RESILIENCE_LOG_DIR:-} ]] || return 0
 mkdir -p "$RESILIENCE_LOG_DIR"
 local f
 for f in "$WORK/"*.jsonl "$WORK/"*.stderr "$WORK/recovery.json"; do
  [[ ! -f $f ]] || cp "$f" "$RESILIENCE_LOG_DIR/"
 done
}
