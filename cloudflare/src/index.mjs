import { DurableObject } from "cloudflare:workers";
import { createTrialClass } from "./trial.mjs";

export class XhsTrial extends createTrialClass(DurableObject) {}

export default {
  async fetch() {
    return new Response("Not found", { status: 404 });
  },
  async scheduled(_controller, env) {
    const result = await env.XHS_TRIAL.getByName("single-private-trial").run();
    console.log(JSON.stringify({ event: "xhs-container-trial-status", status: result.status }));
  },
};
