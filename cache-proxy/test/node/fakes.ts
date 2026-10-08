// Fake services wired into the test Worker as service bindings. They run in Node (vitest's
// process), so their state is shared by all test files: tests use distinct slugs/hashes.
import { Response, type Request } from "miniflare";
import { UPSTREAM_TOKEN } from "../constants.ts";

export { UPSTREAM_TOKEN };

/** In-memory model of workerd's writable DiskDirectory service. */
export function createFakeDisk() {
  const files = new Map<string, Uint8Array>();
  return async (request: Request): Promise<Response> => {
    const segments = new URL(request.url).pathname.split("/").filter(Boolean).map(decodeURIComponent);
    if (segments.some((s) => s.startsWith(".") || s.includes("/"))) return new Response(null, { status: 404 });
    const path = segments.join("/");
    const file = files.get(path);
    switch (request.method) {
      case "GET":
      case "HEAD": {
        if (file) {
          const headers = { "content-type": "application/octet-stream", "content-length": String(file.byteLength) };
          return new Response(request.method === "GET" ? file : null, { headers });
        }
        const prefix = path === "" ? "" : `${path}/`;
        const names = new Map<string, string>();
        for (const key of files.keys()) {
          if (!key.startsWith(prefix)) continue;
          const [name, ...rest] = key.slice(prefix.length).split("/");
          names.set(name!, rest.length ? "directory" : "file");
        }
        if (names.size === 0 && path !== "") return new Response(null, { status: 404 });
        const listing = [...names].map(([name, type]) => ({ name, type }));
        return new Response(request.method === "GET" ? JSON.stringify(listing) : null, {
          headers: { "content-type": "application/json" },
        });
      }
      case "PUT":
        if (path === "") return new Response(null, { status: 405 });
        files.set(path, new Uint8Array(await request.arrayBuffer())); // atomic replace
        return new Response(null, { status: 204 });
      case "DELETE":
        return new Response(null, { status: files.delete(path) ? 204 : 404 });
      default:
        return new Response(null, { status: 405 });
    }
  };
}

export interface LoggedRequest {
  method: string;
  path: string;
  query: Record<string, string>;
  authorization: string | null;
  headers: Record<string, string>;
}

/**
 * Model of the upstream Worker (AdiRishi/turborepo-remote-cache-cloudflare): one bearer token,
 * objects keyed `${teamId ?? slug ?? "team_default_team"}/${hash}`, only x-artifact-tag kept.
 * `/__control/*` endpoints let tests inspect and steer it.
 */
export function createFakeUpstream() {
  const objects = new Map<string, { body: Uint8Array; tag: string | null }>();
  let log: LoggedRequest[] = [];
  let failPuts = false;
  let down = false;

  return async (request: Request): Promise<Response> => {
    const url = new URL(request.url);
    if (url.pathname.startsWith("/__control/")) {
      const op = url.pathname.slice("/__control/".length);
      if (op === "log") return Response.json(log);
      if (op === "object") {
        const obj = objects.get(url.searchParams.get("key")!);
        if (!obj) return new Response(null, { status: 404 });
        return new Response(obj.body, { headers: obj.tag ? { "x-artifact-tag": obj.tag } : {} });
      }
      if (op === "clear-log") {
        const hash = url.searchParams.get("hash");
        log = hash ? log.filter((r) => r.path !== `/v8/artifacts/${hash}`) : [];
      }
      if (op === "fail-puts") failPuts = url.searchParams.get("on") === "1";
      if (op === "down") down = url.searchParams.get("on") === "1";
      if (op === "seed") {
        objects.set(url.searchParams.get("key")!, {
          body: new Uint8Array(await request.arrayBuffer()),
          tag: request.headers.get("x-artifact-tag"),
        });
      }
      return Response.json({ ok: true });
    }

    const headers: Record<string, string> = {};
    request.headers.forEach((v, k) => {
      if (k.startsWith("x-artifact-") || k === "content-type" || k === "content-length") headers[k] = v;
    });
    log.push({
      method: request.method,
      path: url.pathname,
      query: Object.fromEntries(url.searchParams),
      authorization: request.headers.get("authorization"),
      headers,
    });

    if (down) return new Response("upstream down", { status: 503 });
    if (request.headers.get("authorization") !== `Bearer ${UPSTREAM_TOKEN}`) {
      return new Response("Unauthorized", { status: 401 });
    }
    const m = /^\/v8\/artifacts\/([^/]+)$/.exec(url.pathname);
    if (!m) return new Response("not found", { status: 404 });
    const ns = url.searchParams.get("teamId") ?? url.searchParams.get("slug") ?? "team_default_team";
    const key = `${ns}/${m[1]}`;

    if (request.method === "PUT") {
      if (failPuts) return new Response("boom", { status: 500 });
      if (request.headers.get("content-type") !== "application/octet-stream") {
        return new Response("bad content-type", { status: 400 });
      }
      objects.set(key, { body: new Uint8Array(await request.arrayBuffer()), tag: request.headers.get("x-artifact-tag") });
      return Response.json({ urls: [url.toString()] }, { status: 202 });
    }
    if (request.method === "GET" || request.method === "HEAD") {
      const obj = objects.get(key);
      if (!obj) return Response.json({}, { status: 404 });
      const h: Record<string, string> = { "content-type": "application/octet-stream" };
      if (obj.tag) h["x-artifact-tag"] = obj.tag;
      return new Response(request.method === "GET" ? obj.body : null, { headers: h });
    }
    return new Response(null, { status: 405 });
  };
}
