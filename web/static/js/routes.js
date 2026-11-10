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
});

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
