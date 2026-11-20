#!/usr/bin/env bash
set -euo pipefail
# shellcheck source=test/integration/resilience-common.sh
. "$(dirname "$0")/resilience-common.sh"
selected=,${YOHO_E2E_FAULTS:-ssh,lock,disk,docker,reboot,backup},
for item in ${selected//,/ }; do
 case $item in ssh|lock|disk|docker|reboot|backup) ;; *) echo "unknown fault: $item" >&2; exit 1;; esac
done
# Mount only the Yoho root, not Docker's shared data root. 256 MiB holds the
# fixture's Release records and secrets; exhaustion is deterministic.
sudo mount -t tmpfs -o size=256m tmpfs "$ROOT"
sudo chown "$USER:$(id -gn)" "$ROOT"
cleanup() {
 set +e
 collect_logs
 sudo iptables -D OUTPUT -d 127.0.0.1 -p tcp --dport 22 -j REJECT 2>/dev/null
 sudo systemctl start docker.socket docker
 touch "$WORK/go"
 [[ -z ${child:-} ]] || { kill "$child" 2>/dev/null; wait "$child" 2>/dev/null; }
 yoho schedule remove nightly
 sudo umount "$ROOT"
}
trap cleanup EXIT
# The barrier is local, runs under the Destination lock, and has a deadline.
cat > .yoho/hooks/pre-app-boot <<HOOK
#!/bin/sh
set -eu
[ "\${YOHO_FAULT_ARM:-}" = 1 ] || exit 0
touch '$WORK/ready'
i=0
while [ ! -f '$WORK/go' ]; do
 i=\$((i+1)); [ "\$i" -lt 180 ] || exit 1
 sleep 1
done
HOOK
chmod +x .yoho/hooks/pre-app-boot
git add .yoho/hooks/pre-app-boot && git commit -qm 'bounded fault barrier'
yoho apply --auto-approve
wait_until healthy
start_apply() {
 rm -f "$WORK/ready" "$WORK/go"
 YOHO_FAULT_ARM=1 "$YOHO" --json apply --auto-approve > "$WORK/$scenario.jsonl" 2> "$WORK/$scenario.stderr" &
 child=$!
 wait_until test -f "$WORK/ready"
}
candidate_started() {
 local id
 for id in $(docker ps -q --filter label=com.docker.compose.project=yoho-shop-production --filter label=com.docker.compose.service=web); do
  [[ $id == "$oldweb" ]] || return 0
 done
 return 1
}
finish_apply() {
 RC=0
 wait "$child" || RC=$?
 child=
 cat "$WORK/$scenario.jsonl"
}
for scenario in ssh lock disk docker reboot backup; do
 [[ $selected == *,$scenario,* ]] || continue
 echo "Fault scenario: $scenario"
 # Hold a new web candidate in readiness long enough to interrupt the live
 # rollout, after container creation and before the Proxy switch.
 [[ -f $WORK/source.go ]] || cp main.go "$WORK/source.go"
 cp "$WORK/source.go" main.go
 python3 - "$scenario" <<'PYWEB'
