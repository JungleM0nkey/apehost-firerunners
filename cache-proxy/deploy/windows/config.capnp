using Workerd = import "/workerd/workerd.capnp";

const config :Workerd.Config = (
  services = [
    (name = "cache", worker = .cacheWorker),
    (name = "artifacts", disk = (path = "artifacts", writable = true)),
    (name = "index", disk = (path = "index", writable = true)),
    (name = "offline", network = (allow = [])),
  ],
  sockets = [
    # Windows Firewall limits this socket to the Cloudflare Mesh source range.
    (name = "api", address = "0.0.0.0:8787", http = (), service = "cache"),
    (name = "metrics", address = "127.0.0.1:9787", http = (),
      service = (name = "cache", entrypoint = "Metrics")),
  ],
);

const cacheWorker :Workerd.Worker = (
  modules = [(name = "worker.js", esModule = embed "worker.js")],
  compatibilityDate = "2026-10-01",
  bindings = [
    (name = "DISK", service = "artifacts"),
    (name = "UPSTREAM", service = "offline"),
    (name = "INDEX", durableObjectNamespace = "CacheIndex"),
    (name = "SETTINGS", json = "{\"upstream\":null,\"maxBytes\":2147483648}"),
    (name = "TOKENS_RW", fromEnvironment = "TOKENS_RW"),
    (name = "TOKENS_RO", fromEnvironment = "TOKENS_RO"),
  ],
  durableObjectNamespaces = [
    (className = "CacheIndex", uniqueKey = "apehost-desktop-turbo-cache", enableSql = true),
  ],
  durableObjectStorage = (localDisk = "index"),
  globalOutbound = "offline",
);
