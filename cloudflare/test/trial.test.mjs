import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { createTrialClass } from "../src/trial.mjs";
import { BROWSER_COMMAND, BROWSER_VERSION, MAX_RUNTIME_MS, readLimited, runSmokeTest, trialGate } from "../src/smoke.mjs";

const COMMIT = "a5c8f7799980ba1fdd501999843eb2d17e4c9a9f";
const json = value => Response.json(value);

function fixture({ version = COMMIT, execExit = 0, destroyFails = false, armed = true } = {}) {
  const records = new Map();
  const calls = [];
  const ctx = {
    id: { toString: () => "a".repeat(64) },
    exports: { XhsSessionBridge(options) { calls.push(["bridge", options]); return { bridge: true }; } },
    storage: {
      async get(key) { return structuredClone(records.get(key)); },
      async put(key, value) { records.set(key, structuredClone(value)); },
      async setAlarm(value) { records.set("alarm", value); },
      async deleteAlarm() { records.delete("alarm"); },
    },
    blockConcurrencyWhile(fn) { return fn(); },
    container: {
      running: false,
      async interceptOutboundHttp(host, handler) { calls.push(["intercept", host, handler]); },
      start(options) { calls.push(["start", options]); this.running = true; },
      async setInactivityTimeout(ms) { calls.push(["idle", ms]); },
      async destroy(reason) { calls.push(["destroy", reason]); if (destroyFails) throw new Error("cleanup failed"); this.running = false; },
      getTcpPort(port) {
        assert.equal(port, 18060);
        return { async fetch(url, options = {}) {
          calls.push(["fetch", url, options]);
          if (url.endsWith("/health")) return json({ data: { status: "healthy", version } });
          const rpc = JSON.parse(options.body);
          const result = rpc.method === "initialize"
            ? { protocolVersion: "2025-03-26", serverInfo: { name: "xiaohongshu-mcp" } }
            : { tools: [{ name: "check_login_status" }] };
          return json({ jsonrpc: "2.0", id: rpc.id, result });
        } };
      },
      async exec(command, options) {
        calls.push(["exec", command, options]);
        return { stdout: new Response("<html><head></head><body></body></html>").body, exitCode: Promise.resolve(execExit) };
      },
    },
  };
  const env = { TRIAL_APPROVED: armed ? "true" : "false", TRIAL_EXPIRES_AT: new Date(Date.now() + 40 * 60_000).toISOString(), SOURCE_COMMIT: COMMIT };
  class Base { constructor(ctx, env) { this.ctx = ctx; this.env = env; } }
  const Trial = createTrialClass(Base);
  return { trial: new Trial(ctx, env), ctx, env, calls, records };
}

test("trial fails closed without approval and a future expiry", () => {
  const future = "2026-10-01T15:00:00Z";
  const now = Date.parse("2026-10-01T14:00:00Z");
  assert.equal(trialGate({}, now), false);
  assert.equal(trialGate({ TRIAL_APPROVED: "true" }, now), false);
  assert.equal(trialGate({ TRIAL_APPROVED: "false", TRIAL_EXPIRES_AT: future }, now), false);
  assert.equal(trialGate({ TRIAL_APPROVED: "true", TRIAL_EXPIRES_AT: "bad" }, now), false);
  assert.equal(trialGate({ TRIAL_APPROVED: "true", TRIAL_EXPIRES_AT: future }, now), true);
  assert.equal(trialGate({ TRIAL_APPROVED: "true", TRIAL_EXPIRES_AT: future }, Date.parse(future)), false);
});

test("disarmed trial never starts a container", async () => {
  const f = fixture({ armed: false });
  assert.equal((await f.trial.run()).status, "not-armed");
  assert.equal(f.calls.filter(call => call[0] === "start").length, 0);
  assert.equal(f.calls.filter(call => call[0] === "destroy").length, 1);
});

