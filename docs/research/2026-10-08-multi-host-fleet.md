# Multi-host fleet: research findings

Date: 2026-10-08. Scope: the "Open research questions" and the two ⚠️ blockers in
[issue #5](https://github.com/JungleM0nkey/apehost-firerunners/issues/5).
This is research only. No code was changed.

## TL;DR

1. **Blocker: gRPC from workerd. workerd cannot speak gRPC.** Its outbound HTTP client is KJ HTTP, which only writes `HTTP/1.1` request lines, and gRPC needs HTTP/2 with trailers. So (a), (b) and (c) all work, and the cheapest is **(a)**: a few `net/http` handlers on the existing metrics mux. With item 2 below, though, the remote-scaling path may not be needed at all.
2. **Runner scale-set APIs work without ARC.** GitHub publishes an official Go client, [`github.com/actions/scaleset`](https://github.com/actions/scaleset) (public preview since 2026-02-05, MIT, v0.4.0). It supports repo-level registration and GitHub App auth, and uses outbound long-poll (~50 s) with an `X-ScaleSetMaxCapacity` header. The fork is Go, so the listener can run **inside fireactions on each host**. That removes the public webhook, the scheduler Durable Object and the gRPC blocker in one move. **Recommendation: scale sets, not webhooks.** Webhooks are fire-once, with no automatic redelivery and a 10 s response deadline.
3. **Cron blocker.** Self-hosted workerd has no cron config, and its capnp `runScheduled` is unsupported. However, **Durable Object alarms do run in workerd**: they are persisted with `localDisk` storage and have a 15-minute wall limit. A systemd timer is not required. If scale sets are adopted, no polling agent is needed at all.
4. **workerd cache proxy is feasible as designed.** The `DiskDirectory` service supports GET/HEAD/PUT/DELETE, single Range requests, atomic PUT and a JSON listing. It stores **no metadata**, so tags and durations need sidecar files. The listing has no sizes or mtimes, so eviction must live in the Worker with its own index. workerd never times out `waitUntil`.
5. **Turbo protocol is pinned down from source.** The client uses `HEAD`/`GET`/`PUT /v8/artifacts/{hash}`, `POST /v8/artifacts` (dry-run batch query only, with HEAD fallback), `GET /v8/artifacts/status` and `POST /v8/artifacts/events`. `teamId` is sent only if it starts with `team_`, otherwise `slug`. `x-artifact-tag` is a base64 HMAC-SHA256 (v2 framing) that the client checks on GET only. `TURBO_CACHE=remote:r` (or `--cache=remote:r`) is the read-only mode.
6. **Fix scale-down before demand scaling.** `ScalePool` down **kills arbitrary VMs, including busy ones** (`deleteMachine` takes the first map entry and calls `StopVMM`). `PausePool` is effectively a graceful drain.
7. **Firecracker v1.17 has no virtio-fs and no 9p** (confirmed). vsock works and is already used by fireactions. A **read-only shared pmem or block device** is a new option for a warm package store.

## Sources (pinned)

| Key | Source |
|---|---|
| FA | local clone `apehost-firerunners` @ `3117938` |
| WD | `cloudflare/workerd` @ `368d74751f8c` (main, 2026-10-08) |
| KJ | `capnproto/capnproto` @ `e866bdbaa307` (branch `v2`) |
| FC | `firecracker-microvm/firecracker` @ tag `v1.17.0` (`29d66eb2`) |
| TR | `vercel/turborepo` @ tag `v2.11.7` |
| TC | `AdiRishi/turborepo-remote-cache-cloudflare` @ `b0aaab5c` (the upstream of the deployed cache Worker) |
| SS | `actions/scaleset` @ tag `v0.4.0` (`6ce02590`) |
| RUN | `actions/runner` @ tag `v2.338.0` |
| TK | `actions/toolkit` @ `fa980c4100e5` |
| GRPC | `grpc/grpc` @ `90e0af8e9af8`, `grpc/grpc-go` @ `39ebc82c352a`, `connectrpc/connect-go` @ `35bc2366084f`, `grpc-ecosystem/grpc-gateway` @ `ae471035b989` |
| PNPM | `pnpm/pnpm` @ `d3011fd4d6c2` |

---

## 1. workerd

### 1.1 Disk service, `embed`, `text`, `fromEnvironment`: **Confirmed**

- **`DiskDirectory`** [WD `src/workerd/server/workerd.capnp:984-1027`]
  - Fields: `path @0 :Text`, `writable @1 :Bool = false`, `allowDotfiles @2 :Bool = false`. `path` can instead be given as `--directory-path <service>=<path>`. Relative paths resolve against the process CWD, not the config file.
  - It is used as a service: `(name = "cache-disk", disk = (path = "/mnt/ramdisk/cache", writable = true))`.
- **Semantics.** The `DiskDirectoryService::request` handler is at [WD `src/workerd/server/server.c++:2568-2806`].
  - **GET/HEAD** on a file → 200 with `Content-Type: application/octet-stream`, `Content-Length` and `Last-Modified` (mtime). HEAD only runs `stat()`.
  - GET supports a **single** `Range` → 206, and returns 416 when the range is unsatisfiable.
  - Missing files, dotfiles (unless `allowDotfiles`) and `..` → 404. Other inode types → 406.
  - **GET on a directory** → JSON `[{"name":…,"type":"file"|"directory"|…}]`. The listing has **no size or mtime** [server.c++:2712-2763], and its length is intentionally not sent.
  - **PUT** (only when `writable`) → body streamed via `pumpTo` into `replaceFile(...)` (a temp file, then an atomic `commit()`, with parent dirs created) → **204** [server.c++:2769-2786]. Without `writable` → 405. Empty or blocked path → 403.
  - **DELETE** → `tryRemove` → 204, or 404 [server.c++:2787-2803]. Any other method → 501.
- **No custom metadata.** Only Content-Type, Content-Length and Last-Modified are served. The proxy must therefore store `x-artifact-tag`, `x-artifact-duration`, `x-artifact-sha` and `x-artifact-dirty-hash` itself, for example in a sidecar `<hash>.json` or in a Durable Object SQLite index. *(Inference from the handler code.)*
- **`embed`** is Cap'n Proto schema syntax, read when the config is parsed, for example `serviceWorkerScript = embed "worker.js"` [WD workerd.capnp:304-306]. A config change therefore needs a restart, which confirms the issue's claim. The way around it is to have the Worker re-read the file from a disk service at runtime.
- **Binding types:** `text @4 :Text`, `data @5 :Data`, `json @6 :Text` (parsed JSON), and `fromEnvironment @16 :Text`, which calls `getenv()` and gives `null` if the variable is unset [WD workerd.capnp:411-467].
  - Tip: a `json` binding avoids bundling a YAML parser if the renderer emits JSON. *(Inference.)*
- **Outbound reachability.**
  - The default `globalOutbound` is the `"internet"` network service [WD workerd.capnp:673]. `Network.allow` defaults to `["public"]`, so loopback and `10.200.0.0/24` are **blocked by default**. Use `"local"` for `127.0.0.0/8` and `"private"` for RFC 1918 ranges [WD workerd.capnp:939-969].
  - To reach a specific local service, use an `external` service: `ExternalServer.address @0`, `http @1 :HttpOptions`, `https (options, tlsOptions)` [WD workerd.capnp:881-921].

### 1.2 Cron / `scheduled` in self-hosted workerd: **Confirmed: no cron; Durable Object alarms do work**

- `workerd.capnp` has **no cron or trigger field** (grep for `cron` and `scheduled` returns nothing) [WD workerd.capnp].
- The capnp `EventDispatcher.runScheduled` and `runAlarm` throw `throwUnsupported()` for externally delivered events [WD server.c++:6630-6636].
- `/cdn-cgi/handler/scheduled` does not exist in workerd. That endpoint belongs to wrangler/miniflare, not the runtime (grep for `cdn-cgi` returns nothing).
- **Durable Object alarms are implemented in workerd.**
  - Each namespace creates an `AlarmScheduler` [WD server.c++:407-441], backed by `metadata.sqlite` in the namespace's `localDisk` directory, or in memory if there is no disk.
  - Alarms reload on startup via `loadAlarmsFromDb()` [WD `src/workerd/server/alarm-scheduler.c++:47,81`].
  - Alarm wall limit: `getAlarmLimit()` = **15 min** [WD server.c++:4758-4760].
  - So a self-rescheduling `setAlarm(Date.now()+10_000)` in a local Durable Object replaces the systemd timer.
  - Caveat: the alarm needs one initial trigger (any fetch) after a fresh deploy, because the alarm row does not exist until it is set. *(Inference.)*
- Still valid: a systemd timer running `curl http://127.0.0.1:<port>/tick`.

### 1.3 Outbound WebSocket and `ctx.waitUntil` in production mode: **Confirmed**

- **`new WebSocket(url)`** creates an outbound WebSocket ("Creates a new outbound WebSocket") [WD `src/workerd/api/web-socket.h:303-306`, `web-socket.c++:451-476`]. Only `ws`/`wss` are accepted, and they are mapped to http/https. It is not gated by a compat flag. `fetch` with `Upgrade: websocket` also works.
- **`waitUntil` has no time limit in workerd.**
  - For non-actor requests, the drain timeout comes from `limitEnforcer->limitDrain()` [WD `src/workerd/io/io-context.c++:676-685`].
  - workerd's limit enforcer returns `kj::NEVER_DONE` for `limitDrain()`, and documents "No limits are enforced" (CPU, subrequests, buffering) [WD server.c++:4741-4775].
  - Cloudflare's hosted 30 s cap does not apply to the local proxy.
  - Caveat: workerd drains on SIGTERM [WD `src/workerd/server/cli-main.c++:212-213`], but nothing persists in-flight background uploads. Write a "pending upload" marker next to the artifact and retry on start. *(Inference.)*
- The README warns that workerd "is not a hardened sandbox" (fine, since only our own code runs in it) and recommends systemd with `--socket-fd` [WD README.md:32-34,186-237].

### 1.4 Can `fetch` speak gRPC (HTTP/2 + trailers) to `127.0.0.1`? **Contradicted (no)**

- The workerd external HTTP service is built on `kj::newHttpClient(...)` [WD server.c++:2233-2237]. The KJ HTTP client serializes requests as `"HTTP/1.1"` [KJ `c++/src/kj/compat/http.c++:1157,1164,1174`]. There is no HTTP/2 option in `HttpOptions` [WD workerd.capnp:1032-1090].
- gRPC requires HTTP/2, and **status must be sent in trailers** even on success [GRPC `doc/PROTOCOL-HTTP2.md:106-118`].
- fireactions serves plain `grpc.Server.Serve(listener)` [FA `server/server.go:74-77,128,148`], which is HTTP/2 only. grpc-go's `ServeHTTP` path also needs HTTP/2 [GRPC grpc-go `server.go:1113-1120`].
- **Decision on (a), (b), (c):**
  - **(a) HTTP/JSON endpoint in the fork: cheapest.**
    - fireactions already runs a `net/http` `ServeMux` for `/metrics` [FA server.go:107-120].
    - Adding `POST /v1/pools/{name}/scale|pause|resume` that calls `pool.SetReplicas`, `Pause` and `Resume` (the same calls `rpc.go` makes [FA `server/rpc.go:48-87`]) is a small diff. The metrics bind is already loopback on podbox (`127.0.0.1:18081`). *(Inference: design.)*
  - **(b) systemd timer + CLI.** Works with no code change: `fireactions pools scale NAME --replicas N -e 127.0.0.1:18080` [FA `cmd/fireactions/pools.go:20,58-87`]. Note that the CLI defaults to `127.0.0.1:8080`, so the endpoint must be passed. workerd cannot exec, so the timer would also have to fetch the desired state.
  - **(c) grpc-gateway or Connect.**
    - grpc-gateway generates an HTTP/JSON→gRPC reverse proxy. It needs `google.api.http` annotations, or `generate_unbound_methods=true` [GRPC grpc-gateway README:20-22,246-313].
    - connect-go handlers serve the gRPC, gRPC-Web and Connect protocols, and Connect works over HTTP/1.1 [GRPC connect-go README:13-18,29-32]. But it means re-implementing handlers on connect-go.
    - Both are heavier than (a).
  - (d) Hand-rolling h2c over workerd's raw TCP `connect()` is theoretically possible but not worth it. *(Inference.)*
  - **If scale sets (§5.1) are adopted, nothing remote calls fireactions, and the blocker goes away.**

### 1.5 Durable Object storage in self-hosted workerd: **Confirmed (experimental)**

- `durableObjectNamespaces[]` take `className`, then `uniqueKey @1` or `ephemeralLocal @2`, plus `preventEviction @3` and `enableSql @4` [WD workerd.capnp:680-728].
- `durableObjectStorage :union { none @9; inMemory @10; localDisk @12 :Text }`.
  - `localDisk` names a **DiskDirectory service** and is marked "**EXPERIMENTAL; SUBJECT TO BACKWARDS-INCOMPATIBLE CHANGE**".
  - Files go to `<uniqueKey>/<id>.sqlite` (plus `-wal`/`-shm`) [WD workerd.capnp:793-818].
- Durable Objects are always local to one workerd process ("TODO(someday): Support distributing objects across a cluster") [WD workerd.capnp:822]. A fleet-wide scheduler therefore has to be a hosted Cloudflare Durable Object.
- Durable Objects are evicted after 10 s idle unless `preventEviction` is set [WD workerd.capnp:721-726]. Alarms: see §1.2. No CPU, subrequest or storage limits are enforced locally (§1.3).

### 1.6 File size limits and eviction: **Confirmed: no limit in code; eviction is ours**

- PUT streams the body to disk with `pumpTo` (no buffering, no size check) [WD server.c++:2777-2783], and GET streams back [server.c++:2690-2700]. The only cap is the tmpfs `size=`. *(Inference: there is no explicit limit anywhere in the handler.)*
- Nothing in workerd evicts or expires files. The listing gives names only, so LRU/TTL needs either a HEAD per file (to read `Content-Length` and `Last-Modified`) or **an index kept by the Worker**, for example a local Durable Object with `enableSql` storing `(hash, size, last_access)`. **Eviction has to be implemented in the Worker: confirmed.**
- `Last-Modified` is the mtime, not the atime, so LRU needs its own access tracking.

## 2. Firecracker

### 2.1 Device list (no virtio-fs or 9p) and vsock: **Confirmed**

- **Devices in v1.17.0** [FC README.md:98-123, `docs/design.md:103-115`, `src/firecracker/swagger/firecracker.yaml` paths]:
  - virtio-net, virtio-block (read-write or **read-only**), vhost-user-block (developer preview, CHANGELOG:892-898), virtio-vsock, virtio-balloon, virtio-rng (`/entropy`), **virtio-pmem** (`/pmem/{id}`, since 1.14), virtio-mem (`/hotplug/memory`), serial and i8042.
  - Hot-plugging PCI devices is a developer preview.
  - **There is no virtio-fs or 9p** in the API spec or README.
- **vsock is usable host↔guest.**
  - Host→guest: connect to the `uds_path` Unix socket, send `CONNECT <port>\n`, read `OK <hostport>\n` [FC `docs/vsock.md:51-76`].
  - Guest→host: connect to CID 2 port P, which lands on the host Unix socket `<uds_path>_P` [vsock.md:78-90].
  - fireactions already uses it: VMs get `VsockDevices{Path, CID}` [FA `server/pool.go:461-483`], the agent serves gRPC on vsock port 9001 [FA `agent/agent.go:110-117`], and the server dials it [FA `server/machine.go:32-34`].
- **New option (inference): a read-only shared device for a warm store.**
  - Drives can be read-only [FC README.md:105-106].
  - `Pmem` has `path_on_host`, `root_device`, `read_only` and `rate_limiter` [FC firecracker.yaml:1303-1327]. It is mmapped, can use DAX, and needs no guest page cache [FC `docs/pmem.md`].
  - Attaching one image read-only to many VMs is safe. Sharing a **writable** image is not (the issue is right).
  - Whether firecracker-go-sdk (used by fireactions) exposes pmem is **Unresolved**. Check the SDK version in `go.mod`.

## 3. fireactions (this fork)

### 3.1 Basic-auth interceptor vs loopback-only: **Confirmed: loopback-only is sufficient, with one caveat**

- The interceptor is not implemented: `grpc.NewServer(/* TODO: Add auth interceptor … */)` [FA server.go:74-77], while `basic_auth_enabled`/`basic_auth_users` exist in config [FA `server/config.go:16-17`]. Reflection is on [FA server.go:104-105].
- **Caveat: the built-in default `bind_address` is `":8080"`** (all interfaces) [FA config.go:68]. A host listening on all interfaces is reachable from VMs over the bridge. The fleet renderer must always emit `127.0.0.1:<port>` (podbox does: `deploy/podbox/config.example.yaml`).
- Any workerd socket on the bridge IP is reachable by job code and **must** authenticate (the issue already plans VM→proxy tokens).
- If scale sets run in-process (§5.1), no remote client is needed and implementing auth is unnecessary. *(Inference.)*

### 3.2 Per-pool env into jobs, and whether the runner loads `.env`: **Confirmed**

- The runner loads `.env` from its root directory at Listener start: `LoadAndSetEnv()` reads `<bin>/../.env` as `KEY=VALUE` lines and calls `Environment.SetEnvironmentVariable` [RUN `src/Runner.Listener/Program.cs:18-19,172-198`]. GitHub docs say the same: "When the runner starts, it reads the variables set in `.env`" ([use proxy servers](https://docs.github.com/en/actions/how-tos/manage-runners/self-hosted-runners/use-proxy-servers)).
- Steps inherit the process env. `ProcessInvoker` only adds keys to `StartInfo.Environment` and never clears it [RUN `src/Runner.Sdk/ProcessInvoker.cs:272-289`]. *(The inherit-by-default part is .NET semantics. Verify with an `env` step on podbox.)*
- Runner root in the image is `/opt/runner` [FA `agent/runner/runner.go:21`], so the file is `/opt/runner/.env`.
- **Cheapest path (inference).** The agent already reads MMDS key `fireactions` [FA `cmd/fireactions/agent.go:31-47`], and the server copies all of `firecracker.metadata` into `latest/meta-data` [FA pool.go:527-535].
  - Add `metadata.env: {TURBO_API: …}` per pool, then either append those pairs to `runCmd.Env` [FA runner.go:224-232] (simplest, no file) or write `/opt/runner/.env`.
  - Note that MMDS is readable by any process in the VM, the same as the JIT config already stored there.

### 3.3 Does `ScalePool` interact cleanly with VMs mid-job? **Contradicted**

- `ScalePool` only calls `SetReplicas` [FA rpc.go:48-61]. The `Run` loop re-evaluates every 2 s or on trigger [FA pool.go:130-180].
- Scale-down calls `deleteMachine`, which **takes the first entry of a Go map (random order), deletes it and calls `StopVMM()`**. It does not check runner state [FA pool.go:306-338,612-641]. A busy runner is killed mid-job, and then the cleanup goroutine removes it from GitHub [FA pool.go:565-606,676-704].
- Runner state is available: the agent tracks `Idle`/`Running`/`Completed` from runner log lines [FA runner.go:252-272], and the server can query it over vsock [FA `server/convert.go:33-65`]. A fix is to restrict scale-down to `Idle` machines. *(Design, inference.)*
- **`PausePool` is a clean drain.** It only stops the reconcile loop [FA pool.go:167-170,341-358]. Ephemeral VMs finish their job, exit and are not replaced.
- Minor: `isActive` is an unsynchronized `bool` read and written across goroutines [FA pool.go:48,167,341-358].

## 4. Turborepo

### 4.1 Full remote cache API: **Confirmed**

Client: [TR `crates/turborepo-api-client/src/lib.rs`]. Spec: [TR `apps/docs/lib/remote-cache-openapi.json`], which lists paths without `/v8`. The client prepends `/v8` to `TURBO_API` via `make_url` = `base_url + endpoint` [lib.rs:310-313].

| Call | Request | Notes |
|---|---|---|
| exists | `HEAD /v8/artifacts/{hash}` | 200 or 404. Reads `x-artifact-duration`, `x-artifact-sha` and `x-artifact-dirty-hash` from the response [TR `crates/turborepo-cache/src/http.rs:411-436`; lib.rs:349-410] |
| fetch | `GET /v8/artifacts/{hash}` | 404 means a miss and 403 is an error. The body is a gzip tarball. `x-artifact-tag` is **required when signing is on** [http.rs:526-540] |
| upload | `PUT /v8/artifacts/{hash}` | Headers: `Content-Type: application/octet-stream`, `Content-Length`, `x-artifact-duration`, optional `x-artifact-tag`, `x-artifact-sha`, `x-artifact-dirty-hash`, plus `x-artifact-client-ci` when in CI [lib.rs:424-500,228-236]. The spec allows a 200 or 202 response |
| batch query | `POST /v8/artifacts`, body `{"hashes":[…]}` | The response maps hash → `{taskDurationMs, sha?, dirtyHash?}` or null [lib.rs:87-94,319-347]. It is **used only for dry-run**. On an error or missing key the client falls back to HEAD per hash [TR `crates/turborepo-cache/src/multiplexer.rs:307-340`] |
| status | `GET /v8/artifacts/status` | `{status: enabled|disabled|over_limit|paused}` [lib.rs:505-515] |
| events | `POST /v8/artifacts/events` | A JSON array of cache events (openapi `recordCacheEvents`) |

- **Auth:** `Authorization: Bearer <TURBO_TOKEN>` (openapi `bearerToken`, where self-hosters define the token format).
- **Team params:** `teamId` is appended **only if it starts with `team_`**, and `slug` is appended if set [lib.rs:1071-1103].
- Optional CORS-style preflight (`OPTIONS`) only with `TURBO_PREFLIGHT` [lib.rs:357-372].
- **Upstream Worker behaviour** (the Worker already deployed) [TC `src/routes/v8/artifacts.ts`]:
  - Objects are keyed by `${teamId ?? slug ?? DEFAULT}/${hash}`.
  - PUT stores **only** `x-artifact-tag` as R2 customMetadata. `x-artifact-duration` is dropped (lines 100-117).
  - GET/HEAD return the tag (lines 120-170).
  - `POST /v8/artifacts` returns `{}` (lines 57-75), so turbo falls back to HEAD.
  - One shared `TURBO_TOKEN` [TC `src/routes/auth.ts`], so there are no read-only tokens upstream.
  - The dashboard CI sets `TURBO_TEAM` (a slug), so the proxy must key and forward by `slug`.

### 4.2 How `x-artifact-tag` is computed and verified: **Confirmed**

- [TR `crates/turborepo-cache/src/signature_authentication.rs`] It is an HMAC-SHA256 keyed by `TURBO_REMOTE_CACHE_SIGNATURE_KEY`, computed over length-prefixed fields (u64 LE length + bytes): `"artifact-signature:v2"`, hash and team_id. Then come the body length (u64 LE) and the body bytes. The result is `BASE64_STANDARD` (lines 11-12, 65-72, 86-106, 188-191).
- A key length minimum of 32 bytes is enforced when `enforce_signature_key_length` is set.
- `team_id` comes from `api_auth.team_id` (empty if only the slug is set) [http.rs:63-80].
- **The client verifies only on GET.** It streams the body into an HMAC and rejects the artifact before extraction if it doesn't match [http.rs:540-595]. HEAD does not check tags.
- Consequence: the proxy must replay `x-artifact-tag` verbatim and must never alter the bytes (no re-compression).
- The dashboard already has `"remoteCache": {"signature": true}` (`apehost-connect-dashboard/turbo.json`).
- Signing does not stop a holder of both the key and a write token from poisoning the cache. Read-only tokens for PR pools are still needed.

### 4.3 Read-only remote mode: **Confirmed**

- `--cache <options>` (default `local:rw,remote:rw`, values `rw|r|w|` empty) [TR `apps/docs/content/docs/reference/run.mdx:88-118`]. The `TURBO_CACHE` env var is the same setting (system-environment-variables.mdx:49-55). The parser accepts `remote:r` and `local:rw,remote:r` (config.rs:212-220).
- The legacy `TURBO_REMOTE_CACHE_READ_ONLY` still exists (system-environment-variables.mdx:245-252).
- The dashboard's `deploy.yml` already uses `TURBO_CACHE: remote:rw`. Enforce read-only server-side as well (token scope in the proxy), since job code controls its own env.

## 5. GitHub

### 5.1 Runner scale-set APIs without ARC: **Confirmed: officially supported**

- **Client.** GitHub ships `github.com/actions/scaleset`, "a standalone Go client for the GitHub Actions Runner Scale Set APIs … You do *not* need to adopt the full controller (and Kubernetes)". It is in **Public Preview**: "the API is stable, interfaces … may change" [SS README.md]. Changelog: [2026-02-05](https://github.blog/changelog/2026-02-05-github-actions-early-february-2026-updates/), which mentions "Native multi-label support".
- **Scope and auth.**
  - The config URL can point at an org, an enterprise or a **repo** (`https://github.com/owner/repo`) [SS config.go:10,55-67]. That covers the personal-account, repo-level case.
  - Auth is a GitHub App (`ClientID`, `InstallationID`, `PrivateKey`) or a PAT [SS README].
  - Under the hood:
    1. Registration token: `POST /repos/{o}/{r}/actions/runners/registration-token` [SS client.go:915-923].
    2. `POST /actions/runner-registration` with `Authorization: RemoteAuth <regtoken>`, body `{"url","runner_event":"register"}`. This returns the Actions service URL and an admin token [client.go:832-850].
- **Actions-service endpoints** (`api-version=6.0-preview`) [SS client.go:24-25,40,265-266,604-640; session_client.go:51-75,122-142,262-290]:
  - `POST|PATCH|DELETE _apis/runtime/runnerscalesets[/{id}]`
  - `POST …/{id}/sessions` (body `{ownerName}`)
  - `GET <messageQueueUrl>?sessionId=&lastMessageId=`, with `Accept: application/json; api-version=6.0-preview` and **`X-ScaleSetMaxCapacity: N`**
  - `DELETE` message (ack)
  - `POST …/{id}/acquirejobs`
  - `POST …/{id}/generatejitconfig` (body `{name, workFolder}`)
- **Semantics** [SS README]:
  - The long poll blocks ~50 s, and a 202 means no messages.
  - Scale on `statistics.TotalAssignedJobs`, not on message counts (responses are capped at 50 messages).
  - `JobStarted` marks a runner busy, which is what ARC uses to avoid scale-down kills.
  - Jobs can be reassigned up to 3 times.
  - Runners are ephemeral and JIT, so the in-VM side (`run.sh --jitconfig`) is unchanged.
- **Versus webhooks.** Webhooks need a public endpoint and a 2xx response within 10 s ([best practices](https://docs.github.com/en/webhooks/using-webhooks/best-practices-for-using-webhooks)). GitHub "does not automatically redeliver failed webhook deliveries" ([failed deliveries](https://docs.github.com/en/webhooks/using-webhooks/handling-failed-webhook-deliveries)), so a scheduler on webhooks needs reconciliation anyway. **Scale sets are the better fit.**
- **Unresolved items (each needs a test on a throwaway repo):**
  1. Whether two message sessions can be open on one scale set at once. The client has no conflict handling, and the ARC docs describe a single listener per scale set ([deploy runner scale sets](https://docs.github.com/en/actions/how-tos/manage-runners/use-actions-runner-controller/deploy-runner-scale-sets): "Runner scale set names are unique within the runner group").
  2. If not, whether **per-host scale sets sharing a label** (e.g. `fireactions-4vcpu-8gb`) get jobs distributed. With per-host `maxCapacity` this would be the natural fleet design.
  3. Whether `self-hosted` is accepted as a custom scale-set label, given that today's workflows use `[self-hosted, fireactions, …]`.
  4. Personal-account repos have only the default runner group. Check `GetRunnerGroupByName("default")` on a repo URL.

### 5.2 `workflow_job` payload and rate limits: **Confirmed**

- `action` ∈ `queued | in_progress | completed | waiting`. `workflow_job` includes `id`, `run_id`, `run_attempt`, `labels[]`, `status`, `conclusion`, `runner_id`, `runner_name`, `runner_group_id`, `runner_group_name`, `head_branch`, `workflow_name`, `created_at` and `started_at`.
- Delivery types: repository, organization, business and app. A GitHub App needs at least read access to **Actions**. Signature header: `X-Hub-Signature-256` (HMAC-SHA256 hex). Payloads are capped at 25 MB ([webhook payloads](https://docs.github.com/en/webhooks/webhook-events-and-payloads#workflow_job)).
- REST limits: an App installation gets **5,000 req/h**, plus 50/h per repo above 20 repos and 50/h per user above 20 users, capped at 12,500 (15,000 on Enterprise Cloud). Secondary limits are 100 concurrent requests, 900 points/min, and 80 content-creating requests/min or 500/h ([rate limits](https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api)).
- Today each VM costs about 2 REST calls (`POST /repos/{o}/{r}/actions/runners/generate-jitconfig` and `DELETE /repos/{o}/{r}/actions/runners/{id}`), plus one cached installation lookup [FA pool.go:494-525,676-704]. That is far below the limits. *(Inference.)*
- Whether the scale-set Actions-service calls count against the REST limit is **Unresolved**. They go to the Actions service, not api.github.com.

### 5.3 Runner auto-update vs pinned images: **Confirmed, with one inference**

- By default, self-hosted runners auto-update. `--disableupdate` is a `config.sh` flag. "If you do not perform a software update within 30 days, the GitHub Actions service will not queue jobs to your runner", and that applies immediately for critical security updates. Ephemeral runners in containers "can lead to repeated software updates" ([self-hosted runners reference](https://docs.github.com/en/actions/reference/runners/self-hosted-runners)).
- `DisableUpdate` is applied only in `config.sh` registration [RUN `src/Runner.Listener/Configuration/ConfigurationManager.cs:257,312-315,341`]. The JIT REST endpoint takes no such field.
- *Inference:* JIT runners cannot opt out of auto-update. An outdated image self-updates on every VM boot, which costs time on each job, and stops getting jobs after 30 days.
- **Policy:** rebuild the runner image within 30 days of each `actions/runner` release. Current latest is v2.338.0; the fork's comment mentions 2.331.0 [FA runner.go:162].

## 6. Caches

### 6.1 Redirecting `actions/cache` (cache v2, `ACTIONS_RESULTS_URL`): **Confirmed not feasible without patching the runner**

- `@actions/cache` v2 reads only `ACTIONS_RESULTS_URL` (v1 reads `ACTIONS_CACHE_URL` and then `ACTIONS_RESULTS_URL`) and gates on `ACTIONS_CACHE_SERVICE_V2` [TK `packages/cache/src/internal/config.ts:14-59`].
- The runner sets `ACTIONS_RESULTS_URL`, `ACTIONS_CACHE_URL`, `ACTIONS_CACHE_SERVICE_V2` and `ACTIONS_CACHE_MODE` for Node actions **after** merging the env contexts, from the job's `SystemVssConnection` data [RUN `src/Runner.Worker/Handlers/NodeScriptActionHandler.cs:42-83`]. Workflow `env:` and `.env` cannot override them.
- It would need a patched runner or action. Skip, as the issue suggests.

### 6.2 npm/pnpm mirror and tarball URL rewriting: **Partly confirmed, pnpm client behaviour Unresolved**

- npm has `replace-registry-host` (default `npmjs`), which swaps the npmjs host in lockfile URLs for the configured registry ([npm config](https://docs.npmjs.com/cli/v11/using-npm/config#replace-registry-host)).
- pnpm's own registry proxy, **pnpr**, rewrites every `dist.tarball` in packuments to its `public_url` [PNPM `pnpr/crates/upstream/src/packument.rs:7-53`, `pnpr/docs/endpoints.md:69`]. That is evidence a mirror should rewrite.
- pnpr is also a ready-made, pnpm-maintained npm proxy (`127.0.0.1:7677`, proxies npmjs by default) [PNPM `pnpr/npm/pnpr/README.md`]. Check it before building `/npm/*` in workerd.
- Whether the pnpm *client* follows `dist.tarball` or rebuilds the URL from `registry` is Unresolved. Settle it with `pnpm install --reporter=ndjson` against a mirror that does not rewrite, and check which hosts get hit.

## 7. Ops and cost

### 7.1 R2 and Workers costs: **Confirmed prices; volume Unresolved**

- **R2 Standard** ([R2 pricing](https://developers.cloudflare.com/r2/pricing/)):
  - Storage: $0.015/GB-month.
  - Class A (Put, List, multipart): $4.50/M.
  - Class B (Get, Head): $0.36/M.
  - **Egress: free.** DeleteObject: free.
  - Free tier: 10 GB-month, 1M Class A and 10M Class B per month.
- **Workers** ([Workers pricing](https://developers.cloudflare.com/workers/platform/pricing/)):
  - Free plan: 100k requests/day.
  - Paid plan: $5/month with 10M requests included.
  - Durable Objects (SQLite-backed) are available on Free (100k requests/day).
  - Each alarm counts as one request. WebSocket messages count at 20:1.
- **Illustrative volume** *(inference, assumed numbers)*: 100 jobs/day × 30 turbo tasks = 3k lookups/day, or about 90k Class B per month **before** local hits. Uploads of about 1k/day are 30k Class A per month. Both are within the free tier. At 20 GB stored, the cost is (20−10) × $0.015 = $0.15/month.
- The upstream Worker's `deleteOldCache` cron lists 500 keys per page, which counts as Class A [TC `src/crons/deleteOldCache.ts`].
- **Unresolved:** real artifact count and size. Read them from R2 bucket metrics after a week.

### 7.2 Memory budget per host: **Unresolved (needs measurement on podbox)**

- Inputs from code:
  - Thin-pool tmpfs `size=36G`, data file 32G, meta 2G, `base_image_size = "20GB"` per snapshot [FA `deploy/podbox/setup.sh:60-76,93`]. tmpfs uses RAM only for written pages.
  - Guest RAM is 2 × 8192 MiB [FA config.example.yaml].
  - Firecracker overhead is ≤ 5 MiB per VMM [FC SPECIFICATION.md:24-34].
  - Guest memory is faulted in on demand and not returned unless the balloon is used (`free_page_reporting`/`free_page_hinting` exist [FC firecracker.yaml:906-922]).
- Worst case: Σ(vm mem) + tmpfs thin-pool fill + cache cap + workerd ≈ 16 + 36 + 8–16 GiB.
- **Measure on podbox:**
  1. `free -g`.
  2. `df -h` on the thin-pool tmpfs before and after 20 VM cycles. This shows whether freed thin blocks are discarded back to tmpfs through the loop device. If not, the tmpfs only grows until the pool is recreated.
  3. Peak VM RSS (`ps -o rss` of firecracker) during a dashboard build.

### 7.3 Fork PR policy for self-hosted runners: **Confirmed**

- "Self-hosted runners should almost never be used for public repositories on GitHub, because any user can open pull requests against the repository and compromise the environment" ([secure use](https://docs.github.com/en/actions/reference/security/secure-use)).
- `JungleM0nkey/apehost-connect-dashboard` is **private** (`gh api`: `private: true`). Fork PRs there come only from people with access, so the risk is limited to collaborators.
- `apehost-firerunners` itself is **public**, so do not register pool runners against it.
- Keep JIT/ephemeral VMs, which are already in use.

---

## Corrections to the issue

1. **"In-VM runner gets a fixed env (PATH, HOME, USER, UID, GID)"**: also `LOGNAME` [FA runner.go:224-232]. Trivial.
2. **"Self-hosted workerd may not fire cron triggers… a systemd timer may be needed"**: cron is indeed absent, but **Durable Object alarms work locally** (§1.2), so a timer is optional.
3. **"Alternative to webhooks: scale-set APIs, research needed"**: they are officially supported outside ARC through `actions/scaleset` (§5.1). With them, **the Cloudflare scheduler Durable Object, the webhook and the gRPC blocker are all unnecessary**: GitHub does the cross-host matching using each host's `maxCapacity`.
4. **The issue implicitly assumes `ScalePool` scale-down is safe**: it isn't. It kills busy VMs (§3.3). Demand-driven scaling must fix that first.
5. **"Proxy must answer existence checks locally"**: correct, and specifically via `HEAD /v8/artifacts/{hash}`. The batch `POST /v8/artifacts` is dry-run only, and the current upstream Worker answers it with `{}` (§4.1).
6. **"Store and replay response headers"**: the upstream Worker **does not store `x-artifact-duration`** (only the tag), so upstream hits report a time saved of 0. The proxy should keep its own metadata and could add duration on write-through.
7. **"Two hosts uploading the same hash is harmless"**: true for content. Note, though, that cache keys are namespaced by `teamId`/`slug` in the upstream Worker. The proxy must forward query params and key by the same namespace.
8. **Unstated default:** fireactions' built-in `bind_address` default is `:8080` on all interfaces [FA config.go:68]. That would expose the unauthenticated gRPC API to VMs on the bridge. The renderer must always set loopback.

## Recommended next steps

1. **Fix scale-down** in the fork (~2 h). Pick only machines whose agent reports `Idle`, or use the scale-set `JobStarted` signal. Add a test for `deleteMachine` selection.
2. **Scale-set spike on podbox** (~1 day). Embed `github.com/actions/scaleset` (`listener` package) per pool in fireactions, keeping JIT VMs. Test the four Unresolved items in §5.1, especially per-host scale sets sharing a label.
3. **Cache proxy on podbox, Turbo only** (~1–2 days).
   - workerd `disk` service with `writable = true` on the tmpfs dir.
   - Sidecar metadata for the tag, duration, sha and dirty-hash.
   - Local `HEAD`, read-through `GET`, local `PUT` plus background upload via `waitUntil` with a pending marker.
   - `POST /v8/artifacts` passthrough or `{}`.
   - Key by `slug`. Token-scoped read-only mode.
   - An LRU index in a local Durable Object with `enableSql` and `localDisk`.
   - Listen on the bridge IP and use an `external` service or a network with `allow = ["public","local","private"]` for upstream.
4. **Per-pool env** (~1 h): `firecracker.metadata.env` → agent appends to `runCmd.Env` (or `/opt/runner/.env`). Set `TURBO_API`, `TURBO_TOKEN` and `TURBO_CACHE` per pool.
5. **Measure the memory budget** (§7.2) before sizing the cache cap. Then render `fleet.yaml` for podbox alone.
6. **Image cadence:** rebuild the runner image within 30 days of each runner release (§5.3).
7. Only if step 2 fails: fall back to the webhook plus hosted Durable Object scheduler, with option **(a)** HTTP endpoints on the metrics mux and local Durable Object alarms for polling.
