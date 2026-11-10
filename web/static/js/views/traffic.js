// Traffic (#/traffic, GET /v1/exchanges): the gate's A2A exchanges newest first, refused ones
// included, with their outcome, each linking to its task when it has one; failed ones only on
// request. How long they are kept is /v1/info's.

import { gate, state } from "../app.js";
import { $, fill, h } from "../dom.js";
import { showView } from "../nav.js";
import { ago, failure, pager, shortId, span, table } from "./common.js";

let generation = 0;
let failedOnly = false;

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

function row(x) {
  return h("tr", { dataset: { exchange: x.id || "", failed: failed(x) ? "1" : "" } }, [
    h("td", { class: "muted", title: x.at || "", text: ago(x.at) }),
    h("td", { text: x.service || "—" }),
    h("td", { class: "mono", text: x.method || "—" }),
    h("td", { class: "muted", text: [x.principal, x.via].filter(Boolean).join(" · ") || "—" }),
    h("td", { class: failed(x) ? "err" : "", text: outcome(x) }),
    h("td", { class: "m", text: typeof x.duration_ms === "number" ? `${x.duration_ms} ms` : "—" }),
    h("td", {}, [x.task_id ? h("a", { href: `#/t/${x.task_id}`, text: shortId(x.task_id) }) : h("span", { class: "muted", text: "—" })]),
  ]);
}

/** What /v1/info says of the traffic's retention, as a line. */
function horizon() {
  const r = state.info && state.info.retention ? state.info.retention.traffic : undefined;
  if (r === undefined || r === null) return null;
  return h("p", { class: "small", id: "trafficretention", text: Number(r) === 0 ? "Exchanges are kept forever." : `Exchanges are kept ${span(r)}.` });
}

/** #/traffic. */
export async function showTraffic() {
  showView("#view-traffic", "traffic");
  const root = $("#traffic");
  const mine = ++generation;
  const current = () => mine === generation;
  const toggle = h("button", { class: "btn sm", type: "button", "aria-pressed": String(failedOnly), id: "failedonly", text: "Failed only" });
  toggle.addEventListener("click", () => { failedOnly = !failedOnly; showTraffic(); });
  const list = pager(
    (before) => gate.exchanges({ failed: failedOnly, before, limit: 50 }),
    () => {
      if (!current()) return;
      fill(root, [h("div", { class: "row filters" }, [toggle, horizon()]), list.error && !list.items.length
        ? failure(list.error, "The gate's exchanges")
        : h("div", { class: "card", id: "trafficcard" }, [h("div", { class: "bd stack" }, [
          table(["When", "Service", "Method", "Caller", "Outcome", "Took", "Task"], list.items.map(row), failedOnly ? "No failed exchange." : "No exchange yet."),
          h("div", { class: "row" }, [list.more]),
        ])])]);
    },
    current,
  );
  fill(root, [h("p", { class: "empty", text: "Loading…" })]);
  await list.read(true);
  return () => {};
}
