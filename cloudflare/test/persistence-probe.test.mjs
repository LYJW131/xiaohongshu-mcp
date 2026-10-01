import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { runPersistencePhases } from "../src/persistence-probe.mjs";

const COMMIT = "a".repeat(40);
const required = {
  write: ["initialStateEmpty", "syntheticStateSaved", "ephemeralMarkerCreated"],
  restore: ["ephemeralMarkerLost", "syntheticStateRestored", "syntheticStateRefreshed", "syntheticStateDeleted", "staleWriteRejected", "deletedStateEmpty", "ephemeralMarkerCreated"],
  empty: ["ephemeralMarkerLost", "deletedStateStayedEmpty"],
};

function fixture() {
  const calls = [];
  let now = 1_000_000;
  let phase;
  let adjust = health => health;
  const ctx = {
    id: { toString: () => "b".repeat(64) },
    exports: { XhsSessionBridge(options) { calls.push(["bridge", options]); return { bridge: true }; } },
    container: {
      running: false,
      async interceptOutboundHttp(host, handler) { calls.push(["intercept", host, handler]); },
      start(options) {
        assert.equal(this.running, false);
        phase = options.env.XHS_SESSION_PROBE_PHASE;
        calls.push(["start", options]); this.running = true;
      },
      async setInactivityTimeout(ms) { calls.push(["idle", ms]); },
      async destroy(reason) { calls.push(["destroy", reason]); this.running = false; },
      getTcpPort(port) {
        assert.equal(port, 18060);
        return { async fetch(url, options) {
          calls.push(["fetch", url]);
          assert.equal(url, "http://container/health");
          assert.ok(options.signal instanceof AbortSignal);
          return Response.json(adjust({ phase, version: COMMIT, result: "passed", markerLost: phase !== "write",
            processId: 1, checks: Object.fromEntries(required[phase].map(key => [key, true])) }));
        } };
      },
    },
  };
  const options = { sourceCommit: COMMIT, now: () => now, wait: async ms => { now += ms; } };
  return { ctx, calls, options, deadline: now + 20 * 60_000,
    advance(ms) { now += ms; }, adjust(fn) { adjust = fn; } };
}

test("three startups reinstall the bridge, forbid egress, and destroy before restart", async () => {
  const f = fixture();
  const result = await runPersistencePhases(f.ctx, f.deadline, f.options);
  assert.equal(result.persistence, "passed");
  assert.deepEqual(result.phases.map(x => x.phase), ["write", "restore", "empty"]);
  assert.deepEqual(result.phases.map(x => x.markerLost), [false, true, true]);
  assert.deepEqual(f.calls.filter(x => ["bridge", "intercept", "start", "destroy"].includes(x[0])).map(x => x[0]),
    ["bridge", "intercept", "start", "destroy", "bridge", "intercept", "start", "destroy", "bridge", "intercept", "start", "destroy"]);
  for (const [, options] of f.calls.filter(x => x[0] === "start")) {
    assert.equal(options.enableInternet, false);
    assert.equal(options.env.XHS_SESSION_STORE, "cloudflare");
    assert.equal(options.entrypoint.at(-1), "./sessionprobe");
    assert.equal(options.entrypoint[0], "/usr/bin/timeout");
    assert.ok(Number.parseInt(options.entrypoint[3]) <= 45);
  }
  for (const [, options] of f.calls.filter(x => x[0] === "bridge")) {
    assert.equal(options.props.durableObjectId, "b".repeat(64));
  }
  assert.equal(f.ctx.container.running, false);
});

test("expired deadline cannot install or start anything", async () => {
  const f = fixture(); f.advance(20 * 60_000);
  await assert.rejects(runPersistencePhases(f.ctx, f.deadline, f.options), /approved time/);
  assert.equal(f.calls.length, 0);
});

test("expiry during bridge install cannot start the container", async () => {
  const f = fixture();
  f.ctx.container.interceptOutboundHttp = async () => f.advance(20 * 60_000);
  await assert.rejects(runPersistencePhases(f.ctx, f.deadline, f.options), /approved time/);
  assert.equal(f.calls.filter(x => x[0] === "start").length, 0);
  assert.equal(f.calls.filter(x => x[0] === "destroy").length, 1);
});

test("phase readiness timeout stops and never advances to another startup", async () => {
  const f = fixture(); f.adjust(health => ({ ...health, result: "running" }));
  await assert.rejects(runPersistencePhases(f.ctx, f.deadline, f.options), /deadline reached/);
  assert.equal(f.calls.filter(x => x[0] === "start").length, 1);
  assert.equal(f.calls.filter(x => x[0] === "destroy").length, 1);
  assert.equal(f.options.now(), 1_045_000);
});

test("deadline expiring after destroy prevents next start", async () => {
  const f = fixture();
  f.ctx.container.destroy = async () => { f.calls.push(["destroy"]); f.ctx.container.running = false; f.advance(20 * 60_000); };
  await assert.rejects(runPersistencePhases(f.ctx, f.deadline, f.options), /approved time/);
  assert.equal(f.calls.filter(x => x[0] === "start").length, 1);
});

test("image, phase, marker, check, and result mismatches fail closed", async () => {
  for (const mutate of [health => ({ ...health, version: "wrong" }), health => ({ ...health, phase: "wrong" }),
    health => ({ ...health, markerLost: true }), health => ({ ...health, checks: {} }), health => ({ ...health, result: "failed", error: "private-cookie-value" })]) {
    const f = fixture(); f.adjust(mutate);
    await assert.rejects(runPersistencePhases(f.ctx, f.deadline, f.options), error => !error.message.includes("private-cookie-value"));
    assert.equal(f.calls.filter(x => x[0] === "start").length, 1);
    assert.equal(f.calls.filter(x => x[0] === "destroy").length, 1);
  }
});

test("cleanup failure prevents restarts and never reports success", async () => {
  const f = fixture();
  f.ctx.container.destroy = async () => { throw new Error("private-cookie-value"); };
  await assert.rejects(runPersistencePhases(f.ctx, f.deadline, f.options), /cleanup required/);
  assert.equal(f.calls.filter(x => x[0] === "start").length, 1);
});

test("bridge failure cannot start and backend errors are sanitized", async () => {
  const f = fixture();
  f.ctx.container.interceptOutboundHttp = async () => { throw new Error("private-cookie-value"); };
  await assert.rejects(runPersistencePhases(f.ctx, f.deadline, f.options), /write unavailable/);
  assert.equal(f.calls.filter(x => x[0] === "start").length, 0);
});

test("only fixed check booleans are copied from health", async () => {
  const f = fixture(); f.adjust(health => ({ ...health, cookies: "private-cookie-value", checks: { ...health.checks, privateData: "private-cookie-value" } }));
  const result = await runPersistencePhases(f.ctx, f.deadline, f.options);
  assert.equal(JSON.stringify(result).includes("private-cookie-value"), false);
});

test("running containers are never reused", async () => {
  const f = fixture(); f.ctx.container.running = true;
  await assert.rejects(runPersistencePhases(f.ctx, f.deadline, f.options), /configuration invalid/);
  assert.equal(f.calls.length, 0);
});

test("image builds and copies a source-versioned standalone probe", async () => {
  const dockerfile = await readFile(new URL("../Dockerfile", import.meta.url), "utf8");
  assert.match(dockerfile, /go build -ldflags="-s -w -X main.version=\$\{VERSION\}" -o \/out\/sessionprobe \.\/cmd\/sessionprobe/);
  assert.match(dockerfile, /COPY --from=builder \/out\/sessionprobe \./);
});
