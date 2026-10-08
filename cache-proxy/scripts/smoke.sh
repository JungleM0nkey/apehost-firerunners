#!/usr/bin/env bash
# End-to-end smoke test against the real workerd binary.
#
# Builds the bundle, derives a config from deploy/config.capnp (loopback ports, temp dirs, a local
# fake upstream, small LRU cap), then exercises the HTTP API with curl: write-behind upload,
# HEAD/GET hits, read-through, auth, LRU eviction, metrics on the loopback-only socket, and a
# pending upload that survives a workerd restart.
set -euo pipefail

cd "$(dirname "$0")/.."
WORKERD=${WORKERD:-node_modules/.bin/workerd}
T=$(mktemp -d "${TMPDIR:-/tmp}/cache-proxy-smoke.XXXXXX")
PIDS=()
cleanup() {
  for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null || true; done
  wait 2>/dev/null || true
  if [ "${KEEP:-0}" = 1 ]; then echo "kept $T"; else rm -rf "$T"; fi
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*" >&2
  echo "--- workerd log" >&2
  tail -n 50 "$T"/workerd*.log >&2 || true
  exit 1
}
ok() { echo "ok - $*"; }

free_port() { node -e 'const s=require("net").createServer();s.listen(0,"127.0.0.1",()=>{console.log(s.address().port);s.close()})'; }
API_PORT=$(free_port)
MET_PORT=$(free_port)
UP_PORT=$(free_port)
API="http://127.0.0.1:$API_PORT/v8/artifacts"
MET="http://127.0.0.1:$MET_PORT"
UP="http://127.0.0.1:$UP_PORT"
MAX_BYTES=2500

export UPSTREAM_TOKEN=smoke-upstream-token TOKENS_RW=smoke-rw,smoke-rw-2 TOKENS_RO=smoke-ro

npm run --silent build
cp dist/worker.js "$T/worker.js"
mkdir -p "$T/cache/artifacts" "$T/cache/do"
sed -e "s#10.200.0.1:8787#127.0.0.1:$API_PORT#" \
    -e "s#127.0.0.1:9787#127.0.0.1:$MET_PORT#" \
    -e "s#/var/lib/fireactions/turbocache#$T/cache#g" \
    -e "s#https://turbo.apehost.net#$UP#" \
    -e "s#\\\\\"maxBytes\\\\\":[0-9]*#\\\\\"maxBytes\\\\\":$MAX_BYTES#" \
    -e 's#allow = \["public"\]#allow = ["public", "local"]#' \
    deploy/config.capnp > "$T/config.capnp"
grep -q "127.0.0.1:$UP_PORT" "$T/config.capnp" || fail "config rewrite (upstream)"
grep -q "maxBytes\\\\\":$MAX_BYTES" "$T/config.capnp" || fail "config rewrite (maxBytes)"

node scripts/fake-upstream.mjs "$UP_PORT" "$UPSTREAM_TOKEN" >"$T/upstream.log" 2>&1 &
PIDS+=($!)

WORKERD_PID=
start_workerd() {
  "$WORKERD" serve "$T/config.capnp" --verbose >"$T/workerd-$1.log" 2>&1 &
  WORKERD_PID=$!
  PIDS+=("$WORKERD_PID")
  for _ in $(seq 100); do
    curl -fsS "$MET/healthz" >/dev/null 2>&1 && return 0
    kill -0 "$WORKERD_PID" 2>/dev/null || fail "workerd exited"
    sleep 0.1
  done
  fail "workerd did not come up"
}

# curl helpers: status code only / headers only
code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }
hdr() { curl -s -o /dev/null -D - "$@" | tr -d '\r'; }
RW=(-H "Authorization: Bearer smoke-rw")
RO=(-H "Authorization: Bearer smoke-ro")
UPH=(-H "Authorization: Bearer $UPSTREAM_TOKEN")
metric() { curl -fsS "$MET/metrics" | awk -v k="$1" '$1==k {print $2; f=1} END {if(!f) print 0}'; }
until_true() { # until_true <what> <command...>
  local what=$1; shift
  for _ in $(seq 100); do "$@" && return 0; sleep 0.1; done
  fail "timed out: $what"
}
put() { # put <hash> <file> [query] [tag]
  curl -s -o /dev/null -w '%{http_code}' -X PUT "${RW[@]}" -H 'Content-Type: application/octet-stream' \
    -H 'x-artifact-duration: 321' ${4:+-H "x-artifact-tag: $4"} --data-binary "@$2" "$API/$1${3:-}"
}
upstream_has() { [ "$(code "$UP/__control/object?key=$1")" = 200 ]; }

