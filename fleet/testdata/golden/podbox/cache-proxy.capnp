# Rendered by `fireactions fleet render` from fleet.yaml. Do not edit; edit fleet.yaml and re-render.
# workerd config for the per-host Turborepo cache proxy. Installed as
# /opt/fireactions/cache-proxy/config.capnp next to worker.js (cache-proxy `npm run build`).
# Secrets come from the environment (cache-proxy.env), never from this file.

using Workerd = import "/workerd/workerd.capnp";

const config :Workerd.Config = (
  services = [
    (name = "cache-proxy", worker = .proxyWorker),
    (name = "artifacts", disk = (path = "/var/lib/fireactions/turbocache/artifacts", writable = true)),
    (name = "do-storage", disk = (path = "/var/lib/fireactions/turbocache/do", writable = true)),
    (name = "upstream", network = (allow = ["public"], tlsOptions = (trustBrowserCas = true))),
  ],

  sockets = [
    # VM bridge only: VMs use TURBO_API=http://10.200.0.1:8787.
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
    (name = "SETTINGS", json = "{\"upstream\":\"https://turbo.apehost.net\",\"maxBytes\":8589934592,\"maintenanceIntervalSeconds\":60}"),
    (name = "UPSTREAM_TOKEN", fromEnvironment = "UPSTREAM_TOKEN"),
    (name = "TOKENS_RW", fromEnvironment = "TOKENS_RW"),
    (name = "TOKENS_RO", fromEnvironment = "TOKENS_RO"),
  ],

  durableObjectNamespaces = [
    (className = "CacheIndex", uniqueKey = "fireactions-cache-proxy-index", enableSql = true),
  ],
  durableObjectStorage = (localDisk = "do-storage"),

  globalOutbound = "upstream",
);