test("disarmed invocation releases a preallocated instance without starting a trial", async () => {
  const f = fixture({ armed: false });
  f.ctx.container.running = true;
  assert.equal((await f.trial.run()).status, "not-armed");
  assert.equal(f.ctx.container.running, false);
  assert.equal(f.calls.filter(call => call[0] === "start").length, 0);
  assert.equal(f.records.has("trial"), false);
});

test("disarmed cleanup failure remains visible without starting", async () => {
  const f = fixture({ armed: false, destroyFails: true });
  assert.equal((await f.trial.run()).status, "cleanup-required");
  assert.equal(f.calls.filter(call => call[0] === "start").length, 0);
});

test("expiry during storage setup cannot start a container", async () => {
  const f = fixture();
  const realNow = Date.now;
  const base = realNow();
  let clock = base;
  Date.now = () => clock;
  f.env.TRIAL_EXPIRES_AT = new Date(base + 10_000).toISOString();
  f.ctx.storage.setAlarm = async value => {
    f.records.set("alarm", value);
    clock = base + 11_000;
  };
  try {
    const result = await f.trial.run();
    assert.equal(result.status, "failed");
    assert.match(result.error, /Insufficient approved time/);
    assert.equal(f.calls.filter(x => x[0] === "start").length, 0);
  } finally {
    Date.now = realNow;
  }
});

test("interceptor failure prevents starting a container", async () => {
  const f = fixture();
  f.ctx.container.interceptOutboundHttp = async () => { throw new Error("intercept unavailable"); };
  assert.equal((await f.trial.run()).status, "failed");
  assert.equal(f.calls.some(call => call[0] === "start"), false);
});

test("expiry while installing the interceptor cannot start a container", async () => {
  const f = fixture();
  const realNow = Date.now;
  const base = realNow();
  let now = base;
  Date.now = () => now;
  f.env.TRIAL_EXPIRES_AT = new Date(base + 10_000).toISOString();
  f.ctx.container.interceptOutboundHttp = async () => { now = base + 11_000; };
  try {
    assert.equal((await f.trial.run()).status, "failed");
    assert.equal(f.calls.some(call => call[0] === "start"), false);
  } finally { Date.now = realNow; }
});

test("private smoke passes, stops, and only lists MCP tools", async () => {
  const f = fixture();
  const result = await f.trial.run();
  assert.equal(result.status, "passed");
  assert.equal(result.containerStopped, true);
  assert.equal(f.ctx.container.running, false);
  assert.equal(f.records.has("alarm"), false);
  assert.equal(result.deadline - result.startedAt, MAX_RUNTIME_MS);
  const start = f.calls.find(x => x[0] === "start")[1];
  assert.equal(start.enableInternet, false);
  assert.equal(start.env.AUTH_TOKEN, undefined);
  assert.equal(start.env.COOKIES_PATH, "/app/data/cookies.json");
  assert.equal(start.env.XHS_SESSION_STORE, "cloudflare");
  assert.deepEqual(f.calls.find(x => x[0] === "bridge")[1], { props: { durableObjectId: "a".repeat(64) } });
  assert.equal(f.calls.find(x => x[0] === "intercept")[1], "xhs-session.internal");
  assert.ok(f.calls.findIndex(x => x[0] === "intercept") < f.calls.findIndex(x => x[0] === "start"));
  assert.equal(start.entrypoint[0], "/usr/bin/timeout");
  const methods = f.calls.filter(x => x[0] === "fetch" && x[1].endsWith("/mcp")).map(x => JSON.parse(x[2].body).method);
  assert.deepEqual(methods, ["initialize", "tools/list"]);
  assert.equal(f.calls.find(x => x[0] === "exec")[1].at(-1), "about:blank");
});

test("sequential cron calls never restart a completed container", async () => {
  const f = fixture();
  await f.trial.run();
  await f.trial.run();
  assert.equal(f.calls.filter(x => x[0] === "start").length, 1);
});

