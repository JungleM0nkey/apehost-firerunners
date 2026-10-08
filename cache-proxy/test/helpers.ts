import { env, exports } from "cloudflare:workers";
import type { Env } from "../src/env";

export const RW = "rw-token";
export const RO = "ro-token";

const proxyEnv = env as unknown as Env;
const main = (exports as unknown as { default: Fetcher; Metrics: Fetcher });

/** Call the proxy's main (bridge) entrypoint. */
export function api(path: string, init: RequestInit & { token?: string | null } = {}): Promise<Response> {
  const { token = RW, ...rest } = init;
  const headers = new Headers(rest.headers);
  if (token !== null) headers.set("authorization", `Bearer ${token}`);
  return main.default.fetch(`http://10.200.0.1:8787${path}`, { ...rest, headers });
}

export function put(path: string, body: string | Uint8Array, headers: Record<string, string> = {}, token = RW) {
  return api(path, {
    method: "PUT",
    token,
    body,
    headers: { "content-type": "application/octet-stream", "content-length": String(typeof body === "string" ? new TextEncoder().encode(body).byteLength : body.byteLength), ...headers },
  });
}

/** Call the loopback-only metrics entrypoint and parse the Prometheus text. */
export async function metrics(): Promise<Map<string, number>> {
  const res = await main.Metrics.fetch("http://127.0.0.1:9787/metrics");
  const out = new Map<string, number>();
  for (const line of (await res.text()).split("\n")) {
    if (!line || line.startsWith("#")) continue;
    const i = line.lastIndexOf(" ");
    out.set(line.slice(0, i), Number(line.slice(i + 1)));
  }
  return out;
}

export const metric = async (series: string) => (await metrics()).get(series) ?? 0;

export interface UpstreamRequest {
  method: string;
  path: string;
  query: Record<string, string>;
  authorization: string | null;
  headers: Record<string, string>;
}

/** Control/inspection endpoints of the fake upstream (test/node/fakes.ts). */
export const upstream = {
  control: (op: string, init?: RequestInit) => proxyEnv.UPSTREAM.fetch(`http://upstream.test/__control/${op}`, init),
  async log(): Promise<UpstreamRequest[]> {
    return (await upstream.control("log")).json();
  },
  async requestsFor(hash: string): Promise<UpstreamRequest[]> {
    return (await upstream.log()).filter((r) => r.path === `/v8/artifacts/${hash}`);
  },
  clearLog: (hash: string) => upstream.control(`clear-log?hash=${encodeURIComponent(hash)}`),
  seed: (key: string, body: string, tag?: string) =>
    upstream.control(`seed?key=${encodeURIComponent(key)}`, {
      method: "POST",
      body,
      headers: tag ? { "x-artifact-tag": tag } : {},
    }),
  object: (key: string) => upstream.control(`object?key=${encodeURIComponent(key)}`),
  failPuts: (on: boolean) => upstream.control(`fail-puts?on=${on ? 1 : 0}`),
  down: (on: boolean) => upstream.control(`down?on=${on ? 1 : 0}`),
};

/** Raw access to the fake disk, to simulate a wiped tmpfs. */
export const disk = (path: string, init?: RequestInit) => proxyEnv.DISK.fetch(`http://disk/${path}`, init);

export function indexStub() {
  return proxyEnv.INDEX.get(proxyEnv.INDEX.idFromName("index"));
}

/** Poll until `check` returns true (background work runs in ctx.waitUntil). */
export async function eventually(check: () => Promise<boolean> | boolean, what = "condition", ms = 3000) {
  const deadline = Date.now() + ms;
  for (;;) {
    if (await check()) return;
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    await new Promise((r) => setTimeout(r, 10));
  }
}

let n = 0;
/** Unique hash per call: fake services are shared between test files. */
export const uniqueHash = (label: string) => `${label}${Date.now().toString(16)}${(n++).toString(16)}`.replace(/[^A-Za-z0-9_-]/g, "");

/** Decode a body as UTF-8 (artifacts are octet-stream, so Response.text() would warn). */
export const text = async (res: Response) => new TextDecoder().decode(await res.arrayBuffer());
