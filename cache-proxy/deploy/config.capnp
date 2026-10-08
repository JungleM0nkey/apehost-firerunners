# workerd config for the per-host Turborepo remote-cache proxy (podbox).
#
# Install layout (see deploy/podbox/cache-proxy.md):
#   /opt/fireactions/cache-proxy/config.capnp   this file
#   /opt/fireactions/cache-proxy/worker.js      `npm run build` output (dist/worker.js), embedded below
#   /var/lib/fireactions/turbocache/            tmpfs, wiped on reboot => consistent cold cache
#     artifacts/                                artifact bytes, <namespace>/<hash>
#     do/                                       Durable Object SQLite (LRU index, pending uploads, counters)
#
# Secrets come from the process environment (systemd EnvironmentFile), never from this file:
#   UPSTREAM_TOKEN, TOKENS_RW, TOKENS_RO
#
# Run: workerd serve /opt/fireactions/cache-proxy/config.capnp

using Workerd = import "/workerd/workerd.capnp";

const config :Workerd.Config = (
  services = [
    (name = "cache-proxy", worker = .proxyWorker),
    (name = "artifacts", disk = (path = "/var/lib/fireactions/turbocache/artifacts", writable = true)),
    (name = "do-storage", disk = (path = "/var/lib/fireactions/turbocache/do", writable = true)),
    # Upstream access: public addresses only, verified against the browser CA set.
    (name = "upstream", network = (allow = ["public"], tlsOptions = (trustBrowserCas = true))),
  ],

  sockets = [
    # Firecracker bridge only (fireactions-br0 gateway): VMs use TURBO_API=http://10.200.0.1:8787.
    (name = "api", address = "10.200.0.1:8787", http = (), service = "cache-proxy"),
    # Loopback only, separate entrypoint: the bridge socket cannot reach /metrics.
    (name = "metrics", address = "127.0.0.1:9787", http = (),
     service = (name = "cache-proxy", entrypoint = "Metrics")),
  ],
);

const proxyWorker :Workerd.Worker = (
  modules = [(name = "worker.js", esModule = embed "worker.js")],
  compatibilityDate = "2026-10-01",

  bindings = [
    (name = "DISK", service = "artifacts"),
    (name = "UPSTREAM", service = "upstream"),
    (name = "INDEX", durableObjectNamespace = "CacheIndex"),
    (name = "SETTINGS", json = "{\"upstream\":\"https://turbo.apehost.net\",\"maxBytes\":4294967296,\"maintenanceIntervalSeconds\":60}"),
    (name = "UPSTREAM_TOKEN", fromEnvironment = "UPSTREAM_TOKEN"),
    (name = "TOKENS_RW", fromEnvironment = "TOKENS_RW"),
    (name = "TOKENS_RO", fromEnvironment = "TOKENS_RO"),
  ],

  durableObjectNamespaces = [
    (className = "CacheIndex", uniqueKey = "fireactions-cache-proxy-index", enableSql = true),
  ],
  durableObjectStorage = (localDisk = "do-storage"),

  # The Worker only talks to the bindings above.
  globalOutbound = "upstream",
);
