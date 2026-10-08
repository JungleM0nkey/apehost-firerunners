import { DurableObject } from "cloudflare:workers";
import {
  diskUrl,
  type Env,
  readSettings,
  type StoredHeaders,
  upstreamHeaders,
  upstreamUrl,
} from "./env";

export interface Entry {
  size: number;
  headers: StoredHeaders;
}

export interface BatchInfo {
  taskDurationMs: number;
  sha?: string;
  dirtyHash?: string;
}

const COUNTERS: Record<string, string> = {
  turbo_proxy_local_hits_total: "Requests answered from the local disk cache.",
  turbo_proxy_upstream_hits_total: "Local misses answered by the upstream cache.",
  turbo_proxy_misses_total: "Requests that missed both locally and upstream.",
  turbo_proxy_upstream_errors_total: "Upstream lookups that failed (network error or unexpected status); answered as a miss.",
  turbo_proxy_stored_bytes_total: "Artifact bytes written to the local disk cache.",
  turbo_proxy_uploads_total: "Write-behind uploads accepted by the upstream cache.",
  turbo_proxy_upload_failures_total: "Write-behind upload attempts that failed (retried later).",
  turbo_proxy_evictions_total: "Artifacts evicted by the LRU.",
  turbo_proxy_evicted_bytes_total: "Artifact bytes evicted by the LRU.",
  turbo_proxy_auth_failures_total: "Requests rejected for a missing or unknown token.",
};

/**
 * Single instance ("index") per proxy. Holds the LRU index, the write-behind pending markers and
 * the metric counters in SQLite. Artifact bytes live in the DISK service, never in here.
 */
