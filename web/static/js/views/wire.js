// The A2A wire, drawn (D357, D360): an item's label names its party and the A2A type (`anonymous · SendMessage`,
// `red · Message`, `Gate · TaskStatusUpdateEvent`, `Gate → red · GetTask`), the object the principal received beside it and a
// conversion the gate made, its validation against the pinned A2A specification, and ids in full with Copy. The
// context page's timeline, the thread's event log and the exchange page draw with these. Every string is text, never
// markup (dom.js's h sets textContent).

import { h } from "../dom.js";
import { routingOf, validationWords, wireOf, wireOfExchange } from "../taskevents.js";
import { copyButton, shortId } from "./common.js";
import { hms } from "./cards.js";

/** Past this many characters a party's name is cut, its full name on hover (an `oidc:sub:` can be long). */
const NAME_MAX = 32;
const cutName = (s) => (s.length > NAME_MAX ? `${s.slice(0, NAME_MAX - 1)}…` : s);

/** The label chip of an item: its party as the headline, the A2A type as smaller text; coloured by actor. */
export function labelChip(w) {
  if (!w.party) return h("span", { class: `wire wire-${w.actor || "unknown"}`, dataset: { wire: w.label }, text: w.label });
  return h("span", { class: `wire wire-${w.actor || "unknown"}`, dataset: { wire: w.label }, title: w.label }, [
    h("b", { class: "party", text: cutName(w.party) }), h("span", { class: "ty", text: ` · ${w.typeName}` }),
  ]);
}

/** The leg an id belongs to, by its two parties' names: `anonymous → gate`, `gate → red`. */
export const legOf = (parties, callee) => (callee ? `gate → ${(parties && parties.service) || "unknown"}` : `${(parties && parties.principal) || "unknown"} → gate`);

/** An id in full, monospace, with Copy (spec: Identifiers Shown In Full); `leg` names the two parties it belongs to. */
export function idRow(label, id, { href = null, leg = null } = {}) {
  const head = [h("b", { text: label }), leg ? h("span", { class: "leg small muted", text: leg }) : null];
  const data = { id: label, leg: leg || "" };
  if (!id) return h("div", { class: "idrow", dataset: data }, [...head, h("span", { class: "muted", text: "none" })]);
  const value = href ? h("a", { class: "mono", href, text: id }) : h("span", { class: "mono", text: id });
  return h("div", { class: "idrow", dataset: data }, [...head, value, copyButton(id, "Copy")]);
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
export function routingRows(m, { first = false, parties = null } = {}) {
  const r = routingOf(m);
  if (!r) return [];
  const leg = legOf(parties, false);
  const rows = [idRow("contextId", r.contextId, { leg }), first || !r.taskId ? idRow("taskId", null, { leg }) : idRow("taskId", r.taskId, { href: `#/t/${r.taskId}`, leg })];
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

/** One timeline row: an event or an exchange (GET …/timeline items), labelled by its party's name (`parties`, from
 * app.js's partiesOf); `taskLinks` adds the item's task. */
export function timelineRow(item, { taskLinks = false, parties = null } = {}) {
  if (item.type === "exchange") {
    const w = wireOfExchange(item, parties);
    return h("div", { class: "tl-row", dataset: { kind: "exchange", wire: w.label, exchange: item.id || "" } }, [
      h("time", { text: hms(item.at) }),
      h("div", { class: "tl-main" }, [
        h("div", { class: "row tl-head" }, [labelChip(w), h("span", { class: w.failed ? "err small" : "muted small", text: w.outcome }),
          h("a", { class: "small", href: `#/x/${item.id}`, text: "exchange" }),
          taskLinks && item.task_id ? h("a", { class: "small mono", href: `#/t/${item.task_id}`, title: item.task_id, text: shortId(item.task_id) }) : null]),
      ]),
    ]);
  }
  const w = wireOf(item, parties);
  const state = stateOf(item.payload);
  const detail = [];
  if (w.callerType && (w.convertedTo || w.callerType !== w.type)) {
    // The gate converted what the callee sent when the caller received the converted type; a message-only
    // interaction's Message reached the caller as it was sent, though the log holds it converted (D362).
    const converted = Boolean(w.convertedTo) && w.convertedTo === w.callerType;
    detail.push(h("span", { class: "conv", dataset: { converted: converted ? w.convertedTo : w.callerType },
      text: `${(parties && parties.principal) || "the principal"} received a ${w.callerType}${converted ? " (converted by the gate)" : ""}` }));
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
        taskLinks && item.task_id ? h("a", { class: "small mono", href: `#/t/${item.task_id}`, title: item.task_id, text: shortId(item.task_id) }) : null]),
      msg ? h("div", { class: "tl-ids" }, routingRows(msg, { first: item.kind === "task", parties })) : null,
    ]),
  ]);
}

/** A context's opening (spec: Context Opening Shown) and the service's own contexts for it, each id labelled with
 * the names of its leg's two parties (`parties`, from app.js's partiesOf). */
export function openingNodes(opening, callee, parties = null) {
  const out = [];
  const who = (parties && parties.principal) || "the principal";
  if (opening) {
    const how = opening.assigned === true ? "contextId assigned by the gate" : opening.assigned === false ? `contextId sent by ${who}` : "how the context was opened was not recorded";
    out.push(h("div", { class: "opening", dataset: { opening: opening.assigned === true ? "gate" : opening.assigned === false ? "caller" : "" } }, [
      h("p", { text: how }), idRow("contextId", opening.context_id, { leg: legOf(parties, false) }),
    ]));
  }
  const leg = legOf(parties, true);
  for (const c of Array.isArray(callee) ? callee : []) {
    const where = c.backend === "tunnel" && c.session
      ? h("span", { class: "muted small", title: c.session, text: `on tunnel session ${shortId(c.session)}` })
      : h("span", { class: "muted small", text: `on the ${c.backend} backend` });
    out.push(h("div", { class: "idrow", dataset: { callee: c.context_id, leg } }, [h("b", { text: "contextId" }),
      h("span", { class: "leg small muted", text: leg }),
      h("span", { class: "mono", text: c.context_id }), copyButton(c.context_id, "Copy"), where]));
  }
  return out;
}
