// Minimal stand-in for the upstream Worker (AdiRishi/turborepo-remote-cache-cloudflare) used by
// scripts/smoke.sh: one bearer token, objects keyed `${teamId ?? slug ?? "team_default_team"}/${hash}`,
// only x-artifact-tag stored. /__control/* is unauthenticated test plumbing.
// Usage: node fake-upstream.mjs <port> <token>
import { createServer } from "node:http";

const [port, token] = [Number(process.argv[2]), process.argv[3]];
const objects = new Map();
const log = [];
let failPuts = false;

const readBody = (req) =>
  new Promise((resolve, reject) => {
    const chunks = [];
    req.on("data", (c) => chunks.push(c));
    req.on("end", () => resolve(Buffer.concat(chunks)));
    req.on("error", reject);
  });

createServer(async (req, res) => {
  const url = new URL(req.url, "http://upstream");
  const body = await readBody(req);
  const send = (status, data = "", headers = {}) => {
    res.writeHead(status, headers);
    res.end(req.method === "HEAD" ? undefined : data);
  };

  if (url.pathname.startsWith("/__control/")) {
    const op = url.pathname.slice("/__control/".length);
    if (op === "log") return send(200, JSON.stringify(log), { "content-type": "application/json" });
    if (op === "fail-puts") failPuts = url.searchParams.get("on") === "1";
    if (op === "object") {
      const obj = objects.get(url.searchParams.get("key"));
      if (!obj) return send(404);
      return send(200, obj.body, obj.tag ? { "x-artifact-tag": obj.tag } : {});
    }
    return send(200, "{}");
  }

  log.push({
    method: req.method,
    path: url.pathname,
    query: Object.fromEntries(url.searchParams),
    authorization: req.headers.authorization ?? null,
    tag: req.headers["x-artifact-tag"] ?? null,
  });
  if (req.headers.authorization !== `Bearer ${token}`) return send(401, "Unauthorized");
  const m = /^\/v8\/artifacts\/([^/]+)$/.exec(url.pathname);
  if (!m) return send(404, "{}");
  const key = `${url.searchParams.get("teamId") ?? url.searchParams.get("slug") ?? "team_default_team"}/${m[1]}`;

  if (req.method === "PUT") {
    if (failPuts) return send(500, "boom");
    if (req.headers["content-type"] !== "application/octet-stream") return send(400, "bad content-type");
    if (Number(req.headers["content-length"]) !== body.length) return send(400, "bad content-length");
    objects.set(key, { body, tag: req.headers["x-artifact-tag"] ?? null });
    return send(202, JSON.stringify({ urls: [url.href] }), { "content-type": "application/json" });
  }
  if (req.method === "GET" || req.method === "HEAD") {
    const obj = objects.get(key);
    if (!obj) return send(404, "{}", { "content-type": "application/json" });
    const headers = { "content-type": "application/octet-stream", "content-length": obj.body.length };
    if (obj.tag) headers["x-artifact-tag"] = obj.tag;
    return send(200, obj.body, headers);
  }
  return send(405);
}).listen(port, "127.0.0.1", () => console.log(`fake upstream on 127.0.0.1:${port}`));
