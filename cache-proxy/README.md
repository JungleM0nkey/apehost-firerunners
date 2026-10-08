# cache-proxy

A Turborepo remote-cache proxy that runs on each CI host. Firecracker VMs use it as
`TURBO_API=http://10.200.0.1:8787`. It is a single Worker (TypeScript, bundled to one ES module)
running on self-hosted [workerd](https://github.com/cloudflare/workerd).

- **Reads** are answered from local disk when possible. Otherwise they go to the upstream cache
  (`https://turbo.apehost.net`, the R2-backed Worker forked from
  `AdiRishi/turborepo-remote-cache-cloudflare`). A `GET` that hits upstream is streamed to the
  client and copied to disk at the same time.
- **Writes** are write-behind. The bytes are stored locally and the proxy answers `202`. The
  upload to upstream then runs in the background with the proxy's own token. A failed upload
  stays *pending*. It is retried by a maintenance alarm and after a restart.
- **LRU eviction** keeps the disk under `maxBytes`. An artifact still waiting to upload is never
  evicted.
- **Metrics** (Prometheus text) are served only on a separate loopback socket.

For the podbox install, see [`deploy/podbox/cache-proxy.md`](../deploy/podbox/cache-proxy.md).

## Development

```sh
npm ci
npm run typecheck   # tsc for the Worker/tests and for the Node-side files
npm test            # vitest inside workerd (@cloudflare/vitest-plugin) with fake disk + upstream
npm run build       # esbuild -> dist/worker.js (single ES module)
npm run smoke       # build, run the real workerd binary on a derived config, curl round-trips
```

`npm run smoke` needs `curl` and `node`. Set `KEEP=1` to keep its temp directory.

## HTTP API (main socket)

Every request needs `Authorization: Bearer <token>`. A missing or unknown token gets `401` on
every path.

| Request | Behaviour |
|---|---|
| `HEAD /v8/artifacts/{hash}` | Local hit: `200` with the stored headers. Miss: `HEAD` upstream, relay `200` and the artifact headers, or `404`. Nothing is downloaded |
| `GET /v8/artifacts/{hash}` | Local hit: the bytes are streamed from disk. Miss: `GET` upstream; on `200` the body is streamed to the client and a copy is stored. Upstream `404` gives `404` |
| `PUT /v8/artifacts/{hash}` | Needs an `rw` token (`ro` gets `403`). Stored locally, then `202 {"urls":[…]}`, then uploaded upstream |
| `POST /v8/artifacts` | `{"hashes":[…]}` returns `{hash: {taskDurationMs, sha?, dirtyHash?}}` for **local** entries only. Other hashes are left out, so turbo falls back to `HEAD` (which goes upstream) |
| `GET /v8/artifacts/status` | `{"status":"enabled"}`, answered locally |
| `POST /v8/artifacts/events` | `200 {}`, answered locally and not forwarded |

- **Headers.** These are stored per artifact and replayed verbatim on hits: `x-artifact-duration`,
  `x-artifact-tag`, `x-artifact-sha` and `x-artifact-dirty-hash`. The upload also forwards
  `x-artifact-client-ci` and `x-artifact-client-interactive`. Bytes are never re-encoded.
  Responses carry `x-cache-proxy: local-hit|upstream-hit` for debugging.
- **Namespacing.** This matches upstream, which keys objects as
  `${teamId ?? slug ?? "team_default_team"}/${hash}`. The proxy uses the same namespace on disk.
  It forwards the `teamId`/`slug` query parameters unchanged and drops any other parameter.
- **Validation.** Both the namespace and the hash must match `^[A-Za-z0-9_-]{1,128}$`, or the
  request gets `400`. Dots and slashes are rejected, so nothing can escape the disk service's
  directory.
- **Upstream failures.** Network errors and statuses other than 200/404 are answered as a miss
  (`404`) and counted in `turbo_proxy_upstream_errors_total`. The build continues without the
  cache instead of failing.
- **Not supported.** `OPTIONS` preflight is not handled, so leave `TURBO_PREFLIGHT` unset.

## Metrics socket (`Metrics` entrypoint)

| Path | Response |
|---|---|
| `GET /metrics` | Prometheus text |
| `GET /healthz` | `ok`. This also wakes the index so that pending uploads resume right after a start |

Metrics:

| Name | Type | Labels |
|---|---|---|
| `turbo_proxy_local_hits_total` | counter | `method` = GET or HEAD |
| `turbo_proxy_upstream_hits_total` | counter | `method` |
| `turbo_proxy_misses_total` | counter | `method` |
| `turbo_proxy_upstream_errors_total` | counter | none |
| `turbo_proxy_stored_bytes_total` | counter | none |
| `turbo_proxy_uploads_total` | counter | none |
| `turbo_proxy_upload_failures_total` | counter | none |
| `turbo_proxy_evictions_total` | counter | none |
| `turbo_proxy_evicted_bytes_total` | counter | none |
| `turbo_proxy_auth_failures_total` | counter | none |
| `turbo_proxy_bytes` | gauge | none |
| `turbo_proxy_artifacts` | gauge | none |
| `turbo_proxy_pending_uploads` | gauge | none |
| `turbo_proxy_max_bytes` | gauge | none |

Counters are kept in the index's SQLite database. They survive a process restart and reset on
reboot, because the tmpfs is wiped.

## workerd binding contract

The Worker module is `worker.js` (`dist/worker.js` after `npm run build`). It exports `default`
(the API), `Metrics` (a `WorkerEntrypoint`) and `CacheIndex` (a Durable Object class).

| Binding | workerd binding type | Value |
|---|---|---|
| `DISK` | `service` | A `disk` service with `writable = true`. Artifact bytes are stored as `<namespace>/<hash>` |
| `UPSTREAM` | `service` | Used for every upstream request with absolute URLs. In production this is a `network` service with `allow = ["public"]` and `tlsOptions.trustBrowserCas = true` |
| `INDEX` | `durableObjectNamespace` | Class `CacheIndex`. Needs `enableSql = true` and `durableObjectStorage = (localDisk = "<disk service>")` |
| `SETTINGS` | `json` | Non-secret settings, see below |
| `UPSTREAM_TOKEN` | `fromEnvironment = "UPSTREAM_TOKEN"` | The bearer token the proxy presents upstream. Client tokens are never forwarded |
| `TOKENS_RW` | `fromEnvironment = "TOKENS_RW"` | Comma-separated read-write client tokens |
| `TOKENS_RO` | `fromEnvironment = "TOKENS_RO"` | Comma-separated read-only client tokens |

`SETTINGS` schema:

```json
{
  "upstream": "https://turbo.apehost.net",
  "maxBytes": 4294967296,
  "maintenanceIntervalSeconds": 60
}
```

| Key | Required | Default | Meaning |
|---|---|---|---|
| `upstream` | yes | none | http(s) base URL of the upstream cache, without the `/v8` path |
| `maxBytes` | no | 4 GiB | LRU cap on artifact bytes |
| `maintenanceIntervalSeconds` | no | 60 | Period of the self-rescheduling alarm, which retries pending uploads and evicts down to the cap |

Sockets: the default entrypoint goes on the bridge (`10.200.0.1:8787`). The `Metrics`
entrypoint goes on loopback (`127.0.0.1:9787`) only.

Token comparison hashes both sides with SHA-256 and compares every configured token with
`crypto.subtle.timingSafeEqual`, with no early exit. If a token is listed as both `rw` and `ro`,
`rw` wins.

Index storage is a single `CacheIndex` instance, `idFromName("index")`. Its SQLite tables are:

- `artifacts(ns, hash, size, last_access, pending, headers, query)`. `headers` holds the stored
  `x-artifact-*` headers as JSON. `query` holds the original `teamId`/`slug` query, which retries
  forward again.
- `counters(series, value)`.

The disk service stores no metadata of its own. If the index lists an artifact that the disk no
longer has, the entry is dropped and the request is handled as a miss.

[`deploy/config.capnp`](deploy/config.capnp) is the podbox configuration.
[`scripts/smoke.sh`](scripts/smoke.sh) rewrites it for loopback and temporary directories and runs
it, which proves that the config is valid for the pinned workerd version.
