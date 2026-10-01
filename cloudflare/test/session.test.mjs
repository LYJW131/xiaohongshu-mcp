import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { registerHooks } from "node:module";
import test from "node:test";
import { createTrialClass } from "../src/trial.mjs";
import {
  createSessionBridgeClass, handleSessionRequest, installSessionBridge,
  ACTIVITY_URL, MAX_REQUEST_BYTES, MAX_SESSION_BYTES, SESSION_HOST, SESSION_KEY, SESSION_URL,
} from "../src/session.mjs";

const A = "11111111-1111-4111-8111-111111111111";
const B = "22222222-2222-4222-8222-222222222222";
const C = "33333333-3333-4333-8333-333333333333";
const session = (value = "synthetic-test-value") => ({
  version: 2, seed: 23088, saved_at: "2026-10-01T14:00:00Z",
  cookies: [{ name: "web_session", value, domain: ".xiaohongshu.com", path: "/", httpOnly: true,
    partitionKey: { topLevelSite: "https://www.xiaohongshu.com", hasCrossSiteAncestor: false } }],
});

function request(method = "GET", body, headers = {}, url = SESSION_URL) {
  return new Request(url, { method, headers: {
    "X-XHS-Session-Client": "1", ...(body === undefined ? {} : { "Content-Type": "application/json" }), ...headers,
  }, ...(body === undefined ? {} : { body: typeof body === "string" ? body : JSON.stringify(body) }) });
}

const put = (revision = 0, operationId = A, value = session()) => request("PUT", { expected_revision: revision, operation_id: operationId, session: value });
const remove = (revision, operationId = B) => request("DELETE", { expected_revision: revision, operation_id: operationId });

// 模拟串行事务与提交失败，测试不调用真实 Cloudflare 或浏览器。
function storageFixture() {
  let records = new Map();
  let tail = Promise.resolve();
  const fixture = { failure: null, reads: 0, writes: 0, transactions: 0 };
  fixture.records = () => structuredClone(records);
  fixture.corrupt = record => records.set(SESSION_KEY, record);
  fixture.storage = {
    transaction(callback) {
      const operation = tail.then(async () => {
        fixture.transactions++;
        const pending = structuredClone(records);
        const result = await callback({
          async get(key) {
            fixture.reads++;
            if (fixture.failure === "get") throw new Error("synthetic-sensitive-error");
            return structuredClone(pending.get(key));
          },
          async put(key, value) {
            fixture.writes++;
            if (fixture.failure === "put") throw new Error("synthetic-sensitive-error");
            pending.set(key, structuredClone(value));
          },
        });
        if (fixture.failure === "commit") throw new Error("synthetic-sensitive-error");
        records = pending;
        if (fixture.failure === "response") throw new Error("synthetic-sensitive-error");
        return result;
      });
      tail = operation.catch(() => {});
      return operation;
    },
  };
  fixture.call = req => handleSessionRequest(fixture.storage, req);
  return fixture;
}

class Base {
  constructor(ctx, env) { this.ctx = ctx; this.env = env; }
}
const Trial = createTrialClass(Base);
const Bridge = createSessionBridgeClass(Base);

test("empty state, stored CDP fields, and seed round trip without a container", async () => {
  const f = storageFixture();
  assert.deepEqual(await (await f.call(request())).json(), { revision: 0, session: null });
  assert.deepEqual(await (await f.call(put())).json(), { revision: 1, session: session() });
  assert.deepEqual(await (await f.call(request())).json(), { revision: 1, session: session() });
  assert.equal(f.records().size, 1);
  assert.equal(f.writes, 1);
});

test("exact retry is idempotent even when JSON keys are reordered", async () => {
  const f = storageFixture();
  const first = await (await f.call(put())).json();
  const value = session();
  const reordered = { cookies: value.cookies, saved_at: value.saved_at, seed: value.seed, version: value.version };
  assert.deepEqual(await (await f.call(put(0, A, reordered))).json(), first);
  assert.equal(f.writes, 1);
  assert.equal((await f.call(put(1, A, session("different")))).status, 409);
  assert.equal((await f.call(remove(1, A))).status, 409);
});

test("stale overwrite and stale delete cannot replace a newer session", async () => {
  const f = storageFixture();
  await f.call(put());
  await f.call(put(1, B, session("new")));
  for (const stale of [put(), put(1, C), remove(1, C), remove(0)]) {
    const response = await f.call(stale);
    assert.equal(response.status, 409);
    assert.equal((await response.json()).revision, 2);
  }
  assert.deepEqual(await (await f.call(request())).json(), { revision: 2, session: session("new") });
});

