// The console API's route table: every request the console makes goes through a row of it (api.js
// refuses anything else), and no other module names a /v1/ path. The rows are the operations of the
// gate's console API (internal/console), whose paths and JSON shapes follow Kontra Cloud's control
// plane so the same files can serve both. A service is addressed by its name alone: the gate is
// single-tenant.

export const ROUTES = Object.freeze({
  info: ["GET", "/v1/info"],
  tasks: ["GET", "/v1/tasks"],
  conversations: ["GET", "/v1/conversations"],
  conversation: ["GET", "/v1/conversations/{service}/{caller}/{context}"],
  task: ["GET", "/v1/tasks/{task_id}"],
  task_events: ["GET", "/v1/tasks/{task_id}/events"],
  task_file: ["GET", "/v1/tasks/{task_id}/files/{ref}"],
  start_task: ["POST", "/v1/tasks"],
  start_form: ["GET", "/v1/services/{name}/form"],
  task_message: ["POST", "/v1/tasks/{task_id}/messages"],
  cancel_task: ["POST", "/v1/tasks/{task_id}/cancel"],
  continue_task: ["POST", "/v1/tasks/{task_id}/continue"],
  services: ["GET", "/v1/services"],
  service: ["GET", "/v1/services/{name}"],
  tunnels: ["GET", "/v1/tunnels"],
  exchanges: ["GET", "/v1/exchanges"],
  calls: ["GET", "/v1/calls"],
  contexts: ["GET", "/v1/contexts"],
  context: ["GET", "/v1/contexts/{service}/{caller}/{context}"],
  context_timeline: ["GET", "/v1/contexts/{service}/{caller}/{context}/timeline"],
  task_timeline: ["GET", "/v1/tasks/{task_id}/timeline"],
  exchange: ["GET", "/v1/exchanges/{id}"],
  service_cards: ["GET", "/v1/services/{name}/cards"],
});

/** The A2A routes the playground calls (#/play/<service>, D360): a service's own A2A route on the gate's
 * origin, as any A2A client calls it, so the gate admits, relays and records the call as a client's. JSON-RPC
 * has one path for every method; HTTP+JSON has A2A 1.0's per method (specification/a2a.proto's
 * google.api.http rules), `{id}` and `{task_id}` filled from the body. Nothing else of the page reaches
 * /agents/. */
export const A2A_JSONRPC = "/agents/{service}";
export const A2A_ROUTES = Object.freeze({
  SendMessage: ["POST", "/agents/{service}/message:send"],
  SendStreamingMessage: ["POST", "/agents/{service}/message:stream"],
  GetTask: ["GET", "/agents/{service}/tasks/{id}"],
  ListTasks: ["GET", "/agents/{service}/tasks"],
  CancelTask: ["POST", "/agents/{service}/tasks/{id}:cancel"],
  SubscribeToTask: ["GET", "/agents/{service}/tasks/{id}:subscribe"],
  CreateTaskPushNotificationConfig: ["POST", "/agents/{service}/tasks/{task_id}/pushNotificationConfigs"],
  GetTaskPushNotificationConfig: ["GET", "/agents/{service}/tasks/{task_id}/pushNotificationConfigs/{id}"],
  ListTaskPushNotificationConfigs: ["GET", "/agents/{service}/tasks/{task_id}/pushNotificationConfigs"],
  DeleteTaskPushNotificationConfig: ["DELETE", "/agents/{service}/tasks/{task_id}/pushNotificationConfigs/{id}"],
  GetExtendedAgentCard: ["GET", "/agents/{service}/extendedAgentCard"],
});

/** The A2A methods that answer with a stream (SSE). */
export const A2A_STREAMS = Object.freeze(["SendStreamingMessage", "SubscribeToTask"]);

/** The path of A2A route `method` (HTTP+JSON) or the JSON-RPC path, its parameters filled and escaped. */
export function a2aPath(binding, method, params = {}) {
  const tpl = binding === "jsonrpc" ? A2A_JSONRPC : (A2A_ROUTES[method] || [])[1];
  if (!tpl) throw new Error(`no A2A route ${method}`);
  return tpl.replace(/\{([a-z_]+)\}/g, (_, key) => {
    if (!Object.hasOwn(params, key) || typeof params[key] !== "string" || !params[key]) throw new Error(`${method} needs ${key}`);
    return segment(params[key]);
  });
}

/** One URL path segment, escaped: "/", "?", "#" and a bare "." or ".." can never change which
 * route a value reaches. */
export function segment(value) {
  if (typeof value !== "string" || value === "") throw new TypeError("path parameters must be non-empty strings");
  if (value === "." || value === "..") return value.replaceAll(".", "%2E");
  return encodeURIComponent(value);
}

/** The path of route `name` with its parameters filled in (every one must be given). */
export function routePath(name, params = {}) {
  const row = ROUTES[name];
  if (!row) throw new Error(`no route ${name}`);
  return row[1].replace(/\{([a-z_0-9]+)\}/g, (_, key) => {
    if (!Object.hasOwn(params, key)) throw new Error(`route ${name} needs ${key}`);
    return segment(params[key]);
  });
}
