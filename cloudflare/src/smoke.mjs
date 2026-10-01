export const MAX_RUNTIME_MS = 20 * 60 * 1000;
export const BROWSER_VERSION = "148.0.7778.215";

// 只验证本机页面，不访问小红书、不调用任何业务工具。
export const BROWSER_COMMAND = [
  "/usr/bin/timeout", "--signal=TERM", "--kill-after=5s", "45s",
  `/app/cache/xiaohongshu-mcp/browser/${BROWSER_VERSION}/chrome`,
  "--headless=new", "--no-sandbox", "--disable-dev-shm-usage",
  "--disable-gpu", "--no-first-run", "--no-default-browser-check",
  "--user-data-dir=/tmp/xhs-trial-profile", "--dump-dom", "about:blank",
];

export function trialGate(env, now) {
  const expiresAt = Date.parse(env.TRIAL_EXPIRES_AT ?? "");
  return env.TRIAL_APPROVED === "true" && Number.isFinite(expiresAt) && now < expiresAt;
}

export async function readLimited(stream, maxBytes = 256 * 1024) {
  if (!stream) return "";
  const reader = stream.getReader();
  const decoder = new TextDecoder();
  let size = 0;
  let value = "";
  try {
    while (true) {
      const part = await reader.read();
      if (part.done) break;
      size += part.value.byteLength;
      if (size > maxBytes) {
        await reader.cancel();
        throw new Error("Response exceeded trial output limit");
      }
      value += decoder.decode(part.value, { stream: true });
    }
    return value + decoder.decode();
  } finally {
    reader.releaseLock();
  }
}

async function jsonResponse(response) {
  const body = await readLimited(response.body);
  if (!response.ok) throw new Error(`HTTP ${response.status}`);
  return JSON.parse(body);
}

export async function runSmokeTest(container, sourceCommit, options = {}) {
  const now = options.now ?? Date.now;
  const wait = options.wait ?? ((ms) => new Promise(resolve => setTimeout(resolve, ms)));
  const port = container.getTcpPort(18060);
  const readinessDeadline = now() + 120_000;
  let health;
  let lastError;
  while (now() < readinessDeadline) {
    try {
      health = await jsonResponse(await port.fetch("http://container/health", {
        signal: AbortSignal.timeout(5000),
      }));
      if (health?.data?.status !== "healthy") throw new Error("Health response mismatch");
      break;
    } catch (error) {
      lastError = error;
      await wait(500);
    }
  }
  if (!health || health?.data?.status !== "healthy") throw lastError ?? new Error("Readiness timeout");
  if (health.data.version !== sourceCommit) throw new Error("Image source version mismatch");

  async function rpc(id, method, params, protocolVersion) {
    const headers = { "Content-Type": "application/json", Accept: "application/json, text/event-stream" };
    if (protocolVersion) headers["MCP-Protocol-Version"] = protocolVersion;
    const response = await port.fetch("http://container/mcp", {
      method: "POST", headers,
      body: JSON.stringify({ jsonrpc: "2.0", id, method, ...(params ? { params } : {}) }),
      signal: AbortSignal.timeout(15_000),
    });
    const message = await jsonResponse(response);
    if (message.error || message.id !== id || !message.result) throw new Error(`MCP ${method} failed`);
    return message.result;
  }

  const initialized = await rpc(1, "initialize", {
    protocolVersion: "2025-03-26",
    capabilities: {},
    clientInfo: { name: "private-cloudflare-smoke-test", version: "1.0.0" },
  });
  if (!initialized.serverInfo?.name || !initialized.protocolVersion) throw new Error("MCP initialize response invalid");
  const listed = await rpc(2, "tools/list", undefined, initialized.protocolVersion);
  if (!Array.isArray(listed.tools) || listed.tools.length === 0) throw new Error("MCP returned no tools");

  const process = await container.exec(BROWSER_COMMAND, {
    stdout: "pipe", stderr: "ignore", cwd: "/app",
    env: { HOME: "/tmp", XDG_CACHE_HOME: "/app/cache", XDG_CONFIG_HOME: "/tmp/xhs-trial-config" },
  });
  const [stdout, exitCode] = await Promise.all([readLimited(process.stdout, 64 * 1024), process.exitCode]);
  if (exitCode !== 0 || !stdout.includes("<html")) throw new Error(`Browser smoke test failed (${exitCode})`);
  return {
    health: "passed", mcpInitialize: "passed", mcpToolsList: "passed",
    browserAboutBlank: "passed", toolCount: listed.tools.length,
    protocolVersion: initialized.protocolVersion, sourceCommit,
  };
}
