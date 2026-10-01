export const SESSION_HOST = "xhs-session.internal";
export const SESSION_URL = `http://${SESSION_HOST}/v1/session`;
export const ACTIVITY_URL = `http://${SESSION_HOST}/v1/activity`;
export const SESSION_KEY = "private-cookie-session-v1";
export const MAX_SESSION_BYTES = 64 * 1024;
export const MAX_REQUEST_BYTES = MAX_SESSION_BYTES + 1024;

const encoder = new TextEncoder();
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;
const RFC3339 = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/;

class SessionError extends Error {
  constructor(status, message) { super(message); this.status = status; }
}

function json(value, status = 200) {
  return Response.json(value, { status, headers: {
    "Cache-Control": "no-store",
    "X-Content-Type-Options": "nosniff",
    "Cross-Origin-Resource-Policy": "same-origin",
  } });
}

function object(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function exactKeys(value, keys) {
  return object(value) && Object.keys(value).length === keys.length && keys.every(key => Object.hasOwn(value, key));
}

function invalid() { throw new SessionError(400, "invalid session request"); }

function validTimestamp(value) {
  if (typeof value !== "string" || !RFC3339.test(value)) return false;
  const year = Number(value.slice(0, 4));
  const month = Number(value.slice(5, 7));
  const day = Number(value.slice(8, 10));
  const leap = year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0);
  const days = [31, leap ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];
  if (month < 1 || month > 12 || day < 1 || day > days[month - 1] ||
      Number(value.slice(11, 13)) > 23 || Number(value.slice(14, 16)) > 59 || Number(value.slice(17, 19)) > 59) return false;
  if (!value.endsWith("Z") && (Number(value.slice(-5, -3)) > 23 || Number(value.slice(-2)) > 59)) return false;
  return Number.isFinite(Date.parse(value));
}

// 只接受后端固定地址；禁止浏览器页面借虚拟主机读取或覆盖登录态。
function validateBackendRequest(request, url, methods) {
  if (request.url !== url) throw new SessionError(404, "not found");
  if (!methods.includes(request.method)) throw new SessionError(405, "method not allowed");
  if (request.headers.get("X-XHS-Session-Client") !== "1" ||
      request.headers.has("Origin") || request.headers.has("Referer") ||
      [...request.headers.keys()].some(name => name.toLowerCase().startsWith("sec-fetch-"))) {
    throw new SessionError(403, "backend requests only");
  }
  if (request.headers.has("Content-Encoding")) invalid();
  const length = request.headers.get("Content-Length");
  if (length !== null && (!/^\d+$/.test(length) || !Number.isSafeInteger(Number(length)))) invalid();
  if (Number(length) > MAX_REQUEST_BYTES) throw new SessionError(413, "request too large");
  return Number(length);
}

export function validateSessionRequest(request) {
  const length = validateBackendRequest(request, SESSION_URL, ["GET", "PUT", "DELETE"]);
  if (request.method === "GET") {
    if (request.body !== null || length > 0) invalid();
  } else if (request.headers.get("Content-Type")?.split(";", 1)[0].trim().toLowerCase() !== "application/json") {
    throw new SessionError(415, "JSON required");
  }
}

async function validateActivityRequest(request) {
  const length = validateBackendRequest(request, ACTIVITY_URL, ["POST"]);
  if (length > 0) invalid();
  if (!request.body) return;
  const reader = request.body.getReader();
  try {
    while (true) {
      const part = await reader.read();
      if (part.done) return;
      if (part.value.byteLength > 0) {
        await reader.cancel();
        invalid();
      }
    }
  } finally {
    reader.releaseLock();
  }
}

async function readBody(request) {
  if (!request.body) invalid();
  const reader = request.body.getReader();
  const decoder = new TextDecoder("utf-8", { fatal: true });
  let size = 0;
  let text = "";
  try {
    while (true) {
      const part = await reader.read();
      if (part.done) break;
      size += part.value.byteLength;
      if (size > MAX_REQUEST_BYTES) {
        await reader.cancel();
        throw new SessionError(413, "request too large");
      }
      text += decoder.decode(part.value, { stream: true });
    }
    text += decoder.decode();
    return JSON.parse(text);
  } catch (error) {
    if (error instanceof SessionError) throw error;
    invalid();
  } finally {
    reader.releaseLock();
  }
}

// 规范化字段顺序，重试无需依赖 JSON 序列化器的键顺序。
function canonical(value, depth = 0) {
  if (depth > 16) invalid();
  if (Array.isArray(value)) return `[${value.map(item => canonical(item, depth + 1)).join(",")}]`;
  if (object(value)) return `{${Object.keys(value).sort().map(key => `${JSON.stringify(key)}:${canonical(value[key], depth + 1)}`).join(",")}}`;
  if ((typeof value === "number" && !Number.isFinite(value)) || value === undefined) invalid();
  return JSON.stringify(value);
}