start_workerd 1
ok "workerd started with the derived config"

# --- auth and fixed endpoints
[ "$(code "$API/status")" = 401 ] || fail "no token should be 401"
[ "$(code -H 'Authorization: Bearer nope' "$API/status")" = 401 ] || fail "bad token should be 401"
[ "$(curl -fsS "${RO[@]}" "$API/status")" = '{"status":"enabled"}' ] || fail "status"
[ "$(code -X POST "${RW[@]}" -d '[]' "$API/events")" = 200 ] || fail "events"
ok "auth + status + events"

# --- write-behind PUT, local HEAD/GET hits
head -c 1000 /dev/urandom >"$T/a.bin"
[ "$(put aaaa0001 "$T/a.bin" "" dGFnLWE=)" = 202 ] || fail "PUT a"
[ "$(code -X PUT "${RO[@]}" -H 'Content-Type: application/octet-stream' --data-binary "@$T/a.bin" "$API/aaaa0001")" = 403 ] || fail "RO PUT should be 403"
H=$(hdr -I "${RW[@]}" "$API/aaaa0001")
grep -qi '^x-artifact-tag: dGFnLWE=$' <<<"$H" || fail "HEAD tag replay: $H"
grep -qi '^x-artifact-duration: 321$' <<<"$H" || fail "HEAD duration replay"
grep -qi '^x-cache-proxy: local-hit$' <<<"$H" || fail "HEAD local hit"
curl -fsS "${RW[@]}" "$API/aaaa0001" -o "$T/a.out"
cmp -s "$T/a.bin" "$T/a.out" || fail "GET bytes differ"
[ -f "$T/cache/artifacts/team_default_team/aaaa0001" ] || fail "artifact not on disk"
until_true "upload of a" upstream_has team_default_team/aaaa0001
curl -fsS "$UP/__control/object?key=team_default_team/aaaa0001" -o "$T/a.up"
cmp -s "$T/a.bin" "$T/a.up" || fail "upstream bytes differ"
curl -fsS "$UP/__control/log" | node -e '
  const log = JSON.parse(require("fs").readFileSync(0, "utf8"));
  const put = log.find((r) => r.method === "PUT" && r.path.endsWith("/aaaa0001"));
  if (!put || put.authorization !== "Bearer smoke-upstream-token" || put.tag !== "dGFnLWE=") process.exit(1);
' || fail "upload must use the proxy token and carry the tag"
ok "write-behind PUT, local HEAD/GET, upload with proxy token + tag"

# --- read-through with slug namespace
head -c 1000 /dev/urandom >"$T/b.bin"
[ "$(code -X PUT "${UPH[@]}" -H 'Content-Type: application/octet-stream' -H 'x-artifact-tag: dGFnLWI=' \
  --data-binary "@$T/b.bin" "$UP/v8/artifacts/bbbb0001?slug=smoke")" = 202 ] || fail "seed upstream"
