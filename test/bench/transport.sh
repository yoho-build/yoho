#!/bin/sh
# Run only against an idle, disposable Linux Server. No pruning or daemon changes.
set -eu
: "${BENCH_SSH:?set BENCH_SSH to the operator SSH alias for a disposable Server}"
: "${BENCH_INTERFACE:?set BENCH_INTERFACE to its inbound network interface (e.g. eth0)}"
YOHO=${YOHO:-yoho}
BENCH_APP=${BENCH_APP:-transport-bench}
BENCH_PLATFORM=${BENCH_PLATFORM:-linux/amd64}
BENCH_MODES=${BENCH_MODES:-pussh gzip zstd container direct gate}
BENCH_OUTPUT=${BENCH_OUTPUT:-qa/transport-bench-$(date +%Y%m%d-%H%M%S)}
# Values enter generated JSON or remote commands: accept simple SSH aliases/targets only.
for value in "$BENCH_SSH" "$BENCH_INTERFACE" "$BENCH_APP" "$BENCH_PLATFORM" "${BENCH_BUILDER:-unset}" "${BENCH_GATE_SSH:-unset}"; do
 case "$value" in ''|*[!a-zA-Z0-9_./:@-]*) echo 'Use simple SSH aliases and alphanumeric configuration values' >&2; exit 1;; esac
done
mkdir -p "$BENCH_OUTPUT"
BENCH_OUTPUT=$(cd "$BENCH_OUTPUT" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' 0
trap 'exit 130' INT
trap 'exit 143' TERM
printf 'mode,phase,bytes_received,wall_seconds,scope,status\n' > "$BENCH_OUTPUT/results.csv"
cat > "$work/Dockerfile" <<'DOCKER'
FROM busybox:1.37
COPY base /base
COPY change /change
CMD ["sleep", "86400"]
DOCKER
cat > "$work/compose.yaml" <<'COMPOSE'
services:
  web:
    build: .
COMPOSE
rx() {
 ssh -o BatchMode=yes "$BENCH_SSH" "set -eu; cat /sys/class/net/$BENCH_INTERFACE/statistics/rx_bytes"
}
# The same config is used for cold/warm deploys; versions differ but only COPY
# change changes content. Generate new base bytes per mode to avoid cross-mode cache.
run_id=$(date +%s)
for mode in $BENCH_MODES; do
 engine=docker
 location=local
 access=operator
 target=$BENCH_SSH
 transport=load
 direct=false
 builder=${BENCH_BUILDER:-unused}
 case "$mode" in
 pussh)
  if ! docker pussh --help >/dev/null 2>&1; then
   printf '%s,-,-,-,-,SKIP: docker pussh missing\n' "$mode" >> "$BENCH_OUTPUT/results.csv"; continue
  fi
  transport=pussh;;
 gzip) ;;
 zstd)
  if ! command -v zstd >/dev/null 2>&1 || ! ssh -o BatchMode=yes "$BENCH_SSH" 'command -v zstd' >/dev/null; then
   printf '%s,-,-,-,-,SKIP: zstd missing\n' "$mode" >> "$BENCH_OUTPUT/results.csv"; continue
  fi;;
 container)
  if [ "$(uname -s)" != Darwin ] || ! command -v container >/dev/null 2>&1 || ! container system status >/dev/null 2>&1; then
   printf '%s,-,-,-,-,SKIP: Apple container unavailable\n' "$mode" >> "$BENCH_OUTPUT/results.csv"; continue
  fi
  engine=container;;
 direct)
  if [ -z "${BENCH_BUILDER:-}" ]; then
   printf '%s,-,-,-,-,SKIP: BENCH_BUILDER unset\n' "$mode" >> "$BENCH_OUTPUT/results.csv"; continue
  fi
  location=remote; direct=true;;
 gate)
  if [ -z "${BENCH_GATE_SSH:-}" ]; then
   printf '%s,-,-,-,-,SKIP: BENCH_GATE_SSH unset\n' "$mode" >> "$BENCH_OUTPUT/results.csv"; continue
  fi
  access=scoped; target=$BENCH_GATE_SSH;;
 *) echo "Unknown benchmark mode: $mode" >&2; exit 1;;
 esac
 cat > "$work/yoho.json" <<CONFIG
{"app":"$BENCH_APP","servers":{"bench":{"ssh":"$target"}},"destinations":{"production":{"servers":["bench"],"access":"$access"}},"builder":{"location":"$location","engine":"$engine","platforms":["$BENCH_PLATFORM"],"remote":"$builder","direct":"$direct"},"transport":{"mode":"$transport"}}
CONFIG
 dd if=/dev/urandom of="$work/base" bs=1048576 count=496 2>/dev/null
 for phase in cold warm; do
  dd if=/dev/urandom of="$work/change" bs=1048576 count=4 2>/dev/null
  version=bench-$run_id-$mode-$phase
  image=yoho/$BENCH_APP-production-web:$version
  log=$BENCH_OUTPUT/$mode-$phase.log
  scope=deploy-including-build
  # Archive baselines prebuild, then time compression + transfer + docker load.
  if [ "$mode" = gzip ] || [ "$mode" = zstd ]; then
   docker build --load --platform "$BENCH_PLATFORM" -t "$image" "$work" > "$log" 2>&1
   docker save --platform "$BENCH_PLATFORM" -o "$work/image.tar" "$image" >> "$log" 2>&1
   scope=compress-transfer-load
  fi
  before=$(rx)
  start=$(date +%s)
  status=OK
  case "$mode" in
  gzip)
   gzip -1 -c "$work/image.tar" > "$work/image.gz"
   if ! ssh -o BatchMode=yes "$target" 'set -eu; docker load' < "$work/image.gz" >> "$log" 2>&1; then status=FAIL; fi;;
  zstd)
   zstd -1 -T0 -q -c "$work/image.tar" > "$work/image.zst"
   if ! ssh -o BatchMode=yes "$target" 'set -eu; zstd -dc | docker load' < "$work/image.zst" >> "$log" 2>&1; then status=FAIL; fi;;
  *)
   if ! (cd "$work" && "$YOHO" --json deploy --version "$version") > "$log" 2>&1; then status=FAIL; fi;;
  esac
  stop=$(date +%s)
  after=$(rx)
  printf '%s,%s,%s,%s,%s,%s\n' "$mode" "$phase" "$((after-before))" "$((stop-start))" "$scope" "$status" >> "$BENCH_OUTPUT/results.csv"
  rm -f "$work/image.tar" "$work/image.gz" "$work/image.zst"
  if [ "$status" != OK ]; then echo "Failed $mode/$phase; see $log" >&2; exit 1; fi
 done
done
cat "$BENCH_OUTPUT/results.csv"
echo "Logs and results: $BENCH_OUTPUT" >&2
