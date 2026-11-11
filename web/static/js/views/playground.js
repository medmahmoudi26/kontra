// The playground (#/play, #/play/<service>; the local development profile, D360): any of A2A 1.0's eleven methods sent
// to a service on the binding the person picks, from a JSON body prefilled with what the method requires, as a stock A2A
// client sends it -- to the service's own A2A route on the gate's origin (api.js's a2a: A2A-Version 1.0, the
// extensions given, no credential) -- so the gate admits, relays and records it as any client's. It shows the raw
// answer, or a stream's frames as they come, then the exchange the gate recorded for it with its validation.
// Everything shown is text.

import { gate } from "../app.js";
import { $, fill, h } from "../dom.js";
import { showView } from "../nav.js";
import { A2A_ROUTES, A2A_STREAMS } from "../routes.js";
import { readEvents } from "../sse.js";
import { jsonBox } from "./cards.js";
import { failure } from "./common.js";
import { validationMark } from "./wire.js";

let generation = 0;
let abort = null;

export function leavePlayground() {
  generation += 1;
  if (abort) abort.abort();
  abort = null;
  const root = $("#playground");
  if (root) fill(root, []);
}

const METHODS = Object.keys(A2A_ROUTES);
const uuid = () => (globalThis.crypto && crypto.randomUUID ? crypto.randomUUID() : `m-${Date.now()}`);

/** A method's params, prefilled with the members A2A 1.0 requires (placeholders for the ids to fill in). */
export function skeleton(method) {
  const message = () => ({ message: { messageId: uuid(), role: "ROLE_USER", parts: [{ text: "hello" }] } });
  switch (method) {
    case "SendMessage":
    case "SendStreamingMessage":
      return message();
    case "GetTask": return { id: "<taskId>", historyLength: 10 };
    case "ListTasks": return { pageSize: 20 };
    case "CancelTask":
    case "SubscribeToTask": return { id: "<taskId>" };
    case "CreateTaskPushNotificationConfig": return { taskId: "<taskId>", url: "http://127.0.0.1:9000/hook", token: "my-token" };
    case "GetTaskPushNotificationConfig":
    case "DeleteTaskPushNotificationConfig": return { taskId: "<taskId>", id: "<configId>" };
    case "ListTaskPushNotificationConfigs": return { taskId: "<taskId>" };
    default: return {};
  }
}

/** The body the person edits: the JSON-RPC envelope, or the HTTP+JSON params. */
export function draft(binding, method) {
  const params = skeleton(method);
  return JSON.stringify(binding === "jsonrpc" ? { jsonrpc: "2.0", id: 1, method, params } : params, null, 2);
}

/** What an HTTP+JSON call takes from the params: the path's ids, the query of a GET, the body of the rest. */
export function restParts(method, params) {
  const [verb, tpl] = A2A_ROUTES[method];
  const path = {};
  const rest = { ...params };
  for (const m of tpl.matchAll(/\{([a-z_]+)\}/g)) {
    const key = m[1] === "task_id" ? "taskId" : m[1];
    if (key !== "service" && typeof rest[key] === "string") {
      path[m[1]] = rest[key];
      delete rest[key];
    }
  }
  return verb === "GET" || verb === "DELETE" ? { params: path, query: rest, body: undefined } : { params: path, query: {}, body: rest };
}

