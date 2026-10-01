// 编译 cookies 测试二进制后传入路径；仅使用本机假 cookie，不触网或启动容器。
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { spawn } from "node:child_process";
import { once } from "node:events";
import { handleSessionRequest, SESSION_URL } from "../src/session.mjs";

const binary = process.argv[2];
if (!binary) throw new Error("Pass a compiled cookies test binary");
let data = new Map();
let tail = Promise.resolve();
const storage = {
  transaction(callback) {
    const next = tail.then(async () => {
      const pending = structuredClone(data);
      const result = await callback({ get: async key => pending.get(key), put: async (key, value) => pending.set(key, structuredClone(value)) });
      data = pending;
      return result;
    });
    tail = next.catch(() => {});
    return next;
  },
};
const server = createServer(async (req, res) => {
  try {
    const chunks = [];
    for await (const chunk of req) chunks.push(chunk);
    const body = Buffer.concat(chunks);
    const request = new Request(SESSION_URL, { method: req.method, headers: req.headers, ...(body.length ? { body } : {}) });
    const response = await handleSessionRequest(storage, request);
    res.writeHead(response.status, Object.fromEntries(response.headers));
    res.end(Buffer.from(await response.arrayBuffer()));
  } catch { res.writeHead(500); res.end("test adapter failed"); }
});
server.listen(0, "127.0.0.1");
await once(server, "listening");
try {
  for (const mode of ["write", "restore", "logout", "empty"]) {
    const child = spawn(binary, ["-test.run=^TestRemoteSessionProcessHelper$", "-test.count=1"], {
      env: { ...process.env, XHS_TEST_SESSION_CHILD: mode, XHS_TEST_SESSION_ENDPOINT: `http://127.0.0.1:${server.address().port}` },
      stdio: ["ignore", "pipe", "pipe"],
    });
    let output = "";
    child.stdout.on("data", chunk => output += chunk);
    child.stderr.on("data", chunk => output += chunk);
    const [code] = await once(child, "exit");
    assert.equal(code, 0, `Go/JS ${mode} contract failed: ${output}`);
    console.log(`Go/JS fresh-process ${mode}: passed`);
  }
} finally { server.closeAllConnections(); await new Promise(resolve => server.close(resolve)); }
