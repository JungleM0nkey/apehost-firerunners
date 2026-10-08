import { describe, expect, it } from "vitest";
import { UPSTREAM_TOKEN } from "./constants";
import { api, disk, eventually, metric, put, RO, text, uniqueHash, upstream } from "./helpers";

const ART = "/v8/artifacts";

describe("auth", () => {
  it("rejects missing and unknown tokens on every route", async () => {
    const hash = uniqueHash("auth");
    const routes: [string, string, BodyInit?][] = [
      ["GET", `${ART}/${hash}`],
      ["HEAD", `${ART}/${hash}`],
      ["PUT", `${ART}/${hash}`, "x"],
      ["POST", ART, JSON.stringify({ hashes: [hash] })],
      ["GET", `${ART}/status`],
      ["POST", `${ART}/events`, "[]"],
      ["GET", "/metrics"],
      ["GET", "/"],
    ];
    const before = await metric("turbo_proxy_auth_failures_total");
    for (const token of [null, "wrong-token", `${RO}x`, ""]) {
      for (const [method, path, body] of routes) {
        const res = await api(path, { method, body, token });
        expect(res.status, `${method} ${path} with ${token}`).toBe(401);
        await res.body?.cancel();
      }
    }
    await eventually(
      async () => (await metric("turbo_proxy_auth_failures_total")) === before + 4 * routes.length,
      "auth failure counter",
    );
    expect(await upstream.requestsFor(hash)).toEqual([]);
  });

  it("rejects PUT with a read-only token but allows reads", async () => {
    const hash = uniqueHash("ro");
    const res = await put(`${ART}/${hash}`, "data", {}, RO);
    expect(res.status).toBe(403);
    expect((await api(`${ART}/${hash}`, { method: "HEAD", token: RO })).status).toBe(404);
    expect((await api(`${ART}/status`, { token: RO })).status).toBe(200);
    expect((await upstream.requestsFor(hash)).map((r) => r.method)).toEqual(["HEAD"]);
  });

  it("accepts any token in the comma-separated list", async () => {
    const res = await api(`${ART}/status`, { token: "rw-token-2" });
    expect(res.status).toBe(200);
  });

  it("does not serve metrics on the main entrypoint", async () => {
    const res = await api("/metrics");
    expect(res.status).toBe(404);
  });
});

