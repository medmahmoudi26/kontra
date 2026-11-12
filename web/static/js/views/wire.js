// The A2A wire, drawn (D357, D360): an item's label as A2A names it (`Caller · SendMessage`, `Callee · Message`,
// `Gate · TaskStatusUpdateEvent`, `Gate → Callee · GetTask`), the object the caller received beside it and a
// conversion the gate made, its validation against the pinned A2A specification, and ids in full with Copy. The
// context page's timeline, the thread's event log and the exchange page draw with these. Every string is text, never
// markup (dom.js's h sets textContent).

import { h } from "../dom.js";
import { routingOf, validationWords, wireOf, wireOfExchange } from "../taskevents.js";
import { copyButton, shortId } from "./common.js";
import { hms } from "./cards.js";

/** The label chip of an item: `<Actor> · <A2A type>`, coloured by actor. */
export function labelChip(w) {
  return h("span", { class: `wire wire-${w.actor || "unknown"}`, dataset: { wire: w.label }, text: w.label });
}

/** An id in full, monospace, with Copy (spec: Identifiers Shown In Full). */
export function idRow(label, id, { href = null } = {}) {
  if (!id) return h("div", { class: "idrow", dataset: { id: label } }, [h("b", { text: label }), h("span", { class: "muted", text: "none" })]);
  const value = href ? h("a", { class: "mono", href, text: id }) : h("span", { class: "mono", text: id });
  return h("div", { class: "idrow", dataset: { id: label } }, [h("b", { text: label }), value, copyButton(id, "Copy")]);
}

/** A validation result as a mark: ✓ valid, ✗ with its issues (path and rule), or the note of one not checked. */
export function validationMark(v) {
  if (!v) return null;
  const words = validationWords(v);
  if (v.validated && v.valid) return h("span", { class: "vmark ok", title: words, dataset: { valid: "1" }, text: "✓ A2A " + v.version });
  if (!v.validated) return h("span", { class: "vmark muted", title: words, dataset: { valid: "" }, text: "not validated" });
  const issues = Array.isArray(v.issues) ? v.issues : [];
  return h("details", { class: "vmark bad", dataset: { valid: "0" } }, [
    h("summary", { text: words }),
    h("ul", {}, issues.slice(0, 50).map((i) => h("li", {}, [
      h("span", { class: "mono", text: String(i.path || "$") }), h("span", { text: " " + String(i.rule || "") }),
      i.member ? h("span", { class: "muted", text: ` (${i.member})` }) : null,
      i.detail ? h("span", { class: "muted", text: `: ${String(i.detail).slice(0, 300)}` }) : null,
    ]))),
  ]);
}

/** What placed a caller's message (spec: Message Routing Fields Shown): its contextId, its taskId or none, its
 * referenceTaskIds as links. `first` is a task's first message, which named no task (the gate's history fills it). */
export function routingRows(m, { first = false } = {}) {
  const r = routingOf(m);
  if (!r) return [];
  const rows = [idRow("contextId", r.contextId), first || !r.taskId ? idRow("taskId", null) : idRow("taskId", r.taskId, { href: `#/t/${r.taskId}` })];
  if (r.referenceTaskIds.length) {
    rows.push(h("div", { class: "idrow" }, [h("b", { text: "referenceTaskIds" }),
      ...r.referenceTaskIds.map((id) => h("a", { class: "mono", href: `#/t/${id}`, text: id }))]));
  }
  return rows;
}

/** The caller's message an event carries: a task's first (its history's first user message) or a follow-up's. */
function callerMessage(ev) {
  const p = ev && ev.payload;
  if (!p || typeof p !== "object") return null;
  if (ev.kind === "task" && p.task && Array.isArray(p.task.history)) {
    return p.task.history.find((m) => m && String(m.role || "").toLowerCase().includes("user")) || null;
  }
  if (ev.kind === "message" && p.message && String(p.message.role || "").toLowerCase().includes("user")) return p.message;
  return null;
}

