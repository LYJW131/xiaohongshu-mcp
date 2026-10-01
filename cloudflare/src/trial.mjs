import { MAX_RUNTIME_MS, runSmokeTest, trialGate } from "./smoke.mjs";
import { handleSessionRequest, installSessionBridge } from "./session.mjs";
import { runPersistencePhases } from "./persistence-probe.mjs";

// 依赖注入仅用于离线单测；生产基类为 Cloudflare DurableObject。
export function createTrialClass(Base) {
  return class extends Base {
    currentRun;

    constructor(ctx, env) {
      super(ctx, env);
      if (ctx.container?.running) {
        void ctx.blockConcurrencyWhile(() => ctx.container.setInactivityTimeout(60_000));
      }
    }

    async fetch() {
      return new Response("Not found", { status: 404 });
    }

    sessionRequest(request) {
      return handleSessionRequest(this.ctx.storage, request);
    }

    async sessionActivity() {
      // 只保持现有任务的空闲窗口，不启动容器或修改硬截止时间。
      if (this.ctx.container?.running) await this.ctx.container.setInactivityTimeout(60_000);
      return new Response(null, { status: 204, headers: { "Cache-Control": "no-store" } });
    }

    run() {
      this.currentRun ??= this.runOnce().finally(() => { this.currentRun = undefined; });
      return this.currentRun;
    }

    async runOnce() {
      const existing = await this.ctx.storage.get("trial");
      if (existing) return existing; // 崩溃或重复触发也绝不自动再开一台。
      if (!trialGate(this.env, Date.now())) return { status: "not-armed" };
      const container = this.ctx.container;
      if (!container) throw new Error("Missing container binding");
      const startedAt = Date.now();
      const deadline = Math.min(startedAt + MAX_RUNTIME_MS, Date.parse(this.env.TRIAL_EXPIRES_AT));
      const result = { status: "running", startedAt, deadline, sourceCommit: this.env.SOURCE_COMMIT };
      await this.ctx.storage.put("trial", result);
      // alarm + 镜像内 timeout 双保险，不依赖公共 HTTP 请求续命。
      await this.ctx.storage.setAlarm(deadline);
      let hardStop;
      try {
        const remainingMs = deadline - Date.now();
        if (remainingMs <= 6000) throw new Error("Insufficient approved time remaining");
        let operation;
        if (this.env.TRIAL_MODE === "persistence") {
          operation = runPersistencePhases(this.ctx, deadline, { sourceCommit: this.env.SOURCE_COMMIT });
        } else {
          // 拦截规则随容器停止失效，每次获批启动前重新安装。
          await installSessionBridge(this.ctx);
          if (deadline - Date.now() <= 6000) throw new Error("Insufficient approved time remaining");
          const seconds = Math.floor((deadline - Date.now()) / 1000) - 5;
          container.start({
            enableInternet: false,
            entrypoint: ["/usr/bin/timeout", "--signal=TERM", "--kill-after=5s", `${seconds}s`,
              "/usr/bin/tini", "-s", "--", "./app"],
            env: {
              COOKIES_PATH: "/app/data/cookies.json", HOME: "/app/data/home",
              XDG_CONFIG_HOME: "/app/data/config", XDG_CACHE_HOME: "/app/cache",
              XHS_SESSION_STORE: "cloudflare",
            },
          });
          await container.setInactivityTimeout(60_000);
          operation = runSmokeTest(container, this.env.SOURCE_COMMIT);
        }
        const deadlineFailure = new Promise((_, reject) => {
          hardStop = setTimeout(() => reject(new Error("Trial deadline reached")), Math.max(1, deadline - Date.now()));
        });
        const smoke = await Promise.race([operation, deadlineFailure]);
        Object.assign(result, smoke, { status: "passed" });
      } catch (error) {
        result.status = "failed";
        result.error = String(error?.message ?? error).slice(0, 500);
      } finally {
        clearTimeout(hardStop);
        try {
          await container.destroy("Bounded private trial completed");
          result.containerStopped = true;
          await this.ctx.storage.deleteAlarm();
        } catch {
          result.containerStopped = false;
          result.status = "cleanup-required";
        }
        result.finishedAt = Date.now();
        await this.ctx.storage.put("trial", result);
        console.log(JSON.stringify({ event: "xhs-container-trial-result", ...result }));
      }
      return result;
    }

    async alarm() {
      const record = await this.ctx.storage.get("trial");
      if (!record) return;
      await this.ctx.container?.destroy("Trial hard deadline");
      await this.ctx.storage.put("trial", {
        ...record, status: record.status === "running" ? "deadline-stopped" : record.status,
        containerStopped: true, stoppedAt: Date.now(),
      });
    }
  };
}
