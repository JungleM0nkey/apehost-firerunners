# Windows Mesh cache

This installs the existing cache Worker with native Windows `workerd`. It stores artifacts
on this PC and serves authenticated Turbo clients over Cloudflare Mesh. No upstream
credential or R2 access is needed. The `turbo.apehost.net` Worker is unchanged.

From `cache-proxy`:

```powershell
npm ci
npm run build
powershell -NoProfile -ExecutionPolicy Bypass -File deploy/windows/install.ps1
```

The default endpoint is `http://100.96.0.4:8787` (BIGBOX-WIN). Pass `-MeshAddress` on the
first install if this PC has a different Cloudflare Mesh IPv4 address.

The installer uses `%LOCALAPPDATA%\apehost\turbo-cache`, restricted to the current user
and SYSTEM. It generates separate read-write/read-only bearer tokens and a shared artifact
signing key. Reinstalling preserves those credentials and stored artifacts. It also creates
`Apehost Turbo Cache.lnk` in the current user's Startup folder and starts the server now.
The PC must be awake and the user signed in to serve builds.

## Mesh access

The host needs Cloudflare One **Traffic and DNS** mode with `100.96.0.0/12` routed through
the mesh. Proxy-only mode cannot receive connections to its assigned Mesh address. Use a
device-specific profile with only the Mesh range included to keep other IP traffic direct.
See [Cloudflare's client-to-Mesh instructions](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-mesh/guides/connect-client-devices/).

After enabling that profile, run the installed `firewall.ps1` once as Administrator. It
allows TCP 8787 only from `100.96.0.0/12`, for this installation's `workerd.exe`. Metrics
remain bound to loopback at `http://127.0.0.1:9787/metrics`; health is `/healthz`.

From another Mesh device, an unauthenticated request to
`http://100.96.0.4:8787/v8/artifacts/status` must reach the server and return `401`.
With a client token it returns `{"status":"enabled"}`. A local build alone does not
verify Mesh reachability. Client profiles using Include mode must also route the cache
address through Cloudflare; `100.96.0.4/32` is sufficient if only this host is needed.

## Clients

This installation serves apehost-shell builds. Existing private settings are at
`%LOCALAPPDATA%\apehost\turbo-cache\client.env` on Windows and
`/home/m0nkey/.config/apehost/turbo-cache/client.env` on podbox. They include the
endpoint, bearer token, signing key and `TURBO_TEAM=apehost-shell`. The adjacent
`client-readonly.env` permits restores only. Keep these files out of Git, logs and
source archives; the configured clients read them in place.

The merged apehost-shell build wrapper automatically reads the private settings
for native builds. Use the current published shell source, Node 22.9 or newer and
the pinned npm 11.6.2/Turbo 2.11.9:

```powershell
npm ci --ignore-scripts
npm run build
```

The shell's `turbo.json` already enables artifact signature verification. Podbox
Docker builds load their existing private file with `--env-file` and use host
networking to reach Mesh. See the shell repository's `docs/build-cache.md` for the
complete command. No global Turbo environment or other repositories are configured.

## Storage and operation

The 2 GiB limit in `config.capnp` covers artifact bytes; the SQLite index and logs use
additional space. Eviction runs on writes, with no polling or scheduled maintenance.
Standalone uploads must declare `Content-Length`; artifacts larger than the entire cache
are rejected. A cache miss is rebuilt normally.

`artifacts/` and `index/` hold persistent data. Keep them together. `server.env` holds
server credentials; the two client files also contain secrets. `stdout.log`, `stderr.log`
and `worker.pid` are local runtime files. Run the installer again after rebuilding to
update and restart this installation. Windows sign-in starts it through the shortcut;
there is no Windows service or crash watchdog.
