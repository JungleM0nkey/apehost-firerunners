import { evictDurableObject, runDurableObjectAlarm } from "cloudflare:test";
import { expect, it } from "vitest";
import { UPSTREAM_TOKEN } from "./constants";
import { api, eventually, indexStub, metric, put, text, uniqueHash, upstream } from "./helpers";

it("keeps a pending marker when the upload fails and retries it after a restart", async () => {
  const hash = uniqueHash("pending");
  await upstream.failPuts(true);
  try {
    const failures = await metric("turbo_proxy_upload_failures_total");
    const res = await put(`/v8/artifacts/${hash}?slug=retry`, "write-behind", { "x-artifact-tag": "cmV0cnk=" });
    expect(res.status).toBe(202);
    await eventually(async () => (await metric("turbo_proxy_upload_failures_total")) > failures, "failed upload");
    expect(await metric("turbo_proxy_pending_uploads")).toBe(1);
    // Still served locally while pending.
    expect(await text(await api(`/v8/artifacts/${hash}?slug=retry`))).toBe("write-behind");
  } finally {
    await upstream.failPuts(false);
  }
  expect((await upstream.object(`retry/${hash}`)).status).toBe(404);

  // "Restart": drop the in-memory instance; the fresh one reschedules the retry alarm.
  const stub = indexStub();
  await evictDurableObject(stub);
  expect(await metric("turbo_proxy_pending_uploads")).toBe(1); // marker survived
  expect(await runDurableObjectAlarm(stub)).toBe(true);

  expect(await metric("turbo_proxy_pending_uploads")).toBe(0);
  const obj = await upstream.object(`retry/${hash}`);
  expect(await text(obj)).toBe("write-behind");
  expect(obj.headers.get("x-artifact-tag")).toBe("cmV0cnk=");
  const puts = (await upstream.requestsFor(hash)).filter((r) => r.method === "PUT");
  expect(puts.length).toBeGreaterThanOrEqual(2);
  expect(puts.every((r) => r.authorization === `Bearer ${UPSTREAM_TOKEN}` && r.query.slug === "retry")).toBe(true);

  // The maintenance alarm reschedules itself.
  expect(await runDurableObjectAlarm(stub)).toBe(true);
});
