import { runDurableObjectAlarm } from "cloudflare:test";
import { expect, it } from "vitest";
import { api, indexStub, metric, put, RO, text, uniqueHash, upstream } from "./helpers";

it("stores signed artifacts locally without upstream requests, pending uploads or maintenance alarms", async () => {
  const hash = uniqueHash("local");
  const path = `/v8/artifacts/${hash}?slug=desktop`;
  expect((await api(path)).status).toBe(404);
  expect((await put(path, "local cache", { "x-artifact-tag": "signed-tag" })).status).toBe(202);
  const response = await api(path);
  expect(response.headers.get("x-artifact-tag")).toBe("signed-tag");
  expect(await text(response)).toBe("local cache");
  expect(await upstream.requestsFor(hash)).toEqual([]);
  expect(await metric("turbo_proxy_pending_uploads")).toBe(0);
  expect(await runDurableObjectAlarm(indexStub())).toBe(false);
});

it("evicts on writes while retaining authentication and read-only token checks", async () => {
  const hashes = [0, 1, 2, 3].map(() => uniqueHash("bounded"));
  expect((await api("/v8/artifacts/status", { token: null })).status).toBe(401);
  expect((await put(`/v8/artifacts/${hashes[0]}`, "denied", {}, RO)).status).toBe(403);
  for (const hash of hashes) expect((await put(`/v8/artifacts/${hash}`, "x".repeat(300))).status).toBe(202);
  expect(await metric("turbo_proxy_bytes")).toBe(900);
  expect((await api(`/v8/artifacts/${hashes[0]}`)).status).toBe(404);
  expect((await api(`/v8/artifacts/${hashes[3]}`)).status).toBe(200);
  expect(await runDurableObjectAlarm(indexStub())).toBe(false);
});

it("rejects oversized and unknown-length uploads before writing bytes", async () => {
  const hash = uniqueHash("oversize");
  expect((await put(`/v8/artifacts/${hash}`, "x".repeat(1001))).status).toBe(413);
  const body = new ReadableStream<Uint8Array>({ start(controller) { controller.enqueue(new TextEncoder().encode("unknown length")); controller.close(); } });
  expect((await api(`/v8/artifacts/${hash}`, { method: "PUT", body })).status).toBe(411);
  expect((await api(`/v8/artifacts/${hash}`)).status).toBe(404);
});