test("delete creates a revision tombstone and delayed saves cannot resurrect it", async () => {
  const f = storageFixture();
  await f.call(put());
  assert.deepEqual(await (await f.call(remove(1))).json(), { revision: 2, session: null });
  assert.deepEqual(await (await f.call(remove(1))).json(), { revision: 2, session: null });
  assert.equal((await f.call(put())).status, 409);
  assert.equal((await f.call(put(1, C))).status, 409);
  assert.equal(f.records().size, 1);
  assert.equal(f.writes, 2);
  assert.deepEqual(await (await f.call(put(2, C, session("new-login")))).json(), { revision: 3, session: session("new-login") });
  assert.equal((await f.call(remove(1))).status, 409);
});

test("deleting an empty store also advances its revision", async () => {
  const f = storageFixture();
  assert.deepEqual(await (await f.call(remove(0))).json(), { revision: 1, session: null });
  assert.equal((await f.call(put(0))).status, 409);
});

test("concurrent CAS writes have only one winner", async () => {
  const f = storageFixture();
  const responses = await Promise.all([f.call(put()), f.call(put(0, B))]);
  assert.deepEqual(responses.map(r => r.status).sort(), [200, 409]);
  assert.equal(f.writes, 1);
});

test("recreated DO keeps revision, session, and retry result", async () => {
  const f = storageFixture();
  const first = new Trial({ storage: f.storage }, {});
  await first.sessionRequest(put());
  const recreated = new Trial({ storage: f.storage }, {});
  assert.deepEqual(await (await recreated.sessionRequest(request())).json(), { revision: 1, session: session() });
  assert.deepEqual(await (await recreated.sessionRequest(put())).json(), { revision: 1, session: session() });
  await recreated.sessionRequest(remove(1));
  const third = new Trial({ storage: f.storage }, {});
  assert.deepEqual(await (await third.sessionRequest(request())).json(), { revision: 2, session: null });
  assert.equal((await third.sessionRequest(put())).status, 409);
});

test("bridge uses only trusted object ID and isolated DO storage", async () => {
  const ids = ["a".repeat(64), "b".repeat(64)];
  const stores = ids.map(() => storageFixture());
  const objects = stores.map(f => new Trial({ storage: f.storage }, {}));
  const lookedUp = [];
  const env = { XHS_TRIAL: {
    idFromString(id) { lookedUp.push(id); assert.ok(ids.includes(id)); return id; },
    get(id) { return objects[ids.indexOf(id)]; },
  } };
  const bridges = ids.map(durableObjectId => new Bridge({ props: { durableObjectId } }, env));
  await bridges[0].fetch(put());
  assert.deepEqual(await (await bridges[1].fetch(request())).json(), { revision: 0, session: null });
  const malicious = request("GET", undefined, { "X-Durable-Object-Id": ids[1] });
  assert.deepEqual(await (await bridges[0].fetch(malicious)).json(), { revision: 1, session: session() });
  assert.deepEqual(lookedUp, [ids[0], ids[1], ids[0]]);
  assert.equal((await new Bridge({ props: {} }, env).fetch(request())).status, 503);
});

test("storage read, write, and commit failures fail closed and permit safe retry", async () => {
  for (const failure of ["get", "put", "commit"]) {
    const f = storageFixture();
    f.failure = failure;
    const response = await f.call(put());
    assert.equal(response.status, 503);
    assert.deepEqual(await response.json(), { error: "session store unavailable" });
    assert.equal(f.records().size, 0);
    f.failure = null;
    assert.equal((await f.call(put())).status, 200);
    f.failure = failure;
    assert.equal((await f.call(remove(1))).status, 503);
    f.failure = null;
    assert.deepEqual(await (await f.call(request())).json(), { revision: 1, session: session() });
  }
});

test("corrupt stored data is unavailable and never silently reset", async () => {
  for (const record of [{}, { revision: 0, session: null }, { revision: 1, session: null, operation: {} }]) {
    const f = storageFixture();
    f.corrupt(record);
    assert.equal((await f.call(request())).status, 503);
    assert.equal((await f.call(put())).status, 503);
    assert.equal(f.writes, 0);
  }
});

test("a lost success response is recovered by retrying the same operation", async () => {
  const f = storageFixture();
  f.failure = "response";
  assert.equal((await f.call(put())).status, 503);
  f.failure = null;
  assert.deepEqual(await (await f.call(put())).json(), { revision: 1, session: session() });
  assert.equal(f.writes, 1);
  f.failure = "response";
  assert.equal((await f.call(remove(1))).status, 503);
  f.failure = null;
  assert.deepEqual(await (await f.call(remove(1))).json(), { revision: 2, session: null });
  assert.equal(f.writes, 2);
});

