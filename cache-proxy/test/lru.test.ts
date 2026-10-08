import { expect, it } from "vitest";
import { api, eventually, metric, put, text, uniqueHash, upstream } from "./helpers";

// vitest.config.ts caps the cache at 1000 bytes; every artifact here is 300 bytes.
const blob = (c: string) => c.repeat(300);
const head = async (hash: string) => (await api(`/v8/artifacts/${hash}?slug=lru`, { method: "HEAD" })).headers.get("x-cache-proxy");

it("evicts the least recently used artifact (not the oldest) once over maxBytes", async () => {
  const [a, b, c, d] = ["a", "b", "c", "d"].map((l) => uniqueHash(`lru${l}`)) as [string, string, string, string];
  for (const [hash, ch] of [[a, "a"], [b, "b"], [c, "c"]] as const) {
    expect((await put(`/v8/artifacts/${hash}?slug=lru`, blob(ch))).status).toBe(202);
  }
  await eventually(async () => (await metric("turbo_proxy_pending_uploads")) === 0, "uploads");
  expect(await metric("turbo_proxy_bytes")).toBe(900);

  // Touch A so B becomes the least recently used; FIFO would evict A instead.
  expect(await text(await api(`/v8/artifacts/${a}?slug=lru`))).toBe(blob("a"));

  const evictions = await metric("turbo_proxy_evictions_total");
  expect((await put(`/v8/artifacts/${d}?slug=lru`, blob("d"))).status).toBe(202);
  await eventually(async () => (await metric("turbo_proxy_pending_uploads")) === 0, "upload of d");

  expect(await metric("turbo_proxy_evictions_total")).toBe(evictions + 1);
  expect(await metric("turbo_proxy_bytes")).toBe(900);
  expect(await head(a)).toBe("local-hit");
  expect(await head(c)).toBe("local-hit");
  expect(await head(d)).toBe("local-hit");
  await upstream.clearLog(b);
  // B is gone locally but still upstream (it was uploaded before being evicted).
  const res = await api(`/v8/artifacts/${b}?slug=lru`, { method: "HEAD" });
  expect(res.headers.get("x-cache-proxy")).toBe("upstream-hit");
  expect((await upstream.requestsFor(b)).map((r) => r.method)).toEqual(["HEAD"]);
});

it("never evicts artifacts that are still waiting to be uploaded", async () => {
  await upstream.failPuts(true);
  try {
    const hashes = ["p", "q", "r", "s"].map((l) => uniqueHash(`pend${l}`));
    for (const h of hashes) expect((await put(`/v8/artifacts/${h}?slug=lru`, blob("p"))).status).toBe(202);
    await eventually(async () => (await metric("turbo_proxy_pending_uploads")) === 4, "pending");
    // Over the cap, but nothing pending may be dropped.
    for (const h of hashes) expect(await head(h)).toBe("local-hit");
  } finally {
    await upstream.failPuts(false);
  }
});
