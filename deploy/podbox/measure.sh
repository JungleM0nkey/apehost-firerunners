#!/bin/bash
# Memory budget measurements for #11. Read-only: changes nothing on the host.
#
#   sudo deploy/podbox/measure.sh snapshot          # one CSV row (with header)
#   sudo deploy/podbox/measure.sh watch [secs] > build.csv   # a row every secs (default 2) until Ctrl-C
#   sudo deploy/podbox/measure.sh cycles [n] > cycles.csv     # a row every time a VM exits, n exits (default 20)
#
# Columns (MiB unless noted):
#   ts, mem_total, mem_free, mem_available, shmem (all tmpfs incl. the thin-pool),
#   thinpool_tmpfs_used (df of the tmpfs holding the loop files),
#   thinpool_data_used_pct (dmsetup: used/total data blocks),
#   vms (running firecracker processes), fc_rss_total, fc_rss_max (per VMM)
set -euo pipefail

CTR_ROOT=/var/lib/fireactions-containerd
POOL=fireactions-thinpool

header() {
  echo "ts,mem_total,mem_free,mem_available,shmem,thinpool_tmpfs_used,thinpool_data_used_pct,vms,fc_rss_total,fc_rss_max"
}

row() {
  local mem_total mem_free mem_avail shmem tmpfs_used data_pct vms rss_total rss_max
  read -r mem_total mem_free mem_avail shmem < <(awk '
    /^MemTotal:/ {t=$2} /^MemFree:/ {f=$2} /^MemAvailable:/ {a=$2} /^Shmem:/ {s=$2}
    END {printf "%d %d %d %d\n", t/1024, f/1024, a/1024, s/1024}' /proc/meminfo)

  tmpfs_used=$(df -m --output=used "$CTR_ROOT" 2>/dev/null | tail -1 | tr -d ' ' || echo "")

  # thin-pool status: <start> <len> thin-pool <txid> <used_meta>/<total_meta> <used_data>/<total_data> ...
  data_pct=$(dmsetup status "$POOL" 2>/dev/null | awk '{split($6, d, "/"); if (d[2] > 0) printf "%.1f", 100 * d[1] / d[2]}' || echo "")

  read -r vms rss_total rss_max < <(ps -C firecracker -o rss= 2>/dev/null | awk '
    {n++; t+=$1; if ($1 > m) m=$1} END {printf "%d %d %d\n", n, t/1024, m/1024}')

  echo "$(date -u +%FT%TZ),$mem_total,$mem_free,$mem_avail,$shmem,$tmpfs_used,$data_pct,$vms,$rss_total,$rss_max"
}

case "${1:-snapshot}" in
  snapshot)
    header; row ;;
  watch)
    header
    while true; do row; sleep "${2:-2}"; done ;;
  cycles)
    # One row now, then one each time the number of VMs drops (a VM exited).
    want=${2:-20}
    seen=0
    header; row
    prev=$(pgrep -c -x firecracker || true)
    while [ "$seen" -lt "$want" ]; do
      sleep 1
      cur=$(pgrep -c -x firecracker || true)
      if [ "$cur" -lt "$prev" ]; then
        seen=$((seen + prev - cur))
        row
      fi
      prev=$cur
    done ;;
  *)
    echo "usage: $0 snapshot | watch [secs] | cycles [n]" >&2; exit 2 ;;
esac