/** #/play and #/play/<service>. */
export async function showPlayground(service) {
  showView("#view-playground", "playground", service ? [service] : null);
  const root = $("#playground");
  const mine = ++generation;
  const alive = () => mine === generation;
  fill(root, [h("p", { class: "empty", text: "Loading…" })]);
  let services;
  try {
    const got = await gate.services();
    services = (Array.isArray(got) ? got : got.items || []).filter((s) => s.kind === "a2a" && !s.disabled);
  } catch (err) {
    if (alive()) fill(root, [failure(err, "The gate's services")]);
    return () => {};
  }
  if (!alive()) return () => {};
  if (!services.length) {
    fill(root, [h("p", { class: "empty", text: "No A2A service yet: connect one with kontra connect." })]);
    return () => {};
  }
  const svc = h("select", { id: "playservice", "aria-label": "Service" }, services.map((s) => h("option", { value: s.name, text: s.name })));
  svc.value = services.some((s) => s.name === service) ? service : services[0].name;
  const method = h("select", { id: "playmethod", "aria-label": "Method" }, METHODS.map((m) => h("option", { value: m, text: m })));
  const binding = h("select", { id: "playbinding", "aria-label": "Binding" }, [h("option", { value: "jsonrpc", text: "JSON-RPC" }), h("option", { value: "rest", text: "HTTP+JSON" })]);
  const exts = h("input", { id: "playext", type: "text", placeholder: "A2A-Extensions (comma-separated URIs)", "aria-label": "A2A-Extensions" });
  const lastId = h("input", { id: "playlastid", type: "text", placeholder: "Last-Event-ID", "aria-label": "Last-Event-ID", hidden: true });
  const body = h("textarea", { id: "playbody", rows: 14, spellcheck: "false", "aria-label": "Body" });
  const send = h("button", { class: "btn", type: "button", id: "playsend", text: "Send" });
  const out = h("div", { class: "stack", id: "playout" });
  const reset = () => {
    body.value = draft(binding.value, method.value);
    lastId.hidden = method.value !== "SubscribeToTask";
  };
  method.addEventListener("change", reset);
  binding.addEventListener("change", reset);
  reset();

  const recorded = async (since, m) => {
    // The gate records the call when it ends, in batches (250 ms): look for it a few times.
    for (let i = 0; i < 8 && alive(); i++) {
      await new Promise((r) => setTimeout(r, 300));
      try {
        const page = await gate.exchanges({ service: svc.value, method: m, leg: "caller", limit: 5 });
        const x = (page.items || []).find((it) => Date.parse(it.at) >= since - 1000);
        if (x) return gate.exchange(x.id);
      } catch {
        return null;
      }
    }
    return null;
  };

  send.addEventListener("click", async () => {
    if (abort) abort.abort();
    abort = new AbortController();
    const m = method.value;
    let parsed;
    try {
      parsed = JSON.parse(body.value);
    } catch (err) {
      fill(out, [h("p", { class: "err", text: `The body is not JSON: ${err.message}` })]);
      return;
    }
    const extensions = exts.value.split(",").map((x) => x.trim()).filter(Boolean);
    const since = Date.now();
    fill(out, [h("p", { class: "muted", text: `Sending ${m}…` })]);
    let resp;
    try {
      if (binding.value === "jsonrpc") {
        resp = await gate.a2a(svc.value, "jsonrpc", m, { body: body.value, extensions, lastEventId: lastId.value.trim(), signal: abort.signal });
      } else {
        const parts = restParts(m, parsed);
        resp = await gate.a2a(svc.value, "rest", m, { ...parts, extensions, lastEventId: lastId.value.trim(), signal: abort.signal });
      }
    } catch (err) {
      if (alive()) fill(out, [h("p", { class: "err", text: `The call failed: ${err.message}` })]);
      return;
    }
    const head = h("p", { class: "mono", id: "playstatus", text: `HTTP ${resp.status} ${resp.headers.get("Content-Type") || ""}` });
    const isStream = (resp.headers.get("Content-Type") || "").startsWith("text/event-stream");
    if (isStream && A2A_STREAMS.includes(m)) {
      const frames = h("div", { class: "stack", id: "playframes" });
      fill(out, [head, frames]);
      try {
        for await (const ev of readEvents(resp.body, { signal: abort.signal })) {
          if (!alive()) return;
          frames.appendChild(h("div", { class: "frame", dataset: { frame: ev.id || "" } }, [
            h("span", { class: "mono small", text: ev.id ? `id: ${ev.id}` : "no id" }), jsonBox(ev.data)]));
        }
      } catch {
        // aborted, or the stream broke: what came is shown
      }
    } else {
      let text = await resp.text();
      try {
        text = JSON.stringify(JSON.parse(text), null, 2);
      } catch {
        // not JSON: as it came
      }
      fill(out, [head, jsonBox(text)]);
    }
    const x = await recorded(since, m);
    if (!alive()) return;
    if (!x) {
      out.appendChild(h("p", { class: "muted small", text: "The gate's record of this call is not there yet (Traffic shows it once written)." }));
      return;
    }
    const marks = [x.request && validationMark(x.request.validation), x.response && validationMark(x.response.validation),
      ...(Array.isArray(x.frames) ? x.frames.map((f) => validationMark(f.validation)) : [])].filter(Boolean);
    out.appendChild(h("div", { class: "row", id: "playrecorded" }, [
      h("a", { href: `#/x/${x.id}`, text: "The exchange the gate recorded" }), ...marks]));
  });

  fill(root, [h("div", { class: "card", id: "playcard" }, [h("div", { class: "bd stack" }, [
    h("div", { class: "row filters" }, [svc, method, binding, lastId]),
    exts, body, h("div", { class: "row" }, [send]),
  ])]), out]);
  return () => {
    if (abort) abort.abort();
  };
}