function validateSession(session) {
  if (!exactKeys(session, ["version", "seed", "saved_at", "cookies"]) ||
      session.version !== 2 || !Number.isSafeInteger(session.seed) || session.seed < 0 ||
      !validTimestamp(session.saved_at) || !Array.isArray(session.cookies)) invalid();
  for (const cookie of session.cookies) {
    if (!object(cookie) || typeof cookie.name !== "string" || cookie.name.length === 0 ||
        typeof cookie.value !== "string" ||
        (Object.hasOwn(cookie, "domain") && typeof cookie.domain !== "string") ||
        (Object.hasOwn(cookie, "path") && typeof cookie.path !== "string")) invalid();
  }
  const serialized = canonical(session);
  if (encoder.encode(serialized).byteLength > MAX_SESSION_BYTES) throw new SessionError(413, "session too large");
  return session;
}

async function parseMutation(request) {
  const body = await readBody(request);
  const keys = request.method === "PUT" ? ["expected_revision", "operation_id", "session"] : ["expected_revision", "operation_id"];
  if (!exactKeys(body, keys) || !Number.isSafeInteger(body.expected_revision) || body.expected_revision < 0 ||
      typeof body.operation_id !== "string" || !UUID.test(body.operation_id)) invalid();
  const session = request.method === "PUT" ? validateSession(body.session) : null;
  const bytes = encoder.encode(canonical({ method: request.method, expected_revision: body.expected_revision, session }));
  const digest = await crypto.subtle.digest("SHA-256", bytes);
  const fingerprint = Array.from(new Uint8Array(digest), byte => byte.toString(16).padStart(2, "0")).join("");
  return { expectedRevision: body.expected_revision, operationId: body.operation_id.toLowerCase(), session, fingerprint };
}

function snapshot(record) { return { revision: record.revision, session: record.session }; }

function storedRecord(record) {
  if (record === undefined) return { revision: 0, session: null };
  if (!object(record) || !Number.isSafeInteger(record.revision) || record.revision < 1 ||
      !object(record.operation) || !UUID.test(record.operation.id) ||
      !/^[a-f0-9]{64}$/.test(record.operation.fingerprint) ||
      record.operation.expectedRevision !== record.revision - 1) throw new Error("Invalid stored session");
  try {
    if (record.session !== null) validateSession(record.session);
  } catch {
    throw new Error("Invalid stored session");
  }
  return record;
}

export async function handleSessionRequest(storage, request) {
  try {
    validateSessionRequest(request);
    const mutation = request.method === "GET" ? null : await parseMutation(request);
    // SQLite 支撑的异步 KV；事务内仅访问一条记录，不执行外部 I/O。
    const outcome = await storage.transaction(async txn => {
      const record = storedRecord(await txn.get(SESSION_KEY));
      if (!mutation) return { status: 200, body: snapshot(record) };
      if (record.operation?.id === mutation.operationId) {
        if (record.operation.fingerprint === mutation.fingerprint) return { status: 200, body: snapshot(record) };
        return { status: 409, body: { error: "operation conflict", revision: record.revision } };
      }
      if (record.revision !== mutation.expectedRevision) return { status: 409, body: { error: "revision conflict", revision: record.revision } };
      if (record.revision === Number.MAX_SAFE_INTEGER) throw new Error("Revision exhausted");
      const next = {
        revision: record.revision + 1,
        session: mutation.session,
        operation: { id: mutation.operationId, fingerprint: mutation.fingerprint, expectedRevision: mutation.expectedRevision },
      };
      // 删除保留版本墓碑，旧容器无法在删除后重新写回旧 cookies。
      await txn.put(SESSION_KEY, next);
      return { status: 200, body: snapshot(next) };
    });
    return json(outcome.body, outcome.status);
  } catch (error) {
    if (error instanceof SessionError) return json({ error: error.message }, error.status);
    // 存储与 RPC 异常可能带入敏感内容，禁止原样回显或写日志。
    return json({ error: "session store unavailable" }, 503);
  }
}

export function createSessionBridgeClass(Base) {
  return class extends Base {
    async fetch(request) {
      try {
        const activity = request.url === ACTIVITY_URL;
        if (activity) await validateActivityRequest(request);
        else validateSessionRequest(request);
        // props 由本 DO 构造；不接受 URL、请求头或 body 指定的实例。
        const idString = this.ctx.props?.durableObjectId;
        if (typeof idString !== "string" || !/^[a-f0-9]{64}$/.test(idString)) throw new Error("Missing trusted object ID");
        const id = this.env.XHS_TRIAL.idFromString(idString);
        const stub = this.env.XHS_TRIAL.get(id);
        if (activity) return await stub.sessionActivity();
        return await stub.sessionRequest(request);
      } catch (error) {
        if (error instanceof SessionError) return json({ error: error.message }, error.status);
        return json({ error: "session store unavailable" }, 503);
      }
    }
  };
}

export async function installSessionBridge(ctx) {
  const bridge = ctx.exports.XhsSessionBridge({ props: { durableObjectId: ctx.id.toString() } });
  await ctx.container.interceptOutboundHttp(SESSION_HOST, bridge);
}