test("browser requests, preflights, forms, and untrusted URLs never reach storage", async () => {
  const f = storageFixture();
  const headers = [
    { "Origin": "https://evil.example" }, { "Origin": "null" }, { "Origin": `http://${SESSION_HOST}` },
    { "Referer": "https://evil.example/" }, { "Sec-Fetch-Site": "cross-site" },
    { "Sec-Fetch-Mode": "navigate" }, { "Sec-Fetch-Dest": "document" }, { "X-XHS-Session-Client": "" },
  ];
  for (const header of headers) assert.equal((await f.call(request("GET", undefined, header))).status, 403);
  for (const url of [
    "http://evil.example/v1/session", `${SESSION_URL}?object=other`, `${SESSION_URL}/`, `${SESSION_URL}#fragment`,
    `https://${SESSION_HOST}/v1/session`, `http://${SESSION_HOST}:8080/v1/session`, `http://${SESSION_HOST}/v1/other`,
  ]) assert.equal((await f.call(request("GET", undefined, {}, url))).status, 404);
  for (const method of ["POST", "PATCH", "OPTIONS", "HEAD"]) assert.equal((await f.call(request(method))).status, 405);
  assert.equal((await f.call(request("PUT", "form=value", { "Content-Type": "application/x-www-form-urlencoded" }))).status, 415);
  assert.equal((await f.call(request("PUT", "{}", { "Content-Type": "text/plain" }))).status, 415);
  assert.equal(f.transactions, 0);
});

test("invalid JSON, revision, operation, cookie array, and session fields are rejected", async () => {
  const f = storageFixture();
  const valid = { expected_revision: 0, operation_id: A, session: session() };
  const invalidBodies = ["{", "null", "[]", {}, { ...valid, extra: true },
    ...[-1, 0.5, "0", Number.MAX_SAFE_INTEGER + 1].map(expected_revision => ({ ...valid, expected_revision })),
    ...[null, "not-uuid", 3].map(operation_id => ({ ...valid, operation_id })),
    ...[null, [], { ...session(), version: 1 }, { ...session(), seed: -1 }, { ...session(), seed: 0.5 },
      { ...session(), saved_at: "2026-02-30T00:00:00Z" }, { ...session(), saved_at: "2026-10-01T24:00:00Z" },
      { ...session(), saved_at: "2026-10-01" }, { ...session(), cookies: null },
      ...["cookie", {}, [null], ["cookie"], [{ name: "x", value: 5 }], [{ name: "", value: "x" }],
        [{ name: "x", value: "x", domain: [] }]].map(cookies => ({ ...session(), cookies })),
    ].map(value => ({ ...valid, session: value })),
  ];
  for (const body of invalidBodies) assert.equal((await f.call(request("PUT", body))).status, 400);
  const nonFinite = JSON.stringify(valid).replace('"httpOnly":true', '"expires":1e400');
  assert.equal((await f.call(request("PUT", nonFinite))).status, 400);
  assert.equal((await f.call(request("DELETE", valid))).status, 400);
  assert.equal((await f.call(request("GET", undefined, { "Content-Length": "1" }))).status, 400);
  assert.equal((await f.call(request("PUT", valid, { "Content-Encoding": "gzip" }))).status, 400);
  assert.equal(f.transactions, 0);
  assert.equal((await f.call(put(0, A, { ...session(), seed: 0, cookies: [] }))).status, 200);
});

test("session and streaming request byte limits apply before storage", async () => {
  const f = storageFixture();
  const empty = session("");
  const baseBytes = new TextEncoder().encode(JSON.stringify(empty)).length;
  const exact = session("x".repeat(MAX_SESSION_BYTES - baseBytes));
  assert.equal((await f.call(put(0, A, exact))).status, 200);
  assert.equal((await f.call(put(1, B, session("x".repeat(MAX_SESSION_BYTES - baseBytes + 1))))).status, 413);
  assert.equal((await f.call(put(1, B, session("书".repeat(MAX_SESSION_BYTES / 3))))).status, 413);
  assert.equal((await f.call(request("PUT", "{}", { "Content-Length": `${MAX_REQUEST_BYTES + 1}` }))).status, 413);
  let canceled = false;
  const stream = new ReadableStream({
    pull(controller) { controller.enqueue(new Uint8Array(1024)); },
    cancel() { canceled = true; },
  });
  const streamed = new Request(SESSION_URL, { method: "PUT", body: stream, duplex: "half",
    headers: { "Content-Type": "application/json", "X-XHS-Session-Client": "1" } });
  assert.equal((await f.call(streamed)).status, 413);
  assert.equal(canceled, true);
  assert.equal(f.transactions, 1);
});

