// Services (#/services, GET /v1/services): the gate's services with their kind, whether an agent
// is reachable now (a connected session), and how long their ended tasks are kept; an A2A service
// links to starting a task on it.

import { gate } from "../app.js";
import { $, fill, h } from "../dom.js";
import { showView } from "../nav.js";
import { failure, kindTag, span, table } from "./common.js";

let generation = 0;

export function leaveServices() {
  generation += 1;
  const root = $("#services");
  if (root) fill(root, []);
}

/** Whether an agent of service `s` is reachable now. */
export const reachable = (s) => (typeof s.reachable === "boolean" ? s.reachable : Array.isArray(s.sessions) && s.sessions.length > 0);

/** A service's task retention in words: "7d", "kept forever", or "—". */
export function keptText(s) {
  const r = s.retention;
  if (r === undefined || r === null) return "—";
  return Number(r) === 0 ? "kept forever" : span(r);
}

function row(s) {
  const up = reachable(s);
  return h("tr", { dataset: { service: s.name } }, [
    h("td", { text: s.name }),
    h("td", {}, [kindTag(s.kind)]),
    h("td", {}, [h("span", { class: `dot ${up ? "online" : "offline"}` }), up ? " reachable" : " not reachable"]),
    h("td", { class: "muted", text: keptText(s) }),
    h("td", {}, [s.kind === "mcp" ? null : h("a", { class: "btn sm", href: `#/run/${encodeURIComponent(s.name)}`, text: "Start a task" })]),
  ]);
}

/** #/services. */
export async function showServices() {
  showView("#view-services", "services");
  const root = $("#services");
  const mine = ++generation;
  fill(root, [h("p", { class: "empty", text: "Loading…" })]);
  let got;
  try {
    got = await gate.services();
  } catch (err) {
    if (mine === generation) fill(root, [failure(err, "The gate's services")]);
    return () => {};
  }
  if (mine !== generation) return () => {};
  const items = Array.isArray(got) ? got : Array.isArray(got && got.items) ? got.items : [];
  fill(root, [h("div", { class: "card", id: "servicescard" }, [h("div", { class: "bd stack" }, [
    table(["Service", "Kind", "Agent", "Ended tasks kept", ""], items.map(row), "No service yet: configure one, or connect a node with kontra connect."),
  ])])]);
  return () => {};
}
