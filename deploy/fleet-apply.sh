#!/bin/bash
# Install one host's rendered files and restart only the units whose files changed (#16).
#
#   fireactions fleet render -f deploy/fleet.yaml -o rendered/   # with FLEET_* secrets in env
#   deploy/fleet-apply.sh podbox rendered/podbox [--dry-run]
#
# Runs from your machine over ssh (passwordless sudo on the host). Before restarting
# fireactions it drains every pool and waits for running jobs to finish, because a
# restart stops all VMs. The cache proxy restart only costs in-flight cache requests.
# Also installs the cache proxy Worker bundle (cache-proxy/dist/worker.js) when the
# host has the cache enabled; build it first with `npm --prefix cache-proxy run build`.
set -euo pipefail

HOST=${1:?usage: $0 HOST RENDERED_DIR [--dry-run]}
DIR=${2:?usage: $0 HOST RENDERED_DIR [--dry-run]}
DRY=${3:-}
API=${FIREACTIONS_API:-127.0.0.1:18080}
DRAIN_TIMEOUT=${DRAIN_TIMEOUT:-3600}
ROOT=$(cd "$(dirname "$0")/.." && pwd)

[ -f "$DIR/fireactions.yaml" ] || { echo "$DIR/fireactions.yaml not found" >&2; exit 1; }

# rendered name -> installed path, mode, unit to restart
declare -A DEST MODE UNIT
DEST[fireactions.yaml]=/etc/fireactions/config.yaml;              MODE[fireactions.yaml]=600; UNIT[fireactions.yaml]=fireactions
DEST[cache-proxy.capnp]=/opt/fireactions/cache-proxy/config.capnp; MODE[cache-proxy.capnp]=644; UNIT[cache-proxy.capnp]=fireactions-cache-proxy
DEST[cache-proxy.env]=/etc/fireactions/cache-proxy.env;            MODE[cache-proxy.env]=600;   UNIT[cache-proxy.env]=fireactions-cache-proxy
DEST[fireactions-cache-proxy.service]=/etc/systemd/system/fireactions-cache-proxy.service
MODE[fireactions-cache-proxy.service]=644; UNIT[fireactions-cache-proxy.service]=fireactions-cache-proxy

files=()
for f in "$DIR"/*; do
  name=$(basename "$f")
  case "$name" in
    *.mount)
      DEST[$name]=/etc/systemd/system/$name; MODE[$name]=644; UNIT[$name]=$name ;;
  esac
  [ -n "${DEST[$name]:-}" ] || { echo "don't know where $name goes" >&2; exit 1; }
  files+=("$name")
done

if [ -f "$DIR/cache-proxy.capnp" ]; then
  worker="$ROOT/cache-proxy/dist/worker.js"
  [ -f "$worker" ] || { echo "$worker missing: npm --prefix cache-proxy run build" >&2; exit 1; }
  cp "$worker" "$DIR/worker.js"
  DEST[worker.js]=/opt/fireactions/cache-proxy/worker.js; MODE[worker.js]=644; UNIT[worker.js]=fireactions-cache-proxy
  files+=(worker.js)
fi

stage=$(ssh "$HOST" mktemp -d)
trap 'ssh "$HOST" rm -rf "$stage"; rm -f "$DIR/worker.js"' EXIT
scp -q "${files[@]/#/$DIR/}" "$HOST:$stage/"

# Which files differ from what's installed?
changed=()
for name in "${files[@]}"; do
  if ! ssh "$HOST" sudo cmp -s "$stage/$name" "${DEST[$name]}"; then
    changed+=("$name")
  fi
done

if [ ${#changed[@]} -eq 0 ]; then
  echo "$HOST: up to date"; exit 0
fi

declare -A restart
for name in "${changed[@]}"; do
  echo "$HOST: ${DEST[$name]} changed"
  restart[${UNIT[$name]}]=1
done

if [ "$DRY" = "--dry-run" ]; then
  echo "$HOST: would restart: ${!restart[*]}"; exit 0
fi

# Drain before restarting fireactions: pause every pool, wait until no VMs are left.
if [ -n "${restart[fireactions]:-}" ] && ssh "$HOST" systemctl is-active -q fireactions; then
  pools=$(ssh "$HOST" fireactions pools list -e "$API" | awk 'NR > 1 && NF {print $1}')
  for p in $pools; do ssh "$HOST" fireactions pools pause "$p" -e "$API" >/dev/null; done
  echo "$HOST: draining pools ($pools); waiting for running jobs (up to ${DRAIN_TIMEOUT}s)"
  deadline=$((SECONDS + DRAIN_TIMEOUT))
  while [ "$(ssh "$HOST" fireactions ps -e "$API" | awk 'NR > 1 && NF' | wc -l)" -gt 0 ]; do
    if [ $SECONDS -ge $deadline ]; then
      for p in $pools; do ssh "$HOST" fireactions pools resume "$p" -e "$API" >/dev/null; done
      echo "$HOST: jobs still running after ${DRAIN_TIMEOUT}s; resumed pools, nothing applied" >&2
      exit 1
    fi
    sleep 10
  done
fi

for name in "${changed[@]}"; do
  ssh "$HOST" sudo install -D -m "${MODE[$name]}" -o root -g root "$stage/$name" "${DEST[$name]}"
done
ssh "$HOST" sudo systemctl daemon-reload

# A changed mount means a new tmpfs (cold cache): stop the proxy that holds it, remount,
# then start the proxy again. Then fireactions last.
mounts=()
for unit in "${!restart[@]}"; do
  case "$unit" in *.mount) mounts+=("$unit"); restart[fireactions-cache-proxy]=1 ;; esac
done
if [ ${#mounts[@]} -gt 0 ]; then
  ssh "$HOST" sudo systemctl stop fireactions-cache-proxy 2>/dev/null || true
  for unit in "${mounts[@]}"; do
    ssh "$HOST" sudo systemctl enable -q "$unit"
    ssh "$HOST" sudo systemctl restart "$unit"
    echo "$HOST: remounted $unit"
  done
fi
for unit in fireactions-cache-proxy fireactions; do
  if [ -n "${restart[$unit]:-}" ]; then
    ssh "$HOST" sudo systemctl enable -q "$unit"
    ssh "$HOST" sudo systemctl restart "$unit"
    echo "$HOST: restarted $unit"
  fi
done