describe("reads", () => {
  it("answers HEAD for a local artifact without contacting upstream", async () => {
    const hash = uniqueHash("headhit");
    expect((await put(`${ART}/${hash}`, "hello", { "x-artifact-duration": "42" })).status).toBe(202);
    await upstream.clearLog(hash);

    const res = await api(`${ART}/${hash}`, { method: "HEAD" });
    expect(res.status).toBe(200);
    expect(res.headers.get("x-artifact-duration")).toBe("42");
    // Only the write-behind upload may reach upstream, never a lookup.
    expect((await upstream.requestsFor(hash)).filter((r) => r.method !== "PUT")).toEqual([]);
  });

  it("forwards HEAD misses upstream with the proxy token and relays the answer", async () => {
    const hash = uniqueHash("headmiss");
    const miss = await api(`${ART}/${hash}`, { method: "HEAD" });
    expect(miss.status).toBe(404);

    await upstream.seed(`team_default_team/${hash}`, "remote bytes", "dGFnLWhlYWQ=");
    const hit = await api(`${ART}/${hash}`, { method: "HEAD" });
    expect(hit.status).toBe(200);
    expect(hit.headers.get("x-artifact-tag")).toBe("dGFnLWhlYWQ=");

    const reqs = await upstream.requestsFor(hash);
    expect(reqs.map((r) => r.method)).toEqual(["HEAD", "HEAD"]);
    expect(reqs.every((r) => r.authorization === `Bearer ${UPSTREAM_TOKEN}`)).toBe(true);
    // HEAD must not download (and therefore not cache) the artifact.
    expect((await disk(`team_default_team/${hash}`, { method: "HEAD" })).status).toBe(404);
  });

  it("reads through: streams upstream bytes, keeps a copy, serves the next GET locally", async () => {
    const hash = uniqueHash("readthrough");
    const body = "gzip-tarball-bytes-\u0000\u0001ÿ";
    await upstream.seed(`team_default_team/${hash}`, body, "c2lnbmF0dXJl");

    const first = await api(`${ART}/${hash}`);
    expect(first.status).toBe(200);
    expect(first.headers.get("x-artifact-tag")).toBe("c2lnbmF0dXJl");
    expect(await text(first)).toBe(body);

    await eventually(async () => (await disk(`team_default_team/${hash}`, { method: "HEAD" })).status === 200, "copy on disk");
    await eventually(async () => (await api(`${ART}/${hash}`, { method: "HEAD" })).headers.get("x-cache-proxy") === "local-hit", "index entry");
    const second = await api(`${ART}/${hash}`);
    expect(second.status).toBe(200);
    expect(second.headers.get("x-artifact-tag")).toBe("c2lnbmF0dXJl");
    expect(second.headers.get("content-type")).toBe("application/octet-stream");
    expect(await text(second)).toBe(body);

    const gets = (await upstream.requestsFor(hash)).filter((r) => r.method === "GET");
    expect(gets).toHaveLength(1);
  });

  it("replays stored artifact headers verbatim on local hits", async () => {
    const hash = uniqueHash("replay");
    const headers = {
      "x-artifact-duration": "1234",
      "x-artifact-tag": "AbC+/=tag==",
      "x-artifact-sha": "deadbeef",
      "x-artifact-dirty-hash": "f00d",
    };
    const bytes = new Uint8Array([0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 3, 1, 2, 3, 255]);
    expect((await put(`${ART}/${hash}`, bytes, headers)).status).toBe(202);
    await upstream.clearLog(hash);

    for (const method of ["GET", "HEAD"]) {
      const res = await api(`${ART}/${hash}`, { method });
      expect(res.status).toBe(200);
      for (const [k, v] of Object.entries(headers)) expect(res.headers.get(k), `${method} ${k}`).toBe(v);
      expect(res.headers.get("content-encoding")).toBeNull();
      if (method === "GET") expect(new Uint8Array(await res.arrayBuffer())).toEqual(bytes);
    }
    expect((await upstream.requestsFor(hash)).filter((r) => r.method !== "PUT")).toEqual([]);
  });

  it("treats an index entry whose bytes vanished from disk as a miss", async () => {
    const hash = uniqueHash("wiped");
    await put(`${ART}/${hash}`, "soon gone");
    await eventually(async () => (await upstream.object(`team_default_team/${hash}`)).status === 200, "upload");
    expect((await disk(`team_default_team/${hash}`, { method: "DELETE" })).status).toBe(204);
    await upstream.clearLog(hash);

    const res = await api(`${ART}/${hash}`);
    expect(res.status).toBe(200); // falls back to upstream
    expect(res.headers.get("x-cache-proxy")).toBe("upstream-hit");
    expect(await text(res)).toBe("soon gone");
    expect((await upstream.requestsFor(hash)).map((r) => r.method)).toEqual(["GET"]);
  });

  it("answers misses with 404 and treats upstream failure as a miss", async () => {
    const hash = uniqueHash("miss");
    expect((await api(`${ART}/${hash}`)).status).toBe(404);
    await upstream.down(true);
    try {
      const errors = await metric("turbo_proxy_upstream_errors_total");
      expect((await api(`${ART}/${hash}`)).status).toBe(404);
      await eventually(async () => (await metric("turbo_proxy_upstream_errors_total")) === errors + 1, "error counter");
    } finally {
      await upstream.down(false);
    }
  });
});

describe("write-behind", () => {
  it("stores locally, answers 202 and uploads with the proxy's token and the tag", async () => {
    const hash = uniqueHash("put");
    const res = await put(`${ART}/${hash}?slug=apehost`, "artifact-body", {
      "x-artifact-duration": "77",
      "x-artifact-tag": "dGFnLXB1dA==",
      "x-artifact-client-ci": "GITHUB_ACTIONS",
    });
    expect(res.status).toBe(202);

    await eventually(async () => (await upstream.object(`apehost/${hash}`)).status === 200, "upstream upload");
    const obj = await upstream.object(`apehost/${hash}`);
    expect(await text(obj)).toBe("artifact-body");
    expect(obj.headers.get("x-artifact-tag")).toBe("dGFnLXB1dA==");

    const [upload] = (await upstream.requestsFor(hash)).filter((r) => r.method === "PUT");
    expect(upload).toBeDefined();
    expect(upload!.authorization).toBe(`Bearer ${UPSTREAM_TOKEN}`);
    expect(upload!.query).toEqual({ slug: "apehost" });
    expect(upload!.headers).toMatchObject({
      "content-type": "application/octet-stream",
      "content-length": "13",
      "x-artifact-duration": "77",
      "x-artifact-tag": "dGFnLXB1dA==",
      "x-artifact-client-ci": "GITHUB_ACTIONS",
    });
  });

  it("rejects unsafe hashes and namespaces", async () => {
    for (const path of [
      `${ART}/..%2Fescape`,
      `${ART}/.hidden`,
      `${ART}/a.b`,
      `${ART}/ok?slug=..`,
      `${ART}/ok?slug=a%2Fb`,
      `${ART}/ok?teamId=.dot`,
    ]) {
      const res = await put(path, "x");
      expect(res.status, path).toBe(400);
    }
  });
});

