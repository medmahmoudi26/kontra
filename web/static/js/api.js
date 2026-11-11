// The console API client: one method per row of ROUTES (routes.js), nothing else. The console is
// served by the gate it talks to, so every request goes to this origin (base ""), carries no
// credential (no cookie, no Authorization header: the local development profile has no login),
// follows no redirect, and a request that changes something is a JSON body
// (`Content-Type: application/json`), which a page on another site cannot send without a preflight
// the API never answers. Errors are the API's error body as a GateError.

import { normalizeForm } from "./agentstack.js";
import { A2A_ROUTES, A2A_STREAMS, ROUTES, a2aPath, routePath } from "./routes.js";

export class GateError extends Error {
  constructor(status, code, message, detail = null) {
    super(message);
    this.status = status;
    this.code = code;
    this.detail = detail;
  }
}

const isObj = (v) => v !== null && typeof v === "object" && !Array.isArray(v);

/** The error body of a refused response as a GateError: `{code, message, detail?}`, or the same
 * object under `error` (`{error: {code, message}}`); anything else is `http_error`. */
export async function errorFrom(resp) {
  let body = null;
  try {
    body = await resp.json();
  } catch {
    body = null;
  }
  const e = isObj(body) && isObj(body.error) ? body.error : body;
  if (isObj(e) && typeof e.code === "string" && typeof e.message === "string") {
    return new GateError(resp.status, e.code, e.message.slice(0, 2000), isObj(e.detail) ? e.detail : null);
  }
  return new GateError(resp.status, "http_error", `HTTP ${resp.status}`);
}

export class Gate {
  constructor({ base = "", fetch: fetchImpl = globalThis.fetch.bind(globalThis) } = {}) {
    this.base = base.replace(/\/$/, "");
    this.fetchImpl = fetchImpl;
  }

  url(name, path, query) {
    const url = this.base + routePath(name, path);
    const q = new URLSearchParams();
    for (const [k, v] of Object.entries(query || {})) {
      if (v !== undefined && v !== null && v !== "") q.set(k, String(v));
    }
    const qs = q.toString();
    return qs ? `${url}?${qs}` : url;
  }

  /** Send route `name`; returns the fetch Response (any status). A POST always has a JSON body
   * (`{}` when there is nothing to say). */
  async send(name, { path, query, body, signal, accept = "application/json" } = {}) {
    const row = ROUTES[name];
    if (!row) throw new Error(`no route ${name}`);
    const [method] = row;
    const headers = { Accept: accept };
    const init = { method, headers, signal, credentials: "omit", cache: "no-store", referrerPolicy: "no-referrer", redirect: "error" };
    if (method !== "GET") {
      headers["Content-Type"] = "application/json";
      init.body = JSON.stringify(body === undefined ? {} : body);
    }
    return this.fetchImpl(this.url(name, path, query), init);
  }

  /** Route `name`'s JSON answer (null for 204), or a GateError. */
  async call(name, options = {}) {
    const resp = await this.send(name, options);
    if (!resp.ok) throw await errorFrom(resp);
    if (resp.status === 204) return null;
    try {
      return await resp.json();
    } catch {
      throw new GateError(resp.status, "invalid_response", "the gate's answer is not JSON");
    }
  }

  // ---------------------------------------------------------------- one method per route