import sys
p="main.go"
s=open(p).read().replace("func serve() error {\n", "func serve() error {\n\ttime.Sleep(15 * time.Second) // CI readiness window\n", 1)
open(p,"w").write(s)
PYWEB
 # Force a new image and Release even when the previous scenario converged.
 printf '\n// fault scenario %s\n' "$scenario" >> main.go
 git add main.go && git commit -qm "fault $scenario"
 old=$(readlink "$ROOT/apps/shop/production/current")
 oldweb=$(docker ps -q --filter label=com.docker.compose.project=yoho-shop-production --filter label=com.docker.compose.service=web)
 case $scenario in
 lock)
  start_apply
  run_capture "$WORK/contender.jsonl" apply --auto-approve
  assert_result "$WORK/contender.jsonl" "$RC" failed 11
  [[ $(readlink "$ROOT/apps/shop/production/current") == "$old" ]]
  touch "$WORK/go"
  finish_apply
  assert_result "$WORK/$scenario.jsonl" "$RC" success 0
  ;;
 ssh)
  start_apply
  touch "$WORK/go"
  wait_until candidate_started
  # Kill the existing ControlMaster and reject reconnection until the command
  # exits. HTTP remains reachable so Server state is independently observable.
  sudo iptables -I OUTPUT -d 127.0.0.1 -p tcp --dport 22 -j REJECT
  while IFS= read -r socket; do ssh -S "$socket" -O exit "$USER@127.0.0.1" 2>/dev/null || true; done < <(find "/tmp/yoho-$(id -u)" -type s)
  touch "$WORK/go"
  finish_apply
  assert_result "$WORK/$scenario.jsonl" "$RC" unknown 14
  wait_until healthy
  [[ $(readlink "$ROOT/apps/shop/production/current") == "$old" ]]
  records_valid
  sudo iptables -D OUTPUT -d 127.0.0.1 -p tcp --dport 22 -j REJECT
  # The failed process is now gone: recover a lock only after proving that.
  yoho lock release --force
  ;;
 disk)
  # Exhaust blocks before prepare-release; old containers/volumes live outside
  # this mount. The existing Release must remain a complete JSON document.
  dd if=/dev/zero of="$ROOT/full" bs=1M status=none 2>/dev/null || true
  run_capture "$WORK/$scenario.jsonl" apply --auto-approve
  assert_result "$WORK/$scenario.jsonl" "$RC" failed 1
  grep -Eiq 'no space left|free disk space' "$WORK/$scenario.jsonl" "$WORK/$scenario.stderr"
  [[ $(readlink "$ROOT/apps/shop/production/current") == "$old" ]]
  records_valid
  wait_until healthy
  rm -f "$ROOT/full"
  ;;
 docker)
  start_apply
  touch "$WORK/go"
  wait_until candidate_started
  # Stop the socket too, so an API request cannot auto-start Docker and mask
  # the fault. This is the outage half of a deterministic daemon restart.
  sudo systemctl stop docker.socket docker
  touch "$WORK/go"
  finish_apply
  case $RC in
   1|12) assert_result "$WORK/$scenario.jsonl" "$RC" failed "$RC" ;;
   13) assert_result "$WORK/$scenario.jsonl" "$RC" partial 13 ;;
   14) assert_result "$WORK/$scenario.jsonl" "$RC" unknown 14 ;;
   *) echo "unexpected Docker outage exit $RC" >&2; exit 1 ;;
  esac
  sudo systemctl start docker.socket docker
  wait_until healthy
  records_valid
  ;;
 reboot)
  # Deploy this Release, then simulate the Docker portion of a Server reboot.
  run_capture "$WORK/$scenario.jsonl" apply --auto-approve
  assert_result "$WORK/$scenario.jsonl" "$RC" success 0
  sudo systemctl restart docker
  wait_until healthy
  # No Yoho mutation between restart and these probes.
  for service in web worker db; do
   ids=$(docker ps -q --filter label=com.docker.compose.project=yoho-shop-production --filter "label=com.docker.compose.service=$service")
   [[ -n $ids ]]
   for id in $ids; do [[ $(docker inspect -f '{{.HostConfig.RestartPolicy.Name}}' "$id") == unless-stopped ]]; done
  done
  curl -fsS -X POST http://127.0.0.1:18080/jobs --data-binary after-reboot >/dev/null
  wait_until job_done after-reboot
  wait_until services_healthy
  operation_status
  cat "$WORK/status.jsonl"
  ;;
 backup)
  # Make cancellation deterministic inside quiescence, before publication.
  yoho backup run nightly
  yoho backup list nightly --json > "$WORK/before.jsonl"
  python3 - <<'PY'
p='compose.yaml'
s=open(p).read().replace('      backup:\n', '      backup:\n        pre_backup: ["sh", "-c", "touch /tmp/yoho-backup-barrier; sleep 120"]\n')
open(p,'w').write(s)
PY
  "$YOHO" --json backup run nightly > "$WORK/$scenario.jsonl" 2> "$WORK/$scenario.stderr" &
  child=$!
  db=$(docker ps -q --filter label=com.docker.compose.project=yoho-shop-production --filter label=com.docker.compose.service=db)
  wait_until docker exec "$db" test -f /tmp/yoho-backup-barrier
  kill -INT "$child"
  finish_apply
  assert_result "$WORK/$scenario.jsonl" "$RC" unknown 14
  yoho backup list nightly --json > "$WORK/after.jsonl"
  diff <(jq -sc '[.[] | select(.event=="row") | .id] | sort' "$WORK/before.jsonl") <(jq -sc '[.[] | select(.event=="row") | .id] | sort' "$WORK/after.jsonl")
  git restore compose.yaml
  yoho backup run nightly
  wait_until healthy
  ;;
 esac
 converge
 [[ -z ${RESILIENCE_LOG_DIR:-} ]] || cp "$WORK/"*.jsonl "$WORK/"*.stderr "$RESILIENCE_LOG_DIR/"
done