describe("namespacing", () => {
  it("keeps the same hash under different slugs/teams apart and forwards the query", async () => {
    const hash = uniqueHash("ns");
    await put(`${ART}/${hash}?slug=team-a`, "from A");
    await put(`${ART}/${hash}?slug=team-b`, "from B");
    await put(`${ART}/${hash}?teamId=team_c&slug=ignored`, "from C");

    expect(await text(await api(`${ART}/${hash}?slug=team-a`))).toBe("from A");
    expect(await text(await api(`${ART}/${hash}?slug=team-b`))).toBe("from B");
    expect(await text(await api(`${ART}/${hash}?teamId=team_c`))).toBe("from C");
    expect((await api(`${ART}/${hash}`)).status).toBe(404); // default namespace is distinct

    await eventually(async () => (await upstream.object(`team_c/${hash}`)).status === 200, "uploads");
    await eventually(async () => (await upstream.object(`team-b/${hash}`)).status === 200, "uploads");
    expect(await text(await upstream.object(`team-a/${hash}`))).toBe("from A");
    const queries = (await upstream.requestsFor(hash)).filter((r) => r.method === "PUT").map((r) => r.query);
    expect(queries).toEqual(
      expect.arrayContaining([{ slug: "team-a" }, { slug: "team-b" }, { teamId: "team_c", slug: "ignored" }]),
    );
    // The default-namespace miss was forwarded without inventing a query.
    const lastGet = (await upstream.requestsFor(hash)).filter((r) => r.method === "GET").at(-1);
    expect(lastGet?.query).toEqual({});
  });
});

describe("other endpoints", () => {
  it("answers status and events locally", async () => {
    const status = await api(`${ART}/status?slug=x`);
    expect(await status.json()).toEqual({ status: "enabled" });
    const events = await api(`${ART}/events`, {
      method: "POST",
      body: JSON.stringify([{ sessionId: "s", source: "REMOTE", event: "HIT", hash: "abc", duration: 1 }]),
      headers: { "content-type": "application/json" },
    });
    expect(events.status).toBe(200);
    const log = await upstream.log();
    expect(log.some((r) => r.path.endsWith("/status") || r.path.endsWith("/events"))).toBe(false);
  });

  it("answers the batch query from the local index and omits unknown hashes", async () => {
    const known = uniqueHash("batch");
    const unknown = uniqueHash("batchmiss");
    await put(`${ART}/${known}?slug=batch`, "b", {
      "x-artifact-duration": "250",
      "x-artifact-sha": "abc123",
      "x-artifact-dirty-hash": "dirty1",
    });
    const res = await api(`${ART}?slug=batch`, {
      method: "POST",
      body: JSON.stringify({ hashes: [known, unknown] }),
      headers: { "content-type": "application/json" },
    });
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual({ [known]: { taskDurationMs: 250, sha: "abc123", dirtyHash: "dirty1" } });

    const bad = await api(ART, { method: "POST", body: "nope" });
    expect(bad.status).toBe(400);
  });
});

describe("metrics", () => {
  it("moves the hit, miss, upstream-hit and storage counters", async () => {
    const hash = uniqueHash("metrics");
    const read = async () => ({
      local: await metric('turbo_proxy_local_hits_total{method="GET"}'),
      upstream: await metric('turbo_proxy_upstream_hits_total{method="GET"}'),
      miss: await metric('turbo_proxy_misses_total{method="GET"}'),
      stored: await metric("turbo_proxy_stored_bytes_total"),
      uploads: await metric("turbo_proxy_uploads_total"),
    });
    const before = await read();

    await text(await api(`${ART}/${hash}`)); // miss
    await upstream.seed(`team_default_team/${hash}`, "0123456789");
    await text(await api(`${ART}/${hash}`)); // upstream hit + read-through copy
    await eventually(async () => (await read()).stored >= before.stored + 10, "stored bytes");
    await eventually(async () => (await api(`${ART}/${hash}`, { method: "HEAD" })).headers.get("x-cache-proxy") === "local-hit", "index");
    await text(await api(`${ART}/${hash}`)); // local hit
    await put(`${ART}/${uniqueHash("metricsput")}`, "abc");

    await eventually(async () => (await read()).uploads === before.uploads + 1, "upload counter");
    const after = await read();
    expect(after.miss).toBe(before.miss + 1);
    expect(after.upstream).toBe(before.upstream + 1);
    expect(after.local).toBe(before.local + 1);
    expect(after.stored).toBe(before.stored + 13);

    const m = await metric("turbo_proxy_max_bytes");
    expect(m).toBe(1000);
    expect(await metric("turbo_proxy_artifacts")).toBeGreaterThan(0);
    expect(await metric("turbo_proxy_bytes")).toBeGreaterThan(0);
  });
});
