import { DurableObject, WorkerEntrypoint } from "cloudflare:workers";
import { createTrialClass } from "./trial.mjs";
import { createSessionBridgeClass } from "./session.mjs";

export class XhsTrial extends createTrialClass(DurableObject) {}
export class XhsSessionBridge extends createSessionBridgeClass(WorkerEntrypoint) {}

export default {
  async fetch() {
    return new Response("Not found", { status: 404 });
  },
  async scheduled(_controller, env) {
    const name = env.TRIAL_MODE === "persistence" ? "session-persistence-20261001" : "single-private-trial";
    const result = await env.XHS_TRIAL.getByName(name).run();
    console.log(JSON.stringify({ event: "xhs-container-trial-status", status: result.status }));
  },
};
