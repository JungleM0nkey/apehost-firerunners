import { WorkerEntrypoint } from "cloudflare:workers";
import { authenticate } from "./auth";
import type { Entry } from "./cache-index";
import {
  DEFAULT_TEAM_ID,
  diskUrl,
  type Env,
  isSafeSegment,
  pickHeaders,
  readSettings,
  REPLAY_HEADERS,
  type Settings,
  UPLOAD_HEADERS,
  upstreamHeaders,
  upstreamUrl,
} from "./env";

export { CacheIndex } from "./cache-index";

const json = (body: unknown, status = 200, headers: HeadersInit = {}) =>
  Response.json(body, { status, headers });
const notFound = () => json({}, 404);

const indexOf = (env: Env) => env.INDEX.get(env.INDEX.idFromName("index"));
type Index = ReturnType<typeof indexOf>;

let started = false;

/** Artifact request context: namespace (upstream key prefix) and the query to forward. */
interface Target {
  ns: string;
  query: string;
}

function target(url: URL): Target | null {
  const teamId = url.searchParams.get("teamId");
  const slug = url.searchParams.get("slug");
  const ns = teamId ?? slug ?? DEFAULT_TEAM_ID;
  if (!isSafeSegment(ns)) return null;
  const q = new URLSearchParams();
  if (teamId !== null) q.set("teamId", teamId);
  if (slug !== null) q.set("slug", slug);
  const query = q.size > 0 ? `?${q}` : "";
  return { ns, query };
}

function artifactResponse(entry: Entry, body: ReadableStream | null, source: string, length?: string | null) {
  const headers = new Headers({ "content-type": "application/octet-stream", "x-cache-proxy": source });
  for (const name of REPLAY_HEADERS) {
    const v = entry.headers[name];
    if (v !== undefined) headers.set(name, v);
  }
  if (length) headers.set("content-length", length);
  return new Response(body, { status: 200, headers });
}

async function handle(request: Request, env: Env, ctx: ExecutionContext): Promise<Response> {
  const settings = readSettings(env);
  const index = indexOf(env);
  if (!started) {
    started = true;
    ctx.waitUntil(index.kick()); // wakes the index so leftover pending uploads get retried
  }

  const url = new URL(request.url);
  const scope = await authenticate(request, env);
  if (scope === null) {
    ctx.waitUntil(index.count("turbo_proxy_auth_failures_total"));
    return json({ error: "unauthorized" }, 401, { "www-authenticate": "Bearer" });
  }

  const path = url.pathname.replace(/\/+$/, "");
  const method = request.method;
  if (path === "/v8/artifacts/status" && method === "GET") return json({ status: "enabled" });
  if (path === "/v8/artifacts/events" && method === "POST") return json({});

  const t = target(url);
  if (path === "/v8/artifacts" && method === "POST") {
    if (!t) return json({ error: "invalid teamId/slug" }, 400);
    const body = (await request.json().catch(() => null)) as { hashes?: unknown } | null;
    const hashes = Array.isArray(body?.hashes) ? body.hashes.filter((h): h is string => typeof h === "string") : null;
    if (!hashes) return json({ error: "expected {\"hashes\": [...]}" }, 400);
    return json(await index.batch(t.ns, hashes.filter(isSafeSegment).slice(0, 10_000)));
  }

  const m = /^\/v8\/artifacts\/([^/]+)$/.exec(path);
  if (!m) return notFound();
  const hash = m[1]!;
  if (!t || !isSafeSegment(hash)) return json({ error: "invalid artifact hash or teamId/slug" }, 400);

  switch (method) {
    case "HEAD":
    case "GET":
      return read(method, hash, t, env, ctx, settings, index);
    case "PUT":
      if (scope !== "rw") return json({ error: "token is read-only" }, 403);
      return write(request, hash, t, env, ctx, index);
    default:
      return json({ error: "method not allowed" }, 405, { allow: "GET, HEAD, PUT" });
  }
}