test("concurrent cron calls share one run", async () => {
  const f = fixture();
  const [a, b] = await Promise.all([f.trial.run(), f.trial.run()]);
  assert.deepEqual(a, b);
  assert.equal(f.calls.filter(x => x[0] === "start").length, 1);
});

test("persisted running marker prevents restart after worker interruption", async () => {
  const f = fixture();
  f.records.set("trial", { status: "running" });
  assert.equal((await f.trial.run()).status, "running");
  assert.equal(f.calls.length, 0);
});

test("wrong source image fails before browser execution and still stops", async () => {
  const f = fixture({ version: "unverified-image" });
  const result = await f.trial.run();
  assert.equal(result.status, "failed");
  assert.match(result.error, /version mismatch/);
  assert.equal(result.containerStopped, true);
  assert.equal(f.calls.some(x => x[0] === "exec"), false);
});

test("browser launch failure still stops and records failure", async () => {
  const f = fixture({ execExit: 1 });
  const result = await f.trial.run();
  assert.equal(result.status, "failed");
  assert.equal(result.containerStopped, true);
});

test("failed cleanup retains hard-deadline alarm", async () => {
  const f = fixture({ destroyFails: true });
  const result = await f.trial.run();
  assert.equal(result.status, "cleanup-required");
  assert.equal(result.containerStopped, false);
  assert.equal(f.records.has("alarm"), true);
});

test("deadline alarm destroys an interrupted container", async () => {
  const f = fixture();
  f.records.set("trial", { status: "running" });
  await f.trial.alarm();
  assert.equal(f.records.get("trial").status, "deadline-stopped");
  assert.equal(f.calls.filter(x => x[0] === "destroy").length, 1);
});

test("output reader rejects oversized content", async () => {
  await assert.rejects(readLimited(new Response("0123456789").body, 5), /output limit/);
  assert.equal(await readLimited(new Response("小红书").body), "小红书");
});

test("checked-in configuration has no public routes, credentials, or schedule", async () => {
  const config = JSON.parse(await readFile(new URL("../wrangler.jsonc", import.meta.url), "utf8"));
  assert.equal(config.workers_dev, false);
  assert.equal(config.preview_urls, false);
  assert.deepEqual(config.routes, []);
  assert.deepEqual(config.triggers.crons, []);
  assert.equal(config.containers[0].max_instances, 1);
  assert.equal(config.containers[0].instance_type, "standard-1");
  assert.equal(config.containers[0].image, "./Dockerfile");
  assert.equal(config.containers[0].image_build_context, "..");
  assert.equal(config.containers[0].ssh.enabled, false);
  assert.equal(config.vars.TRIAL_APPROVED, "false");
  assert.match(config.vars.SOURCE_COMMIT, /^[a-f0-9]{40}$/);
  assert.equal(config.containers[0].image_vars.VERSION, config.vars.SOURCE_COMMIT);
  assert.equal(config.vars.TRIAL_MODE, "persistence");
});

test("browser path matches exact upstream version", async () => {
  const upstreamVersion = (await readFile(new URL("../../browser/browser_version.txt", import.meta.url), "utf8")).trim();
  assert.equal(BROWSER_VERSION, upstreamVersion);
  assert.ok(BROWSER_COMMAND.includes("--no-sandbox"));
  assert.ok(BROWSER_COMMAND.includes("--disable-dev-shm-usage"));
});

test("Cloudflare Dockerfile preserves application build and browser source", async () => {
  const file = await readFile(new URL("../Dockerfile", import.meta.url), "utf8");
  assert.equal(file.includes("mirrors.aliyun.com"), false);
  assert.ok(file.includes("Acquire::http::Timeout=30"));
  assert.ok(file.includes("CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build"));
  assert.ok(file.includes("https://cdn.one-world.ai/browsers/${VER}"));
  assert.ok(file.includes("sha256sum -c -"));
});