[ "$(code "${RW[@]}" "$API/bbbb0001")" = 404 ] || fail "other namespace must miss"
H=$(curl -fsS -D - -o "$T/b.out" "${RW[@]}" "$API/bbbb0001?slug=smoke" | tr -d '\r')
cmp -s "$T/b.bin" "$T/b.out" || fail "read-through bytes differ"
grep -qi '^x-artifact-tag: dGFnLWI=$' <<<"$H" || fail "read-through tag"
until_true "read-through copy" test -f "$T/cache/artifacts/smoke/bbbb0001"
until_true "index entry" bash -c "curl -s -I -H 'Authorization: Bearer smoke-rw' '$API/bbbb0001?slug=smoke' | grep -qi 'x-cache-proxy: local-hit'"
curl -fsS "${RW[@]}" "$API/bbbb0001?slug=smoke" -o "$T/b.out2"
cmp -s "$T/b.bin" "$T/b.out2" || fail "local copy differs"
GETS=$(curl -fsS "$UP/__control/log" | node -e '
  const log = JSON.parse(require("fs").readFileSync(0, "utf8"));
  console.log(log.filter((r) => r.method === "GET" && r.path.endsWith("/bbbb0001") && r.query.slug === "smoke").length);')
[ "$GETS" = 1 ] || fail "expected exactly one upstream GET, got $GETS"
ok "read-through stores a copy; second GET served locally; slug forwarded"

# --- metrics only on the loopback socket
[ "$(code "${RW[@]}" "http://127.0.0.1:$API_PORT/metrics")" = 404 ] || fail "metrics must not be on the API socket"
[ "$(metric 'turbo_proxy_local_hits_total{method="GET"}')" -ge 2 ] || fail "local GET hits"
[ "$(metric 'turbo_proxy_upstream_hits_total{method="GET"}')" = 1 ] || fail "upstream GET hits"
[ "$(metric 'turbo_proxy_misses_total{method="GET"}')" = 1 ] || fail "GET misses"
[ "$(metric turbo_proxy_uploads_total)" = 1 ] || fail "uploads"
[ "$(metric turbo_proxy_bytes)" = 2000 ] || fail "bytes gauge"
ok "metrics on loopback socket"

# --- LRU eviction at the cap (2500 bytes): touch a, then add c => b (LRU) goes, a stays
curl -fsS "${RW[@]}" "$API/aaaa0001" -o /dev/null
head -c 1000 /dev/urandom >"$T/c.bin"
[ "$(put cccc0001 "$T/c.bin")" = 202 ] || fail "PUT c"
until_true "upload of c" upstream_has team_default_team/cccc0001
until_true "eviction" test "$(metric turbo_proxy_evictions_total)" = 1
[ ! -e "$T/cache/artifacts/smoke/bbbb0001" ] || fail "LRU victim should be b"
[ -f "$T/cache/artifacts/team_default_team/aaaa0001" ] || fail "recently used a must stay"
[ "$(metric turbo_proxy_bytes)" = 2000 ] || fail "bytes after eviction"
ok "LRU eviction removed the least recently used artifact from disk"

# --- pending upload survives a restart
curl -fsS "$UP/__control/fail-puts?on=1" >/dev/null
head -c 100 /dev/urandom >"$T/d.bin"
[ "$(put dddd0001 "$T/d.bin" "?teamId=team_smoke" dGFnLWQ=)" = 202 ] || fail "PUT d"
until_true "failed upload" test "$(metric turbo_proxy_upload_failures_total)" -ge 1
[ "$(metric turbo_proxy_pending_uploads)" = 1 ] || fail "pending marker"
kill "$WORKERD_PID"; wait "$WORKERD_PID" 2>/dev/null || true
curl -fsS "$UP/__control/fail-puts?on=0" >/dev/null
start_workerd 2
until_true "retry after restart" upstream_has team_smoke/dddd0001
until_true "pending cleared" test "$(metric turbo_proxy_pending_uploads)" = 0
curl -fsS -D - "$UP/__control/object?key=team_smoke/dddd0001" -o "$T/d.up" | tr -d '\r' | grep -qi '^x-artifact-tag: dGFnLWQ=$' || fail "retried upload tag"
cmp -s "$T/d.bin" "$T/d.up" || fail "retried upload bytes"
ok "pending upload retried after workerd restart"

# --- chunked PUT (no Content-Length) still lands on disk and upstream with a known length
head -c 300 /dev/urandom >"$T/e.bin"
[ "$(curl -s -o /dev/null -w '%{http_code}' -X PUT "${RW[@]}" -H 'Content-Type: application/octet-stream' \
  -H 'Transfer-Encoding: chunked' --data-binary "@$T/e.bin" "$API/eeee0001")" = 202 ] || fail "chunked PUT"
curl -fsS "${RW[@]}" "$API/eeee0001" -o "$T/e.out"
cmp -s "$T/e.bin" "$T/e.out" || fail "chunked PUT bytes"
until_true "upload of chunked PUT" upstream_has team_default_team/eeee0001
ok "chunked PUT"

echo "smoke: all checks passed"