async function read(
  method: "GET" | "HEAD",
  hash: string,
  t: Target,
  env: Env,
  ctx: ExecutionContext,
  settings: Settings,
  index: Index,
): Promise<Response> {
  const count = (name: string) => ctx.waitUntil(index.count(`turbo_proxy_${name}_total{method="${method}"}`));

  const entry = await index.lookup(t.ns, hash);
  if (entry) {
    const file = await env.DISK.fetch(diskUrl(t.ns, hash), { method });
    if (file.ok) {
      count("local_hits");
      return artifactResponse(entry, method === "GET" ? file.body : null, "local-hit", file.headers.get("content-length"));
    }
    await file.body?.cancel();
    await index.forget(t.ns, hash); // index said yes, disk said no (e.g. tmpfs wiped)
  }

  let res: Response;
  try {
    res = await env.UPSTREAM.fetch(upstreamUrl(settings, hash, t.query), { method, headers: upstreamHeaders(env) });
  } catch (err) {
    console.warn(`upstream ${method} ${hash} failed: ${String(err)}`);
    ctx.waitUntil(index.count("turbo_proxy_upstream_errors_total"));
    count("misses");
    return notFound();
  }
  if (res.status !== 200) {
    await res.body?.cancel();
    if (res.status !== 404) {
      console.warn(`upstream ${method} ${hash} answered ${res.status}`);
      ctx.waitUntil(index.count("turbo_proxy_upstream_errors_total"));
    }
    count("misses");
    return notFound();
  }

  count("upstream_hits");
  const upstreamEntry: Entry = { size: 0, headers: pickHeaders(res.headers, REPLAY_HEADERS) };
  const length = res.headers.get("content-length");
  if (method === "HEAD" || !res.body) {
    await res.body?.cancel();
    return artifactResponse(upstreamEntry, null, "upstream-hit", length);
  }

  // Read-through: stream to the client while a copy goes to disk.
  const size = length === null ? NaN : Number(length);
  if (Number.isSafeInteger(size) && size > settings.maxBytes) {
    return artifactResponse(upstreamEntry, res.body, "upstream-hit", length);
  }
  const [toClient, toDisk] = res.body.tee();
  ctx.waitUntil(store(env, index, t, hash, toDisk, size, upstreamEntry.headers, false));
  return artifactResponse(upstreamEntry, toClient, "upstream-hit", length);
}

async function write(
  request: Request,
  hash: string,
  t: Target,
  env: Env,
  ctx: ExecutionContext,
  index: Index,
): Promise<Response> {
  if (!request.body) return json({ error: "missing body" }, 400);
  const length = Number(request.headers.get("content-length") ?? NaN);
  const ok = await store(env, index, t, hash, request.body, length, pickHeaders(request.headers, UPLOAD_HEADERS), true);
  if (!ok) return json({ error: "could not store artifact" }, 500);
  ctx.waitUntil(index.upload(t.ns, hash)); // write-behind; the alarm retries on failure
  return json({ urls: [`${new URL(request.url).origin}/v8/artifacts/${hash}${t.query}`] }, 202);
}

/** Write bytes to the disk service, then record them in the index. */
async function store(
  env: Env,
  index: Index,
  t: Target,
  hash: string,
  body: ReadableStream,
  length: number,
  headers: Record<string, string>,
  pending: boolean,
): Promise<boolean> {
  try {
    let stream = body;
    if (Number.isSafeInteger(length) && length >= 0) {
      const fixed = new FixedLengthStream(length);
      body.pipeTo(fixed.writable).catch(() => {});
      stream = fixed.readable;
    }
    const put = await env.DISK.fetch(diskUrl(t.ns, hash), { method: "PUT", body: stream });
    await put.body?.cancel();
    if (!put.ok) throw new Error(`disk PUT ${put.status}`);
    const stat = await env.DISK.fetch(diskUrl(t.ns, hash), { method: "HEAD" });
    const size = Number(stat.headers.get("content-length"));
    if (!stat.ok || !Number.isSafeInteger(size)) throw new Error(`disk HEAD ${stat.status}`);
    await index.record(t.ns, hash, size, headers, t.query, pending);
    return true;
  } catch (err) {
    console.warn(`store ${t.ns}/${hash} failed: ${String(err)}`);
    return false;
  }
}

export default {
  async fetch(request, env, ctx): Promise<Response> {
    try {
      return await handle(request, env, ctx);
    } catch (err) {
      console.error(`request failed: ${String(err)}`);
      return json({ error: "internal error" }, 500);
    }
  },
} satisfies ExportedHandler<Env>;

/** Loopback-only entrypoint (separate socket): Prometheus metrics and a health check. */
export class Metrics extends WorkerEntrypoint<Env> {
  override async fetch(request: Request): Promise<Response> {
    const { pathname } = new URL(request.url);
    const index = indexOf(this.env);
    if (pathname === "/healthz") {
      // Touching the index proves its storage works and, right after a start, wakes it so
      // leftover pending uploads are retried without waiting for the first VM request.
      await index.kick();
      return new Response("ok\n");
    }
    if (pathname !== "/metrics") return new Response("not found\n", { status: 404 });
    const text = await index.metrics();
    return new Response(text, { headers: { "content-type": "text/plain; version=0.0.4; charset=utf-8" } });
  }
}