/** A state as A2A writes it, short: "working", "input-required". */
const stateOf = (payload) => {
  const s = payload && ((payload.statusUpdate && payload.statusUpdate.status) || (payload.task && payload.task.status));
  return s && s.state ? String(s.state).replace(/^TASK_STATE_/, "").toLowerCase().replaceAll("_", "-") : null;
};

/** One timeline row: an event or an exchange (GET …/timeline items), labelled; `taskLinks` adds the item's task. */
export function timelineRow(item, { taskLinks = false } = {}) {
  if (item.type === "exchange") {
    const w = wireOfExchange(item);
    return h("div", { class: "tl-row", dataset: { kind: "exchange", wire: w.label, exchange: item.id || "" } }, [
      h("time", { text: hms(item.at) }),
      h("div", { class: "tl-main" }, [
        h("div", { class: "row tl-head" }, [labelChip(w), h("span", { class: w.failed ? "err small" : "muted small", text: w.outcome }),
          h("a", { class: "small", href: `#/x/${item.id}`, text: "exchange" }),
          taskLinks && item.task_id ? h("a", { class: "small mono", href: `#/t/${item.task_id}`, text: shortId(item.task_id) }) : null]),
      ]),
    ]);
  }
  const w = wireOf(item);
  const state = stateOf(item.payload);
  const detail = [];
  if (w.callerType && (w.convertedTo || w.callerType !== w.type)) {
    // The gate converted what the callee sent when the caller received the converted type; a message-only
    // interaction's Message reached the caller as it was sent, though the log holds it converted (D362).
    const converted = Boolean(w.convertedTo) && w.convertedTo === w.callerType;
    detail.push(h("span", { class: "conv", dataset: { converted: converted ? w.convertedTo : w.callerType },
      text: `the caller received a ${w.callerType}${converted ? " (converted by the gate)" : ""}` }));
  }
  if (w.method && w.method !== w.type) detail.push(h("span", { class: "muted small", text: `in ${w.method}` }));
  if (state) detail.push(h("span", { class: "muted small", text: state }));
  if (!w.recorded) detail.push(h("span", { class: "muted small", text: "origin not recorded" }));
  const msg = callerMessage(item);
  return h("div", { class: "tl-row", dataset: { kind: "event", seq: String(item.seq || ""), wire: w.label } }, [
    h("time", { text: hms(item.at) }),
    h("div", { class: "tl-main" }, [
      h("div", { class: "row tl-head" }, [labelChip(w), ...detail, validationMark(w.validation),
        w.exchangeId ? h("a", { class: "small", href: `#/x/${w.exchangeId}`, text: "exchange" }) : null,
        taskLinks && item.task_id ? h("a", { class: "small mono", href: `#/t/${item.task_id}`, text: shortId(item.task_id) }) : null]),
      msg ? h("div", { class: "tl-ids" }, routingRows(msg, { first: item.kind === "task" })) : null,
    ]),
  ]);
}

/** A context's opening (spec: Context Opening Shown) and the callee's own contexts for it. */
export function openingNodes(opening, callee) {
  const out = [];
  if (opening) {
    const how = opening.assigned === true ? "contextId assigned by the gate" : opening.assigned === false ? "contextId sent by the caller" : "how the context was opened was not recorded";
    out.push(h("div", { class: "opening", dataset: { opening: opening.assigned === true ? "gate" : opening.assigned === false ? "caller" : "" } }, [
      h("p", { text: how }), idRow("contextId", opening.context_id),
    ]));
  }
  for (const c of Array.isArray(callee) ? callee : []) {
    const where = c.backend === "tunnel" && c.session ? `on tunnel session ${shortId(c.session)}` : `on the ${c.backend} backend`;
    out.push(h("div", { class: "idrow", dataset: { callee: c.context_id } }, [h("b", { text: "callee contextId" }),
      h("span", { class: "mono", text: c.context_id }), copyButton(c.context_id, "Copy"), h("span", { class: "muted small", text: where })]));
  }
  return out;
}
