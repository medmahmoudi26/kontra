// One exchange (#/x/<id>, GET /v1/exchanges/{id}): one A2A call on either leg as the gate recorded it -- its leg,
// binding, method, A2A version, ids, caller, sizes, duration and outcome -- and, when the gate kept them
// (traffic.bodies, D358), its request body and its answer or its stream's frames as sent, credentials redacted, each
// with its validation against the pinned A2A specification. Bodies are shown as text, never parsed as markup.

import { gate, partiesOf } from "../app.js";
import { $, fill, fmtBytes, h } from "../dom.js";
import { showView } from "../nav.js";
import { wireOfExchange } from "../taskevents.js";
import { jsonBox } from "./cards.js";
import { failure, shortId, when } from "./common.js";
import { idRow, labelChip, legOf, validationMark } from "./wire.js";

let generation = 0;

export function leaveExchange() {
  generation += 1;
  const root = $("#exchange");
  if (root) fill(root, []);
}

/** A body's text, pretty when it is JSON (a cut body as it came). */
function bodyText(b) {
  if (!b) return "";
  if (typeof b.text !== "string") return `(binary, ${fmtBytes(b.size)}, base64)\n${b.base64 || ""}`;
  if (!b.truncated) {
    try {
      return JSON.stringify(JSON.parse(b.text), null, 2);
    } catch {
      // not JSON: as it came
    }
  }
  return b.text;
}

function bodyCard(title, b, id) {
  if (!b) return null;
  const note = b.truncated ? `cut at the gate's traffic.body_max: ${fmtBytes(b.size)} in all` : fmtBytes(b.size);
  return h("div", { class: "card", id }, [
    h("div", { class: "hd" }, [h("h3", { text: title }), h("span", { class: "muted small", text: note }), validationMark(b.validation)]),
    h("div", { class: "bd" }, [jsonBox(bodyText(b))]),
  ]);
}

/** #/x/<id>. */
export async function showExchange(id) {
  showView("#view-exchange", "traffic", [shortId(id || "")]);
  const root = $("#exchange");
  const mine = ++generation;
  fill(root, [h("p", { class: "empty", text: "Loading…" })]);
  let x;
  try {
    x = await gate.exchange(id);
  } catch (err) {
    if (mine === generation) fill(root, [failure(err, "This exchange")]);
    return () => {};
  }
  if (mine !== generation) return () => {};
  const parties = partiesOf(x.principal, x.service);
  const w = wireOfExchange(x, parties);
  $("#exchangetitle").textContent = w.label;
  $("#exchangesub").textContent = `${x.leg === "callee" ? "The gate's call to the agent" : "A call the gate received"}, ${when(x.at)}`;
  const facts = [
    ["leg", x.leg], ["binding", x.binding === "rest" ? "HTTP+JSON" : "JSON-RPC"], ["A2A version", x.a2a_version || "—"],
    ["service", x.service], ["caller", [x.principal, x.via].filter(Boolean).join(" · ") || "—"],
    ["outcome", w.outcome || "—"], ["took", `${x.duration_ms} ms`], ["sizes", `${fmtBytes(x.bytes_in)} in, ${fmtBytes(x.bytes_out)} out`],
  ];
  const parts = [
    h("div", { class: "card", id: "exchangefacts" }, [h("div", { class: "hd" }, [labelChip(w)]), h("div", { class: "bd stack" }, [
      ...facts.map(([k, v]) => h("div", { class: "idrow" }, [h("b", { text: k }), h("span", { text: String(v) })])),
      idRow("exchange id", x.id),
      x.task_id ? idRow("taskId", x.task_id, { href: `#/t/${x.task_id}`, leg: legOf(parties, false) }) : null,
      x.callee_task_id ? idRow("taskId", x.callee_task_id, { leg: legOf(parties, true) }) : null,
      x.context_id ? idRow("contextId", x.context_id, { leg: legOf(parties, x.leg === "callee") }) : null,
    ])]),
  ];
  if (!x.bodies_kept) {
    parts.push(h("p", { class: "empty", id: "nobodies", text: "This exchange's bodies were not kept (traffic.bodies was off, or the service is sealed): only what the call was is recorded." }));
  } else {
    parts.push(bodyCard("Request", x.request, "exchangerequest"));
    parts.push(bodyCard("Answer", x.response, "exchangeresponse"));
    const frames = Array.isArray(x.frames) ? x.frames : [];
    if (frames.length) {
      parts.push(h("div", { class: "card", id: "exchangeframes" }, [
        h("div", { class: "hd" }, [h("h3", { text: `Stream: ${frames.length} frame${frames.length === 1 ? "" : "s"}` }),
          x.frames_dropped ? h("span", { class: "muted small", text: `${x.frames_dropped} more not kept` }) : null]),
        h("div", { class: "bd stack" }, frames.map((f) => h("div", { class: "frame", dataset: { frame: String(f.n) } }, [
          h("div", { class: "row" }, [h("span", { class: "mono small", text: `+${f.offset_ms} ms` }),
            h("span", { class: "mono small", text: f.sse_id ? `id: ${f.sse_id}` : "no id" }), validationMark(f.validation)]),
          jsonBox(bodyText(f.data)),
        ]))),
      ]));
    }
  }
  fill(root, parts.filter(Boolean));
  return () => {};
}
