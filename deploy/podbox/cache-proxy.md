# Turborepo cache proxy on podbox

Every CI host runs `fireactions-cache-proxy`, a [workerd](https://github.com/cloudflare/workerd)
process that serves the Turborepo remote-cache API to the Firecracker VMs on the bridge. Reads
are served from a local RAM cache and fall through to `https://turbo.apehost.net`. Writes are
stored locally and uploaded upstream in the background. The design and the HTTP and binding
contract are in [`cache-proxy/README.md`](../../cache-proxy/README.md).

```
VM (10.200.0.x) --TURBO_API--> 10.200.0.1:8787  workerd (default entrypoint) --> https://turbo.apehost.net
                                127.0.0.1:9787   workerd (Metrics entrypoint, loopback only)
                                /var/lib/fireactions/turbocache   tmpfs: artifacts/ + do/ (index)
```

## Files

| Path on podbox | Source in this repo |
|---|---|
| `/opt/fireactions/bin/workerd` | The `workerd` npm package pinned in `cache-proxy/package-lock.json` |
| `/opt/fireactions/cache-proxy/config.capnp` | `cache-proxy/deploy/config.capnp` |
| `/opt/fireactions/cache-proxy/worker.js` | `cache-proxy/dist/worker.js` (from `npm run build`) |
| `/etc/fireactions/cache-proxy.env` | Written by hand, `0600 root:root`. Holds the secrets |
| `/etc/systemd/system/var-lib-fireactions-turbocache.mount` | `deploy/podbox/var-lib-fireactions-turbocache.mount` |
| `/etc/systemd/system/fireactions-cache-proxy.service` | `deploy/podbox/fireactions-cache-proxy.service` |

## Install

The build can run on any machine with Node 22 and npm. Copy the results to podbox if you build
elsewhere.

```sh
cd cache-proxy
npm ci && npm test && npm run build && npm run smoke
sudo install -D -m755 node_modules/@cloudflare/workerd-linux-64/bin/workerd /opt/fireactions/bin/workerd
sudo install -D -m644 deploy/config.capnp /opt/fireactions/cache-proxy/config.capnp
sudo install -D -m644 dist/worker.js      /opt/fireactions/cache-proxy/worker.js
sudo install -m644 ../deploy/podbox/var-lib-fireactions-turbocache.mount ../deploy/podbox/fireactions-cache-proxy.service /etc/systemd/system/
```

Next, create the env file. It holds secrets only. The non-secret settings, such as the upstream
URL and `maxBytes`, are in the `SETTINGS` json binding in `config.capnp`.

```sh
sudo install -m600 /dev/null /etc/fireactions/cache-proxy.env
sudoedit /etc/fireactions/cache-proxy.env
```

```sh
# Token the proxy presents to https://turbo.apehost.net (the Worker's TURBO_TOKEN secret).
UPSTREAM_TOKEN=...
# Client tokens for VMs, comma-separated. Generate each with: openssl rand -hex 32
TOKENS_RW=...   # trusted pools (pushes to main, deploys): may upload
TOKENS_RO=...   # PR pools: read-only, PUT returns 403
```

Finally, start the mount and the service:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now var-lib-fireactions-turbocache.mount fireactions-cache-proxy.service
curl -s 127.0.0.1:9787/healthz
curl -s -H "Authorization: Bearer <a TOKENS_RO token>" http://10.200.0.1:8787/v8/artifacts/status
curl -s 127.0.0.1:9787/metrics | grep -v '^#'
```

To upgrade, rebuild and reinstall `worker.js`, `config.capnp` and `workerd`, then run
`systemctl restart fireactions-cache-proxy`. The tmpfs stays mounted across the restart, so the
cache stays warm and pending uploads are retried.

## Storage and lifecycle

- **tmpfs.** `var-lib-fireactions-turbocache.mount` is a 5 GiB tmpfs at
  `/var/lib/fireactions/turbocache`. `ExecStartPre` creates `artifacts/` (artifact bytes,
  `<namespace>/<hash>`) and `do/` (the Durable Object SQLite index, pending markers, counters and
  alarm). Both are on the same tmpfs, so a reboot always gives a consistent **cold** cache:
  no index survives without its bytes, and no bytes survive without their index.
- **Size.** The LRU keeps artifact bytes under `SETTINGS.maxBytes` (4 GiB). The tmpfs `size=` is
  set a bit higher for SQLite, writes in flight and pending uploads, which are never evicted. If
  you change one, change the other, and check the host RAM budget against the thin-pool tmpfs
  in `setup.sh`. tmpfs only uses RAM for pages that are actually written.
- **Restarts and reboots.** A service restart keeps the cache and retries pending uploads, both
  on the first request and on `/healthz`. A reboot, or `systemctl stop` of the mount, drops the
  cache. Uploads still pending at that moment are lost: those artifacts never reached upstream
  and are rebuilt on the next miss.
- **Startup order.** The service waits up to 120 s for `10.200.0.1` to appear on
  `fireactions-br0`. CNI creates the bridge when fireactions starts the first VM. If the address
  does not appear, the service fails, and `Restart=always` tries again 5 s later.

## Network exposure

- **API socket.** The API socket binds `10.200.0.1:8787` only, the bridge gateway, so it is not
  on the LAN-facing interfaces. Anything that can route to `10.200.0.0/24` through this host
  could still reach it, which is why every request needs a token.
- **Metrics socket.** `/metrics` and `/healthz` exist only on `127.0.0.1:9787`, which is served
  by a separate workerd entrypoint. The bridge socket answers `404` for them, even with a valid
  token.
- **Upstream.** The proxy reaches upstream through a workerd `network` service that allows public
  addresses only and verifies TLS against the browser CA set.
- **Host firewall.** If the host firewall drops `INPUT` by default, allow the bridge:
  `iptables -I INPUT -i fireactions-br0 -p tcp --dport 8787 -j ACCEPT`.

## Pointing jobs at the proxy

The turbo client in a VM needs these variables:

| Variable | Trusted pools (main, deploys) | PR pools |
|---|---|---|
| `TURBO_API` | `http://10.200.0.1:8787` | `http://10.200.0.1:8787` |
| `TURBO_TOKEN` | a `TOKENS_RW` token | a `TOKENS_RO` token |
| `TURBO_TEAM` | the team slug used today (keys objects upstream) | same |
| `TURBO_CACHE` | `remote:rw` | `remote:r` |

`TURBO_REMOTE_CACHE_SIGNATURE_KEY` and `remoteCache.signature` stay exactly as they are. The
proxy replays `x-artifact-tag` byte-for-byte, so signatures still verify end to end. Leave
`TURBO_PREFLIGHT` unset.

Set them per pool with `env:` (#8), so workflows need no changes and never see a Cloudflare
credential. A pool has one token, so PR jobs and trusted jobs need separate pools (labels):

```yaml
pools:
- name: fireactions-4vcpu-8gb            # PR CI: read-only
  env:
    TURBO_API: http://10.200.0.1:8787
    TURBO_TEAM: <team slug>
    TURBO_TOKEN: <a TOKENS_RO token>
    TURBO_CACHE: remote:r
- name: fireactions-4vcpu-8gb-trusted    # deploy job (runs-on its own label): read-write
  env:
    TURBO_API: http://10.200.0.1:8787
    TURBO_TEAM: <team slug>
    TURBO_TOKEN: <a TOKENS_RW token>
    TURBO_CACHE: remote:rw
```

Remove the `TURBO_*` env/secrets from the deploy workflow once the trusted pool serves it.
Caveat: anyone who can push a branch can point a PR job at the trusted label. In a private repo
with trusted collaborators that's accepted; the environment-protected deploy job is the only
intended user of that label. (Until the pools are split, the workflow-level `env:` with
`github.event_name == 'pull_request'` switching between an RO and an RW secret works too.)

`TURBO_CACHE=remote:r` alone does not protect anything, because job code controls its own
environment. The `TOKENS_RO` scope is what stops PR jobs from writing: the proxy returns `403`
on `PUT`.

## Observability

Scrape `127.0.0.1:9787/metrics`. The series are listed in `cache-proxy/README.md`. Watch these:

- `turbo_proxy_pending_uploads` should return to 0.
- `turbo_proxy_upload_failures_total` and `turbo_proxy_upstream_errors_total` count upstream
  trouble. Reads degrade to misses, so builds still pass.
- The hit ratio is `turbo_proxy_local_hits_total` against `turbo_proxy_upstream_hits_total` and
  `turbo_proxy_misses_total`.

workerd logs (upload failures, store errors) go to the journal:
`journalctl -u fireactions-cache-proxy`.
