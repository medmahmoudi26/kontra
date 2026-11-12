// Traffic (#/traffic, GET /v1/exchanges): the gate's A2A exchanges on both legs (D355) newest first, refused ones
// included: the calls it received (Caller · <method>) and the calls it made to its agents (Gate → Callee · <method>),
// each with its binding, A2A method, contextId and outcome, linking to its exchange (#/x/<id>: bodies, frames,
// validation) and to its task. Filters: leg, method, contextId, failed only. How long they are kept is /v1/info's.

import { gate, offers, partiesOf, state } from "../app.js";
import { $, fill, h } from "../dom.js";
import { showView } from "../nav.js";
import { wireOfExchange } from "../taskevents.js";
import { ago, failure, pager, shortId, span, table } from "./common.js";
import { labelChip } from "./wire.js";

let generation = 0;
let failedOnly = false;
let leg = "";
let method = "";
let context = "";

export function leaveTraffic() {
  generation += 1;
  const root = $("#traffic");
  if (root) fill(root, []);
}

/** An exchange's outcome in words: its HTTP status, its JSON-RPC code, the gate's reason. */
export function outcome(x) {
  const parts = [x.http_status ? String(x.http_status) : null, x.rpc_code !== null && x.rpc_code !== undefined ? `rpc ${x.rpc_code}` : null, x.reason || null];
  return parts.filter(Boolean).join(" · ") || "—";
}

const failed = (x) => (x.http_status && x.http_status >= 400) || (x.rpc_code !== null && x.rpc_code !== undefined) || Boolean(x.reason);

/** The A2A 1.0 methods, and the card fetches: the method filter's choices. */
const METHODS = ["SendMessage", "SendStreamingMessage", "GetTask", "ListTasks", "CancelTask", "SubscribeToTask",
  "CreateTaskPushNotificationConfig", "GetTaskPushNotificationConfig", "ListTaskPushNotificationConfigs",
  "DeleteTaskPushNotificationConfig", "GetExtendedAgentCard", "card", "upstream_card"];

function row(x) {
  const w = wireOfExchange(x, partiesOf(x.principal, x.service));
  return h("tr", { dataset: { exchange: x.id || "", failed: failed(x) ? "1" : "", leg: x.leg || "caller" } }, [
    h("td", { class: "muted", title: x.at || "", text: ago(x.at) }),
    h("td", {}, [h("a", { href: `#/x/${x.id}`, title: "Open this exchange" }, [labelChip(w)])]),
    h("td", { text: x.service || "—" }),
    h("td", { class: "muted", text: x.binding === "rest" ? "HTTP+JSON" : "JSON-RPC" }),
    h("td", { class: "muted", text: x.leg === "callee" ? "the gate" : [x.principal, x.via].filter(Boolean).join(" · ") || "—" }),
    h("td", { class: "mono small", title: x.context_id || "", text: x.context_id ? shortId(x.context_id) : "—" }),
    h("td", { class: failed(x) ? "err" : "", text: outcome(x) }),
    h("td", { class: "m", text: typeof x.duration_ms === "number" ? `${x.duration_ms} ms` : "—" }),
    h("td", {}, [x.task_id ? h("a", { href: `#/t/${x.task_id}`, title: x.task_id, text: shortId(x.task_id) }) : h("span", { class: "muted", text: "—" })]),
  ]);
}

/** What /v1/info says of the traffic's retention and bodies, as a line. */
function horizon() {
  const info = state.info || {};
  const r = info.retention ? info.retention.traffic : undefined;
  if (r === undefined || r === null) return null;
  const kept = Number(r) === 0 ? "Exchanges are kept forever" : `Exchanges are kept ${span(r)}`;
  const bodies = info.traffic && info.traffic.bodies ? ", with their bodies." : ", metadata only.";
  return h("p", { class: "small", id: "trafficretention", text: kept + (offers("bodies") ? bodies : ".") });
}

function select(id, label, value, choices, onChange) {
  const s = h("select", { id, "aria-label": label }, choices.map(([v, t]) => h("option", { value: v, text: t, selected: v === value || null })));
  s.value = value;
  s.addEventListener("change", () => onChange(s.value));
  return s;
}

/** #/traffic. */
export async function showTraffic() {
  showView("#view-traffic", "traffic");
  const root = $("#traffic");
  const mine = ++generation;
  const current = () => mine === generation;
  const toggle = h("button", { class: "btn sm", type: "button", "aria-pressed": String(failedOnly), id: "failedonly", text: "Failed only" });
  toggle.addEventListener("click", () => { failedOnly = !failedOnly; showTraffic(); });
  const legs = select("legfilter", "Leg", leg, [["", "Both legs"], ["caller", "Principal → gate"], ["callee", "Gate → service"]], (v) => { leg = v; showTraffic(); });
  const methods = select("methodfilter", "Method", method, [["", "Every method"], ...METHODS.map((m) => [m, m])], (v) => { method = v; showTraffic(); });
  const ctx = h("input", { id: "contextfilter", type: "search", placeholder: "contextId", "aria-label": "contextId", value: context });
  ctx.addEventListener("change", () => { context = ctx.value.trim(); showTraffic(); });
  const list = pager(
    (before) => gate.exchanges({ failed: failedOnly, leg, method, context, before, limit: 50 }),
    () => {
      if (!current()) return;
      fill(root, [h("div", { class: "row filters" }, [legs, methods, ctx, toggle, horizon()]), list.error && !list.items.length
        ? failure(list.error, "The gate's exchanges")
        : h("div", { class: "card", id: "trafficcard" }, [h("div", { class: "bd stack" }, [
          table(["When", "A2A", "Service", "Binding", "Principal", "contextId", "Outcome", "Took", "Task"], list.items.map(row), failedOnly ? "No failed exchange." : "No exchange yet."),
          h("div", { class: "row" }, [list.more]),
        ])])]);
    },
    current,
  );
  fill(root, [h("p", { class: "empty", text: "Loading…" })]);
  await list.read(true);
  return () => {};
}
