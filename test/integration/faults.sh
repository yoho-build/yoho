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
cat > .yoho/hooks/pre-deploy <<HOOK
#!/bin/sh
set -eu
[ "\${YOHO_DISK_ARM:-}" = 1 ] || exit 0
test -s '$ROOT/apps/shop/production/lock/info.json'
test "\${YOHO_LOCK:-}" = true
operation_id=\$(jq -ser '[.[] | select(.event == "operation_started")] | select(length == 1) | .[0].operation_id | select(type == "string" and length > 0)' '$WORK/disk.jsonl')
jq -e --arg id "\$operation_id" '.id == \$id and .state == "running"' '$ROOT/apps/shop/production/operations/'"\$operation_id".json >/dev/null
touch '$WORK/disk-mutated'
# ENOSPC is the Hook failure, after real Release preparation, before rollout.
dd if=/dev/zero of='$ROOT/full' bs=1M 2>'$WORK/disk-fill.stderr'
HOOK
chmod +x .yoho/hooks/pre-app-boot .yoho/hooks/pre-deploy
git add .yoho/hooks/pre-app-boot .yoho/hooks/pre-deploy && git commit -qm 'bounded fault barrier'
yoho apply --auto-approve
wait_until healthy
start_apply() {
 rm -f "$WORK/ready" "$WORK/go"
 YOHO_FAULT_ARM=1 "$YOHO" --json apply --auto-approve > "$WORK/$scenario.jsonl" 2> "$WORK/$scenario.stderr" &
 child=$!
 wait_until test -f "$WORK/ready"
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
 python3 - <<'PYWEB'
p="main.go"
s=open(p).read()
if s.count("func serve() error {\n") != 1:
    raise SystemExit("readiness Hook did not match")
s=s.replace("func serve() error {\n", "func serve() error {\n\ttime.Sleep(15 * time.Second) // CI readiness window\n", 1)
open(p,"w").write(s)
PYWEB
 # Force a new image and Release even when the previous scenario converged.
 printf '\n// fault scenario %s\n' "$scenario" >> main.go
 git add main.go && git commit -qm "fault $scenario"
 old=$(readlink "$ROOT/apps/shop/production/current")
 candidate_version=$(git rev-parse HEAD)
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
  wait_until candidate_started "$candidate_version"
  kill -0 "$child" # Never claim injection after the deploy has already exited.
  # Kill the existing ControlMaster and reject reconnection until the command
  # exits. HTTP remains reachable so Server state is independently observable.
  sudo iptables -I OUTPUT -d 127.0.0.1 -p tcp --dport 22 -j REJECT
  sockets=$(find "/tmp/yoho-$(id -u)" -type s)
  [[ -n $sockets ]]
  closed=0
  while IFS= read -r socket; do
   if ssh -n -S "$socket" -O exit "$USER@127.0.0.1" 2>/dev/null; then closed=$((closed + 1)); fi
  done <<< "$sockets"
  [[ $closed -gt 0 ]]
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
  # Fill only after the operation, secrets and Release preparation exist.
  server_run cat "$ROOT/apps/shop/production/current/release.json" > "$WORK/disk-before-release.json"
  YOHO_DISK_ARM=1 run_capture "$WORK/$scenario.jsonl" apply --auto-approve
  assert_result "$WORK/$scenario.jsonl" "$RC" failed 1
  test -f "$WORK/disk-mutated"
  server_run cat "$ROOT/apps/shop/production/current/release.json" | cmp - "$WORK/disk-before-release.json"
  jq -se 'any(.[]; .event == "operation_started") and any(.[]; .event == "step_fail" and .failure_facts.phase == "pre-deploy")' "$WORK/$scenario.jsonl" >/dev/null
  grep -Eiq 'no space left|free disk space' "$WORK/disk-fill.stderr" "$WORK/$scenario.jsonl.stderr"
  [[ $(readlink "$ROOT/apps/shop/production/current") == "$old" ]]
  records_valid
  wait_until healthy
  rm -f "$ROOT/full"
  ;;
 docker)
  start_apply
  touch "$WORK/go"
  wait_until candidate_started "$candidate_version"
  kill -0 "$child" # Never claim injection after the deploy has already exited.
  # Stop the socket too, so an API request cannot auto-start Docker and mask
  # the fault. This is the outage half of a deterministic daemon restart.
  sudo systemctl stop docker.socket docker
  touch "$WORK/go"
  finish_apply
  operation_id=$(jq -ser '[.[] | select(.event == "operation_started")] | select(length == 1) | .[0].operation_id | select(type == "string" and test("^[A-Za-z0-9_-]{1,128}$"))' "$WORK/$scenario.jsonl")
  server_run cat "$ROOT/apps/shop/production/operations/$operation_id.json" > "$WORK/docker-operation.json"
  docker_fault_result "$WORK/$scenario.jsonl" "$WORK/docker-operation.json" "$RC"
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
  jq -se '[.[] | select(.event == "row")] | length > 0' "$WORK/before.jsonl" >/dev/null
  db=$(docker ps -q --filter label=com.docker.compose.project=yoho-shop-production --filter label=com.docker.compose.service=db)
  # Incompressible data keeps the real pause/tar window observable. No
  # pre_backup override: SIGINT must exercise the normal unpause cleanup.
  docker exec "$db" sh -c 'dd if=/dev/urandom of=/var/lib/postgresql/data/yoho-fault-payload bs=1M count=128' >/dev/null
  "$YOHO" --json backup run nightly > "$WORK/$scenario.jsonl" 2> "$WORK/$scenario.stderr" &
  child=$!
  paused=false
  for ((attempt=0; attempt<2400; attempt++)); do
   if [[ $(docker inspect -f '{{.State.Paused}}' "$db") == true ]]; then paused=true; break; fi
   kill -0 "$child" # A completed Backup is not an injected interruption.
   sleep 0.05
  done
  [[ $paused == true ]]
  kill -INT "$child"
  finish_apply
  assert_result "$WORK/$scenario.jsonl" "$RC" unknown 14
  [[ $(docker inspect -f '{{.State.Paused}}' "$db") == false ]]
  yoho backup list nightly --json > "$WORK/after.jsonl"
  diff <(jq -sc '[.[] | select(.event=="row") | .id] | sort' "$WORK/before.jsonl") <(jq -sc '[.[] | select(.event=="row") | .id] | sort' "$WORK/after.jsonl")
  docker exec "$db" rm -f /var/lib/postgresql/data/yoho-fault-payload
  yoho backup run nightly
  wait_until healthy
  ;;
 esac
 converge
 [[ -z ${RESILIENCE_LOG_DIR:-} ]] || cp "$WORK/"*.jsonl "$WORK/"*.stderr "$RESILIENCE_LOG_DIR/"
done
