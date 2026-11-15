// MCP calls (#/calls, GET /v1/calls): the gate's MCP tool calls newest first, with tool, caller,
// sizes, duration and outcome, each linking to its task when it is task-augmented.

import { gate } from "../app.js";
import { $, fill, h } from "../dom.js";
import { showView } from "../nav.js";
import { ago, failure, pager, shortId, table } from "./common.js";

let generation = 0;

export function leaveCalls() {
  generation += 1;
  const root = $("#calls");
  if (root) fill(root, []);
}

const bytes = (n) => (typeof n === "number" ? `${n} B` : "—");

function row(c) {
  const bad = Boolean(c.error) || c.outcome === "error";
  return h("tr", { dataset: { call: c.id || "" } }, [
    h("td", { class: "muted", title: c.at || "", text: ago(c.at) }),
    h("td", { text: c.service || "—" }),
    h("td", { class: "mono", text: c.tool || "—" }),
    h("td", { class: "muted", text: [c.caller, c.via].filter(Boolean).join(" · ") || "—" }),
    h("td", { class: "m", text: `${bytes(c.bytes_in)} / ${bytes(c.bytes_out)}` }),
    h("td", { class: "m", text: typeof c.duration_ms === "number" ? `${c.duration_ms} ms` : "—" }),
    h("td", { class: bad ? "err" : "", text: c.outcome || c.error || "—" }),
    h("td", {}, [c.task_id ? h("a", { href: `#/t/${c.task_id}`, title: c.task_id, text: shortId(c.task_id) }) : h("span", { class: "muted", text: "—" })]),
  ]);
}

/** #/calls. */
export async function showCalls() {
  showView("#view-calls", "calls");
  const root = $("#calls");
  const mine = ++generation;
  const current = () => mine === generation;
  const list = pager(
    (before) => gate.calls({ before, limit: 50 }),
    () => {
      if (!current()) return;
      fill(root, [list.error && !list.items.length
        ? failure(list.error, "The gate's MCP calls")
        : h("div", { class: "card", id: "callscard" }, [h("div", { class: "bd stack" }, [
          table(["When", "Service", "Tool", "Principal", "In / out", "Took", "Outcome", "Task"], list.items.map(row), "No MCP call yet."),
          h("div", { class: "row" }, [list.more]),
        ])])]);
    },
    current,
  );
  fill(root, [h("p", { class: "empty", text: "Loading…" })]);
  await list.read(true);
  return () => {};
}
