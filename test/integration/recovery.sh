#!/usr/bin/env bash
set -euo pipefail
# shellcheck source=test/integration/resilience-common.sh
. "$(dirname "$0")/resilience-common.sh"
# A fresh SSH user and fresh root model a replacement Server. Only Yoho-owned
# resources are removed; Docker/OS provisioning is reused, never unrelated Apps.
replacement=yoho-recovery
cleanup() {
 collect_logs
 yoho schedule remove nightly >/dev/null 2>&1 || true
}
trap cleanup EXIT
yoho apply --auto-approve
wait_until healthy
curl -fsS -X POST http://127.0.0.1:18080/jobs --data-binary before-loss >/dev/null
wait_until job_done before-loss
yoho backup run nightly
# The encrypted repository is off-Server material; neither root is its parent.
oldroot=$ROOT
yoho schedule remove nightly
mapfile -t ids < <(docker ps -aq --filter label=com.docker.compose.project=yoho-shop-production)
[[ ${#ids[@]} == 0 ]] || docker rm -f "${ids[@]}"
mapfile -t volumes < <(docker volume ls -q --filter label=com.docker.compose.project=yoho-shop-production)
[[ ${#volumes[@]} == 0 ]] || docker volume rm "${volumes[@]}"
# The Proxy belongs to this isolated CI job; remove its state as well.
docker rm -f yoho-proxy
docker volume rm yoho-proxy-config
mapfile -t networks < <(docker network ls -q --filter label=com.docker.compose.project=yoho-shop-production)
[[ ${#networks[@]} == 0 ]] || docker network rm "${networks[@]}"
docker network rm yoho
mapfile -t images < <(docker image ls -q --filter label=yoho.app=shop | sort -u)
[[ ${#images[@]} == 0 ]] || docker image rm -f "${images[@]}"
rm -rf "$oldroot"
sudo useradd -m -G docker -s /bin/bash "$replacement"
sudo install -d -m 700 -o "$replacement" -g "$replacement" "/home/$replacement/.ssh"
sudo install -m 600 -o "$replacement" -g "$replacement" "$HOME/.ssh/id_ed25519.pub" "/home/$replacement/.ssh/authorized_keys"
printf '%s ALL=(ALL) NOPASSWD:ALL\n' "$replacement" | sudo tee "/etc/sudoers.d/$replacement" >/dev/null
# Reattach the external disk to the replacement identity.
sudo chown -R "$replacement:$replacement" "$TARGET"
chmod 755 "$WORK"
ROOT=$WORK/replacement
sudo install -d -m 700 -o "$replacement" -g "$replacement" "$ROOT"
write_config "$replacement"
git add yoho.yml && git commit -qm 'point at replacement Server'
version=$(git rev-parse HEAD)
# Ensure a new identity has no inherited Release or generated-secret material.
[[ ! -e $ROOT/apps ]]
[[ ! -d $oldroot ]]
[[ -z $(docker ps -aq --filter label=com.docker.compose.project=yoho-shop-production) ]]
start=$(date +%s)
yoho setup --yes --no-containerd-image-store
yoho apply --auto-approve
run_capture "$WORK/restore.jsonl" backup restore latest --yes --report
assert_result "$WORK/restore.jsonl" "$RC" success 0
converge
wait_until job_done before-loss
curl -fsS -X POST http://127.0.0.1:18080/jobs --data-binary after-recovery >/dev/null
wait_until job_done after-recovery
sudo systemctl is-active --quiet yoho-shop-production-nightly.timer
yoho schedule status
# Read back both known rows directly, in addition to HTTP/worker checks.
yoho app exec db -- psql -U postgres -tAc "SELECT count(*) FROM jobs WHERE payload IN ('before-loss', 'after-recovery') AND status='done'" | tee "$WORK/rows.txt"
grep -Eq '^[[:space:]]*2[[:space:]]*$' "$WORK/rows.txt"
full=$(( $(date +%s) - start ))
jq -sc --argjson full "$full" --arg source "rebuilt from git at $version" '[.[] | select(.event == "recovery")][0] | .full_recovery_seconds=$full | .image_source=$source | .scheduled_jobs_reinstalled=true' "$WORK/restore.jsonl" | tee "$WORK/recovery.json"
if [[ -n ${GITHUB_STEP_SUMMARY:-} ]]; then
 { echo 'Recovery measured in CI (setup through healthy Proxy, restored rows, worker, active Scheduled Job):'; echo '```json'; cat "$WORK/recovery.json"; echo '```'; } >> "$GITHUB_STEP_SUMMARY"
fi
[[ -z ${RESILIENCE_LOG_DIR:-} ]] || cp "$WORK/recovery.json" "$RESILIENCE_LOG_DIR/"