  /** What this backend offers: { profile, features: [...], retention: {...}, ... }. The console
   * hides what a backend lacks. */
  info() { return this.call("info"); }
  /** The gate's tasks, newest first: { items, next }. `kind`: "a2a" or "mcp". */
  tasks({ service, state, kind, before, limit } = {}) { return this.call("tasks", { query: { service, state, kind, before, limit } }); }
  /** Conversations (service, caller, context), newest activity first: { items, next }. */
  conversations({ service, before, limit } = {}) { return this.call("conversations", { query: { service, before, limit } }); }
  /** One conversation and its tasks, oldest first. */
  conversation(service, caller, context) { return this.call("conversation", { path: { service, caller, context } }); }
  /** One task: its metadata, its snapshot, and `actions` (a subset of reply, cancel, continue). */
  task(taskId) { return this.call("task", { path: { task_id: taskId } }); }
  /** The task's events as Server-Sent Events after seq `after`: the fetch Response (sse.js reads
   * it), so the reconnect point is ours to name. */
  taskEvents(taskId, { after, signal } = {}) {
    return this.send("task_events", { path: { task_id: taskId }, query: { after }, signal, accept: "text/event-stream" });
  }
  /** A part the gate stores apart (`kontra-blob:<ref>` on the thread): the fetch Response, its
   * body the file's bytes (js/files.js reads it). */
  taskFile(taskId, ref, { signal } = {}) {
    return this.send("task_file", { path: { task_id: taskId, ref }, signal, accept: "application/octet-stream" });
  }
  /** Start a task: { service, text?, data? | form?, a2ui_catalogs? } (`form`: { values } of the
   * card's AgentStack initial form); { task_id, state, message_id }. */
  startTask(body) { return this.call("start_task", { body }); }
  /** What starting a task on service `name` takes, read from its card: { name, description,
   * skills (with their examples), accepts_json, a2ui, initial_form, unsupported_extensions }. */
  async startForm(name) {
    const f = await this.call("start_form", { path: { name } });
    // The gate returns the card's AgentStack initial form as the card has it; the console draws it normalised.
    if (f && f.initial_form) {
      const { form, note } = normalizeForm(f.initial_form);
      f.initial_form = form;
      f.initial_form_note = note;
    }
    return f;
  }
  /** Answer a task that waits for input (reply offered): `{text}`, an A2UI action (`a2ui`), a form's
   * answer (`data`, with `structured_input` for a Structured Input request) or an AgentStack form's
   * answer (`form_response: {values}`), each with the catalogs the console draws (`a2ui_catalogs`,
   * js/a2uiwire.js). */
  replyTask(taskId, message) { return this.call("task_message", { path: { task_id: taskId }, body: message }); }
  /** A2A CancelTask (cancel offered): { state } after the call (409 task_not_cancelable once it
   * ended or when the agent refuses). */
  cancelTask(taskId) { return this.call("cancel_task", { path: { task_id: taskId } }); }
  /** A new task in an ended task's context (continue offered): { text?, data?, a2ui_catalogs? };
   * { task_id, state, message_id }. */
  continueTask(taskId, body) { return this.call("continue_task", { path: { task_id: taskId }, body }); }
  /** The gate's services: kind, served card (A2A), connected sessions, retention. */
  services() { return this.call("services"); }
  service(name) { return this.call("service", { path: { name } }); }
  /** The live tunnel sessions of the nodes connected to the gate. */
  tunnels() { return this.call("tunnels"); }
  /** The gate's A2A exchanges on both legs, newest first: { items, next }. `leg`: "caller" or "callee". */
  exchanges({ service, method, task, failed, leg, context, before, limit } = {}) {
    return this.call("exchanges", { query: { service, method, task, failed: failed ? "1" : undefined, leg, context, before, limit } });
  }
  /** The gate's MCP tool calls, newest first: { items, next }. */
  calls({ service, before, limit } = {}) { return this.call("calls", { query: { service, before, limit } }); }
  /** A2A contexts (service, caller, contextId), newest activity first: { items, next }. */
  contexts({ service, before, limit } = {}) { return this.call("contexts", { query: { service, before, limit } }); }
  /** One context and its tasks, oldest first. */
  context(service, caller, context) { return this.call("context", { path: { service, caller, context } }); }
  /** A context's timeline: { opening, callee_contexts, task_ids, items, more } (D360). */
  contextTimeline(service, caller, context) { return this.call("context_timeline", { path: { service, caller, context } }); }
  /** A task's timeline: { task, opening, callee_contexts, items, more }: its events with their origin, and the
   * exchanges that carried none (D360). */
  taskTimeline(taskId) { return this.call("task_timeline", { path: { task_id: taskId } }); }
  /** One exchange, with its bodies or frames and their validation when the gate kept them. */
  exchange(id) { return this.call("exchange", { path: { id } }); }
  /** A service's callee card and served card, each validated, and what the gate set. */
  serviceCards(name) { return this.call("service_cards", { path: { name } }); }

  /** The playground's A2A call (D360): `method` on `binding` ("jsonrpc" or "rest") to the service's own A2A route,
   * with A2A-Version 1.0 and the extensions given, its body the JSON-RPC envelope or the HTTP+JSON body as the
   * person wrote it (`params` fills an HTTP+JSON path). Same origin, no credential, no redirect: the fetch Response,
   * whatever its status (a stream's body read by sse.js). */
  a2a(service, binding, method, { body, params = {}, query = {}, extensions = [], lastEventId, signal } = {}) {
    const [verb] = binding === "jsonrpc" ? ["POST"] : A2A_ROUTES[method] || [];
    if (!verb) throw new Error(`no A2A route ${method}`);
    const headers = { Accept: A2A_STREAMS.includes(method) ? "text/event-stream" : "application/json", "A2A-Version": "1.0" };
    if (extensions.length) headers["A2A-Extensions"] = extensions.join(", ");
    if (lastEventId) headers["Last-Event-ID"] = String(lastEventId);
    const init = { method: verb, headers, signal, credentials: "omit", cache: "no-store", referrerPolicy: "no-referrer", redirect: "error" };
    if (verb !== "GET" && verb !== "DELETE") {
      headers["Content-Type"] = "application/json";
      init.body = typeof body === "string" ? body : JSON.stringify(body === undefined ? {} : body);
    }
    const q = new URLSearchParams();
    for (const [k, v] of Object.entries(binding === "jsonrpc" ? {} : query)) {
      if (v !== undefined && v !== null && v !== "") q.set(k, typeof v === "string" ? v : JSON.stringify(v));
    }
    const qs = q.toString();
    return this.fetchImpl(this.base + a2aPath(binding, method, { ...params, service }) + (qs ? `?${qs}` : ""), init);
  }
}