export class CacheIndex extends DurableObject<Env> {
  private readonly sql: SqlStorage;
  private clock = 0;
  private readonly uploading = new Map<string, Promise<boolean>>();

  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    this.sql = ctx.storage.sql;
    ctx.blockConcurrencyWhile(async () => {
      this.sql.exec(`CREATE TABLE IF NOT EXISTS artifacts (
        ns TEXT NOT NULL,
        hash TEXT NOT NULL,
        size INTEGER NOT NULL,
        last_access INTEGER NOT NULL,
        pending INTEGER NOT NULL DEFAULT 0,
        headers TEXT NOT NULL DEFAULT '{}',
        query TEXT NOT NULL DEFAULT '',
        PRIMARY KEY (ns, hash)
      )`);
      this.sql.exec("CREATE INDEX IF NOT EXISTS artifacts_lru ON artifacts (pending, last_access)");
      this.sql.exec("CREATE TABLE IF NOT EXISTS counters (series TEXT PRIMARY KEY, value INTEGER NOT NULL)");
      this.clock = this.one<{ t: number | null }>("SELECT max(last_access) AS t FROM artifacts")?.t ?? 0;
      // Fresh instance (process start or eviction): retry leftover pending uploads soon and make
      // sure the self-rescheduling maintenance alarm exists.
      const pending = this.pendingCount() > 0;
      const current = await ctx.storage.getAlarm();
      const soon = Date.now() + 1000;
      if (current === null || (pending && current > soon)) {
        await ctx.storage.setAlarm(pending ? soon : Date.now() + this.intervalMs());
      }
    });
  }

  // ---- index -----------------------------------------------------------------------------

  /** Look up an artifact and mark it most recently used. */
  lookup(ns: string, hash: string): Entry | null {
    const row = this.one<{ size: number; headers: string }>(
      "SELECT size, headers FROM artifacts WHERE ns = ? AND hash = ?",
      ns,
      hash,
    );
    if (!row) return null;
    this.sql.exec("UPDATE artifacts SET last_access = ? WHERE ns = ? AND hash = ?", this.tick(), ns, hash);
    return { size: row.size, headers: JSON.parse(row.headers) as StoredHeaders };
  }

  /** Drop an entry whose bytes are gone from disk (e.g. tmpfs wiped). */
  forget(ns: string, hash: string): void {
    this.sql.exec("DELETE FROM artifacts WHERE ns = ? AND hash = ?", ns, hash);
  }

  /** Record bytes that were just written to disk, then evict down to the cap. */
  async record(
    ns: string,
    hash: string,
    size: number,
    headers: StoredHeaders,
    query: string,
    pending: boolean,
  ): Promise<void> {
    this.sql.exec(
      `INSERT INTO artifacts (ns, hash, size, last_access, pending, headers, query)
       VALUES (?, ?, ?, ?, ?, ?, ?)
       ON CONFLICT (ns, hash) DO UPDATE SET
         size = excluded.size, last_access = excluded.last_access, headers = excluded.headers,
         query = excluded.query, pending = max(artifacts.pending, excluded.pending)`,
      ns,
      hash,
      size,
      this.tick(),
      pending ? 1 : 0,
      JSON.stringify(headers),
      query,
    );
    this.bump("turbo_proxy_stored_bytes_total", size);
    await this.evict();
  }

  /** POST /v8/artifacts: answer only for local entries; the client HEADs the rest. */
  batch(ns: string, hashes: string[]): Record<string, BatchInfo> {
    const out: Record<string, BatchInfo> = {};
    for (const hash of hashes) {
      const row = this.one<{ headers: string }>(
        "SELECT headers FROM artifacts WHERE ns = ? AND hash = ?",
        ns,
        hash,
      );
      if (!row) continue;
      const h = JSON.parse(row.headers) as StoredHeaders;
      const info: BatchInfo = { taskDurationMs: Number(h["x-artifact-duration"] ?? 0) || 0 };
      if (h["x-artifact-sha"]) info.sha = h["x-artifact-sha"];
      if (h["x-artifact-dirty-hash"]) info.dirtyHash = h["x-artifact-dirty-hash"];
      out[hash] = info;
    }
    return out;
  }

  // ---- write-behind ----------------------------------------------------------------------

  /** Upload one pending artifact from disk to upstream. Concurrent calls share one attempt. */
  upload(ns: string, hash: string): Promise<boolean> {
    const key = `${ns}/${hash}`;
    let p = this.uploading.get(key);
    if (!p) {
      p = this.doUpload(ns, hash).finally(() => this.uploading.delete(key));
      this.uploading.set(key, p);
    }
    return p;
  }

  private async doUpload(ns: string, hash: string): Promise<boolean> {
    const row = this.one<{ headers: string; query: string }>(
      "SELECT headers, query FROM artifacts WHERE ns = ? AND hash = ? AND pending = 1",
      ns,
      hash,
    );
    if (!row) return true;
    try {
      const file = await this.env.DISK.fetch(diskUrl(ns, hash));
      if (file.status === 404) {
        this.forget(ns, hash); // bytes are gone; nothing left to upload
        return false;
      }
      if (!file.ok || !file.body) throw new Error(`disk read ${file.status}`);
      const length = Number(file.headers.get("content-length"));
      if (!Number.isSafeInteger(length)) throw new Error("disk response without Content-Length");
      const headers = upstreamHeaders(this.env, {
        ...(JSON.parse(row.headers) as StoredHeaders),
        "content-type": "application/octet-stream",
        "content-length": String(length),
      });
      // Known-length body so upstream (R2 put) sees a Content-Length.
      const body = new FixedLengthStream(length);
      file.body.pipeTo(body.writable).catch(() => {});
      const res = await this.env.UPSTREAM.fetch(upstreamUrl(readSettings(this.env), hash, row.query), {
        method: "PUT",
        headers,
        body: body.readable,
      });
      await res.body?.cancel();
      if (!res.ok) throw new Error(`upstream PUT ${res.status}`);
      this.sql.exec("UPDATE artifacts SET pending = 0 WHERE ns = ? AND hash = ?", ns, hash);
      this.bump("turbo_proxy_uploads_total");
      await this.evict(); // the artifact just became evictable
      return true;
    } catch (err) {
      this.bump("turbo_proxy_upload_failures_total");
      console.warn(`upload ${ns}/${hash} failed: ${String(err)}`);
      return false;
    }
  }

  // ---- maintenance -----------------------------------------------------------------------

  /** Called on the first request after start; the constructor does the real work. */
  kick(): void {}

  override async alarm(): Promise<void> {
    try {
      const pending = this.sql
        .exec<{ ns: string; hash: string }>("SELECT ns, hash FROM artifacts WHERE pending = 1 ORDER BY last_access")
        .toArray();
      for (const { ns, hash } of pending) await this.upload(ns, hash);
      await this.evict();
    } finally {
      await this.ctx.storage.setAlarm(Date.now() + this.intervalMs());
    }
  }

  /** Evict least-recently-used, already-uploaded artifacts until under the cap. */
  private async evict(): Promise<void> {
    const max = readSettings(this.env).maxBytes;
    for (;;) {
      const total = this.one<{ total: number | null }>("SELECT sum(size) AS total FROM artifacts")?.total ?? 0;
      if (total <= max) return;
      const victim = this.one<{ ns: string; hash: string; size: number }>(
        "SELECT ns, hash, size FROM artifacts WHERE pending = 0 ORDER BY last_access LIMIT 1",
      );
      if (!victim) return; // only pending uploads left: keep them, they are not upstream yet
      this.forget(victim.ns, victim.hash);
      this.bump("turbo_proxy_evictions_total");
      this.bump("turbo_proxy_evicted_bytes_total", victim.size);
      const res = await this.env.DISK.fetch(diskUrl(victim.ns, victim.hash), { method: "DELETE" });
      await res.body?.cancel();
    }
  }

  // ---- metrics ---------------------------------------------------------------------------

  count(series: string, n = 1): void {
    this.bump(series, n);
  }

  metrics(): string {
    const values = new Map(
      this.sql
        .exec<{ series: string; value: number }>("SELECT series, value FROM counters")
        .toArray()
        .map((r) => [r.series, r.value]),
    );
    const lines: string[] = [];
    for (const [name, help] of Object.entries(COUNTERS)) {
      lines.push(`# HELP ${name} ${help}`, `# TYPE ${name} counter`);
      const series = [...values.keys()].filter((s) => s === name || s.startsWith(`${name}{`)).sort();
      if (series.length === 0) lines.push(`${name} 0`);
      for (const s of series) lines.push(`${s} ${values.get(s)}`);
    }
    const agg = this.one<{ n: number; bytes: number | null; pending: number | null }>(
      "SELECT count(*) AS n, sum(size) AS bytes, sum(pending) AS pending FROM artifacts",
    );
    const gauges: [string, string, number][] = [
      ["turbo_proxy_bytes", "Artifact bytes currently cached on disk.", agg?.bytes ?? 0],
      ["turbo_proxy_artifacts", "Artifacts currently cached on disk.", agg?.n ?? 0],
      ["turbo_proxy_pending_uploads", "Artifacts stored locally but not yet uploaded.", agg?.pending ?? 0],
      ["turbo_proxy_max_bytes", "Configured LRU cap.", readSettings(this.env).maxBytes],
    ];
    for (const [name, help, v] of gauges) lines.push(`# HELP ${name} ${help}`, `# TYPE ${name} gauge`, `${name} ${v}`);
    return `${lines.join("\n")}\n`;
  }

  // ---- helpers ---------------------------------------------------------------------------

  private bump(series: string, n = 1): void {
    this.sql.exec(
      "INSERT INTO counters (series, value) VALUES (?, ?) ON CONFLICT (series) DO UPDATE SET value = value + excluded.value",
      series,
      n,
    );
  }

  private pendingCount(): number {
    return this.one<{ n: number }>("SELECT count(*) AS n FROM artifacts WHERE pending = 1")?.n ?? 0;
  }

  /** Strictly increasing access clock (Date.now() does not advance within one event). */
  private tick(): number {
    this.clock = Math.max(Date.now(), this.clock + 1);
    return this.clock;
  }

  private intervalMs(): number {
    try {
      return readSettings(this.env).maintenanceIntervalSeconds * 1000;
    } catch {
      return 60_000;
    }
  }

  private one<T extends Record<string, SqlStorageValue>>(query: string, ...args: SqlStorageValue[]): T | null {
    return this.sql.exec<T>(query, ...args).toArray()[0] ?? null;
  }
}
