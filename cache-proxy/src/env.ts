import type { CacheIndex } from "./cache-index";

/** Bindings the Worker expects. See README.md for the workerd side of this contract. */
export interface Env {
  /** workerd `disk` service (DiskDirectory, writable) holding artifact bytes as `<ns>/<hash>`. */
  DISK: Fetcher;
  /** Outbound service used for every upstream request (a workerd `network` service in production). */
  UPSTREAM: Fetcher;
  /** SQLite-backed Durable Object namespace holding the LRU index, pending markers and counters. */
  INDEX: DurableObjectNamespace<CacheIndex>;
  /** Non-secret settings (`json` binding). */
  SETTINGS: unknown;
  /** Bearer token the proxy presents upstream (`fromEnvironment`). */
  UPSTREAM_TOKEN?: string | null;
  /** Comma-separated read-write client tokens (`fromEnvironment`). */
  TOKENS_RW?: string | null;
  /** Comma-separated read-only client tokens (`fromEnvironment`). */
  TOKENS_RO?: string | null;
}

export interface Settings {
  /** Upstream base URL, e.g. https://turbo.apehost.net (no trailing slash). */
  upstream: string;
  /** LRU cap for artifact bytes kept on disk. */
  maxBytes: number;
  /** Period of the maintenance alarm (upload retries + eviction). */
  maintenanceIntervalSeconds: number;
}

export function readSettings(env: Env): Settings {
  const raw = (env.SETTINGS ?? {}) as Record<string, unknown>;
  const upstream = raw.upstream;
  if (typeof upstream !== "string" || !/^https?:\/\/[^/]+/.test(upstream)) {
    throw new Error("SETTINGS.upstream must be an http(s) URL");
  }
  const maxBytes = raw.maxBytes ?? 4 * 1024 ** 3;
  if (typeof maxBytes !== "number" || !Number.isFinite(maxBytes) || maxBytes <= 0) {
    throw new Error("SETTINGS.maxBytes must be a positive number");
  }
  const interval = raw.maintenanceIntervalSeconds ?? 60;
  if (typeof interval !== "number" || !Number.isFinite(interval) || interval < 1) {
    throw new Error("SETTINGS.maintenanceIntervalSeconds must be >= 1");
  }
  return { upstream: upstream.replace(/\/+$/, ""), maxBytes, maintenanceIntervalSeconds: interval };
}

/** Upstream default namespace: `${teamId ?? slug ?? DEFAULT_TEAM_ID}/${hash}`. */
export const DEFAULT_TEAM_ID = "team_default_team";

/** Strict charset for namespaces and hashes: they become path segments in the disk service. */
const SAFE_SEGMENT = /^[A-Za-z0-9_-]{1,128}$/;
export const isSafeSegment = (s: string): boolean => SAFE_SEGMENT.test(s);

/** Headers stored with an artifact and replayed verbatim on local hits. */
export const REPLAY_HEADERS = [
  "x-artifact-duration",
  "x-artifact-tag",
  "x-artifact-sha",
  "x-artifact-dirty-hash",
] as const;

/** Headers forwarded on the write-behind upload (replayed ones plus client hints). */
export const UPLOAD_HEADERS = [
  ...REPLAY_HEADERS,
  "x-artifact-client-ci",
  "x-artifact-client-interactive",
] as const;

export type StoredHeaders = Record<string, string>;

export function pickHeaders(from: Headers, names: readonly string[]): StoredHeaders {
  const out: StoredHeaders = {};
  for (const name of names) {
    const v = from.get(name);
    if (v !== null) out[name] = v;
  }
  return out;
}

export const diskUrl = (ns: string, hash: string): string => `http://disk/${ns}/${hash}`;

export function upstreamUrl(settings: Settings, hash: string | null, query: string): string {
  return `${settings.upstream}/v8/artifacts${hash === null ? "" : `/${hash}`}${query}`;
}

export function upstreamHeaders(env: Env, extra: Record<string, string> = {}): Headers {
  const h = new Headers(extra);
  if (env.UPSTREAM_TOKEN) h.set("authorization", `Bearer ${env.UPSTREAM_TOKEN}`);
  return h;
}
