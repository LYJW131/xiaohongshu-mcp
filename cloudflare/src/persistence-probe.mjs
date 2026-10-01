import { installSessionBridge } from "./session.mjs";
import { readLimited } from "./smoke.mjs";

const PHASES = ["write", "restore", "empty"];
const PHASE_LIMIT_MS = 45_000;
const STOP_RESERVE_MS = 5_000;
const CHECKS = {
  write: ["initialStateEmpty", "syntheticStateSaved", "ephemeralMarkerCreated"],
  restore: ["ephemeralMarkerLost", "syntheticStateRestored", "syntheticStateRefreshed",
    "syntheticStateDeleted", "staleWriteRejected", "deletedStateEmpty", "ephemeralMarkerCreated"],
  empty: ["ephemeralMarkerLost", "deletedStateStayedEmpty"],
};

class ProbeFailure extends Error {}

function remaining(deadline, now) {
  const ms = deadline - now();
  if (!Number.isFinite(ms) || ms <= STOP_RESERVE_MS + 1_000) {
    throw new ProbeFailure("Insufficient approved time for persistence probe");
  }
  return ms;
}

async function healthRequest(port, timeoutMs) {
  const controller = new AbortController();
  let timer;
  try {
    return await Promise.race([
      (async () => {
        const response = await port.fetch("http://container/health", { signal: controller.signal });
        if (!response.ok) throw new Error("Probe health unavailable");
        return JSON.parse(await readLimited(response.body, 8 * 1024));
      })(),
      new Promise((_, reject) => {
        timer = setTimeout(() => reject(new Error("Probe health timeout")), timeoutMs);
      }),
    ]);
  } finally {
    clearTimeout(timer);
    controller.abort();
  }
}

async function pollPhase(container, phase, sourceCommit, deadline, now, wait) {
  const port = container.getTcpPort(18060);
  while (now() < deadline) {
    let health;
    try {
      health = await healthRequest(port, Math.max(1, Math.min(5_000, deadline - now())));
    } catch {
      // 启动期间的连接失败可重试；不回显 HTTP body 或底层错误。
      const pause = Math.min(250, deadline - now());
      if (pause > 0) await wait(pause);
      continue;
    }
    if (now() >= deadline) break;
    if (health?.phase !== phase || health?.version !== sourceCommit) {
      throw new ProbeFailure("Persistence probe image or phase mismatch");
    }
    if (health.result === "running") {
      await wait(Math.max(1, Math.min(250, deadline - now())));
      continue;
    }
    if (health.result !== "passed" || health.markerLost !== (phase !== "write") ||
        !CHECKS[phase].every(key => health.checks?.[key] === true)) {
      throw new ProbeFailure(`Persistence probe ${phase} failed`);
    }
    // 只保留预定义布尔检查，防止健康响应夹带 cookie 内容。
    return {
      phase, result: "passed", markerLost: health.markerLost,
      checks: Object.fromEntries(CHECKS[phase].map(key => [key, true])),
    };
  }
  throw new ProbeFailure(`Persistence probe ${phase} deadline reached`);
}

// 每个阶段销毁容器后再启动，DO storage 与实例 ID 始终保持不变。
export async function runPersistencePhases(ctx, deadline, options = {}) {
  const now = options.now ?? Date.now;
  const wait = options.wait ?? (ms => new Promise(resolve => setTimeout(resolve, ms)));
  const sourceCommit = options.sourceCommit;
  const container = ctx.container;
  if (!container || container.running || typeof sourceCommit !== "string" || !sourceCommit) {
    throw new ProbeFailure("Persistence probe configuration invalid");
  }
  const phases = [];
  for (const phase of PHASES) {
    remaining(deadline, now);
    try {
      // 拦截器随销毁失效，因此必须先重装再启动，且禁止公网出口。
      await installSessionBridge(ctx);
      remaining(deadline, now);
      const phaseDeadline = Math.min(deadline - STOP_RESERVE_MS, now() + PHASE_LIMIT_MS);
      const seconds = Math.floor((phaseDeadline - now()) / 1000);
      container.start({
        enableInternet: false,
        entrypoint: ["/usr/bin/timeout", "--signal=TERM", "--kill-after=5s", `${seconds}s`,
          "/usr/bin/tini", "-s", "--", "./sessionprobe"],
        env: {
          COOKIES_PATH: "/app/data/cookies.json", HOME: "/app/data/home",
          XDG_CONFIG_HOME: "/app/data/config", XDG_CACHE_HOME: "/app/cache",
          XHS_SESSION_STORE: "cloudflare", XHS_SESSION_PROBE_PHASE: phase,
        },
      });
      await container.setInactivityTimeout(Math.min(PHASE_LIMIT_MS, phaseDeadline - now()));
      phases.push(await pollPhase(container, phase, sourceCommit, phaseDeadline, now, wait));
    } catch (error) {
      if (error instanceof ProbeFailure) throw error;
      throw new ProbeFailure(`Persistence probe ${phase} unavailable`);
    } finally {
      try {
        await container.destroy(`Synthetic persistence probe ${phase} completed`);
      } catch {
        throw new ProbeFailure("Persistence probe cleanup required");
      }
    }
  }
  return { persistence: "passed", phases };
}
