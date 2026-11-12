#!/bin/sh
# Usage: scripts/custom-domains-scale.sh <server-ip> <N> [concurrency]
# Requires a public IPv4 Server with an App deployed by Yoho:
# tls: true, tls_staging: true, custom_domains.allow accepts *.sslip.io.
# See examples/custom-domains-allow/README.md. Ports 80/443 must be reachable.
# SSH uses your normal config/user for the IP. Read access to the Docker socket
# and root (or sudo -n du/find) is needed to inspect yoho-proxy-config.
# Only inspects yoho-proxy and its existing volume; never starts a container,
# mounts a volume, removes certificates, or changes Proxy configuration.
# Use fresh hostnames/Server for cold issuance: repeat runs measure cached TLS.
# Local dependencies: curl, ssh, awk, sort, xargs and basic POSIX utilities.
set -eu
LC_ALL=C
export LC_ALL

if [ "$#" -lt 2 ] || [ "$#" -gt 3 ]; then
    echo "Usage: $0 <server-ip> <N> [concurrency]" >&2
    exit 2
fi
server=$1
count=$2
concurrency=${3:-5}
if ! awk -v ip="$server" 'BEGIN { n=split(ip,a,"."); if(n!=4)exit 1; for(i=1;i<=n;i++)if(a[i]!~/^[0-9]+$/ || a[i]>255)exit 1 }'; then
    echo "server-ip must be an IPv4 address" >&2
    exit 2
fi
for value in "$count" "$concurrency"; do
    case "$value" in ''|*[!0-9]*) echo "N and concurrency must be positive integers" >&2; exit 2;; esac
    if ! awk -v n="$value" 'BEGIN {exit !(n>0)}'; then
        echo "N and concurrency must be positive integers" >&2
        exit 2
    fi
done
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT HUP INT TERM
rss_before=$(ssh -o BatchMode=yes "$server" 'docker stats --no-stream --format "{{.MemUsage}}" yoho-proxy')
start=$(date +%s)
dashed=$(printf '%s' "$server" | awk '{gsub(/\./,"-"); print}')
: > "$work/results"
export work dashed
awk -v n="$count" 'BEGIN {for(i=1;i<=n;i++)print i}' |
    xargs -P "$concurrency" -n 1 sh -c '
        host="c$1-$dashed.sslip.io"
        # curl writes its timing even on failure; keep exactly one record.
        result=$(curl --noproxy "*" -sk -o /dev/null --max-time 60 -w "%{http_code} %{time_total}" "https://$host/") || :
        printf "%s %s\n" "$host" "$result" >> "$work/results"
    ' sh
end=$(date +%s)
rss_after=$(ssh -o BatchMode=yes "$server" 'docker stats --no-stream --format "{{.MemUsage}}" yoho-proxy')
# Inspect the existing volume directly: docker run -v could create a volume.
ssh -o BatchMode=yes "$server" 'sh -s' > "$work/volume" <<'REMOTE'
set -eu
path=$(docker volume inspect --format '{{.Mountpoint}}' yoho-proxy-config)
case "$path" in /*) ;; *) echo "invalid volume mountpoint" >&2; exit 1;; esac
if [ "$(id -u)" = 0 ]; then
    du -sh "$path"
    files=$(find "$path" -type f -path "$path/certs/*")
    printf '%s\n' "$files" | awk 'NF {n++} END {print "cert files: " n+0}'
else
    sudo -n du -sh "$path"
    files=$(sudo -n find "$path" -type f -path "$path/certs/*")
    printf '%s\n' "$files" | awk 'NF {n++} END {print "cert files: " n+0}'
fi
REMOTE
awk '$2 ~ /^2[0-9][0-9]$/ {print $3}' "$work/results" | sort -n > "$work/times"
stats=$(awk '
{a[NR]=$1}
END {
 if (!NR) {print "0 | n/a | n/a | n/a"; exit}
 p50=int((NR*50+99)/100); p95=int((NR*95+99)/100)
 printf "%d | %.3fs | %.3fs | %.3fs",NR,a[p50],a[p95],a[NR]
}' "$work/times")
size=$(awk 'NR==1 {print $1}' "$work/volume")
certs=$(awk '/^cert files:/ {print $3}' "$work/volume")
printf '| N | Success (2xx) | p50 | p95 | Max | Wall | Proxy RSS before | Proxy RSS after | Volume | Cert files |\n'
printf '|---|---|---|---|---|---|---|---|---|---|\n'
printf '| %s | %s | %ss | %s | %s | %s | %s |\n' "$count" "$stats" "$((end-start))" "$rss_before" "$rss_after" "$size" "$certs"
# Percentiles use successful HTTP responses only; curl time includes TLS + HTTP.