test("responses prohibit caching and never enable browser CORS", async () => {
  const f = storageFixture();
  for (const response of [await f.call(request()), await f.call(request("POST"))]) {
    assert.equal(response.headers.get("Cache-Control"), "no-store");
    assert.equal(response.headers.get("Access-Control-Allow-Origin"), null);
    assert.equal(response.headers.get("X-Content-Type-Options"), "nosniff");
    assert.equal(response.headers.get("Cross-Origin-Resource-Policy"), "same-origin");
  }
});

test("bridge registration awaits one exact host and uses only the current DO ID", async () => {
  let installed = false;
  const handler = {};
  const id = "a".repeat(64);
  await installSessionBridge({ id: { toString: () => id },
    exports: { XhsSessionBridge(options) { assert.deepEqual(options, { props: { durableObjectId: id } }); return handler; } },
    container: { async interceptOutboundHttp(host, bridge) { assert.equal(host, SESSION_HOST); assert.equal(bridge, handler); installed = true; } },
  });
  assert.equal(installed, true);
});

test("activity only refreshes running-container idleness, without storage or hard-deadline changes", async () => {
  const calls = [];
  const f = storageFixture();
  const container = {
    running: false,
    start() { throw new Error("Activity must never start a container"); },
    async setInactivityTimeout(ms) { calls.push(ms); },
  };
  const trial = new Trial({ storage: f.storage, container }, {});
  const bridge = new Bridge({ props: { durableObjectId: "a".repeat(64) } }, { XHS_TRIAL: {
    idFromString(id) { return id; }, get() { return trial; },
  } });
  const ping = () => request("POST", undefined, {}, ACTIVITY_URL);
  assert.equal((await bridge.fetch(ping())).status, 204);
  assert.deepEqual(calls, []);
  const emptyStream = new ReadableStream({ start(controller) { controller.close(); } });
  assert.equal((await bridge.fetch(new Request(ACTIVITY_URL, { method: "POST", body: emptyStream, duplex: "half",
    headers: { "X-XHS-Session-Client": "1", "Content-Length": "0" } }))).status, 204);
  container.running = true;
  assert.equal((await bridge.fetch(ping())).status, 204);
  assert.deepEqual(calls, [60_000]);
  assert.equal(f.transactions, 0);
  assert.equal(f.records().size, 0);
  for (const req of [
    request("GET", undefined, {}, ACTIVITY_URL),
    request("POST", "{}", {}, ACTIVITY_URL),
    request("POST", undefined, { Origin: "https://evil.example" }, ACTIVITY_URL),
    request("POST", undefined, { "Sec-Fetch-Site": "cross-site" }, ACTIVITY_URL),
    request("POST", undefined, { "X-XHS-Session-Client": "" }, ACTIVITY_URL),
    request("POST", undefined, {}, `${ACTIVITY_URL}?object=other`),
  ]) assert.notEqual((await bridge.fetch(req)).status, 204);
  assert.deepEqual(calls, [60_000]);
});

test("actual Worker and DO public fetch remain 404 and expose no cookie state", async () => {
  // 只替换平台基类，直接执行实际 Worker 导出，不使用外部服务。
  const hooks = registerHooks({ resolve(specifier, context, nextResolve) {
    if (specifier === "cloudflare:workers") return { url: "data:text/javascript,export class DurableObject {}; export class WorkerEntrypoint {}", shortCircuit: true };
    return nextResolve(specifier, context);
  } });
  try {
    const { default: worker } = await import("../src/index.mjs");
    const env = { XHS_TRIAL: { get() { throw new Error("Must not access private binding"); } } };
    const f = storageFixture();
    const trial = new Trial({ storage: f.storage }, {});
    for (const req of [request(), put(), remove(0), request("POST", undefined, {}, ACTIVITY_URL), new Request("https://public.example/v1/session")]) {
      assert.equal((await worker.fetch(req, env)).status, 404);
      assert.equal((await trial.fetch(req)).status, 404);
    }
    assert.equal(f.transactions, 0);
    const config = JSON.parse(await readFile(new URL("../wrangler.jsonc", import.meta.url), "utf8"));
    assert.deepEqual(config.routes, []);
    assert.deepEqual(config.triggers.crons, []);
    assert.equal(config.vars.TRIAL_APPROVED, "false");
    assert.equal(config.workers_dev, false);
    assert.equal(config.preview_urls, false);
    assert.equal(config.exports.XhsTrial.storage, "sqlite");
    assert.equal(config.services, undefined);
  } finally { hooks.deregister(); }
});
