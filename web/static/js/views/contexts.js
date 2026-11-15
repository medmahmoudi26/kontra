// Contexts (#/, #/contexts, and #/conversations as before; GET /v1/contexts): in A2A every message belongs to a
// context, and a task is what an agent may create in it (D360), so the console opens here. The gate's contexts,
// grouped by service, caller and contextId (a context is scoped per caller), newest activity first; and one of them
// (#/c/<service>/<caller>/<context>): how it was opened, the callee's own contexts for it, its tasks oldest first, and
// one timeline of every interaction in it, each item labelled as A2A names it.

import { gate, offers, partiesOf } from "../app.js";
import { $, fill, h } from "../dom.js";
import { showView } from "../nav.js";
import { TASK_HEAD, taskRow } from "./tasks.js";
import { ago, chip, failure, pager, shortId, stateChip, table } from "./common.js";
import { idRow, legOf, openingNodes, timelineRow } from "./wire.js";

let generation = 0;

export function leaveContexts() {
  generation += 1;
  const root = $("#contexts");
  if (root) fill(root, []);
}

const ctxHref = (c) => `#/c/${encodeURIComponent(c.service)}/${encodeURIComponent(c.caller)}/${encodeURIComponent(c.context_id)}`;

function ctxRow(c) {
  return h("tr", { dataset: { context: c.context_id } }, [
    h("td", {}, [h("a", { class: "mono", href: ctxHref(c), title: c.context_id, text: c.context_id })]),
    h("td", { text: c.service }),
    h("td", { class: "muted", title: c.caller || "", text: c.principal || c.caller || "—" }),
    h("td", { class: "m", text: String(c.tasks ?? "—") }),
    h("td", {}, [chip(stateChip(c.state))]),
    h("td", { class: "muted", title: c.last_at || "", text: ago(c.last_at) }),
  ]);
}

function heading(title, sub) {
  $("#contextstitle").textContent = title;
  $("#contextssub").textContent = sub;
}

/** #/ and #/contexts. */
export async function showContexts() {
  showView("#view-contexts", "contexts");
  heading("Contexts", "Every A2A context on this gate, newest activity first: in A2A each message belongs to a context, and the agent may answer it with a task.");
  const root = $("#contexts");
  const mine = ++generation;
  const current = () => mine === generation;
  const read = (before) => (offers("contexts") ? gate.contexts({ before, limit: 50 }) : gate.conversations({ before, limit: 50 }));
  const list = pager(
    read,
    () => {
      if (!current()) return;
      fill(root, [list.error && !list.items.length
        ? failure(list.error, "The gate's contexts")
        : h("div", { class: "card", id: "ctxcard" }, [h("div", { class: "bd stack" }, [
          table(["contextId", "Service", "Principal", "Tasks", "Latest", "Active"], list.items.map(ctxRow), "No context yet."),
          h("div", { class: "row" }, [list.more]),
        ])])]);
    },
    current,
  );
  fill(root, [h("p", { class: "empty", text: "Loading…" })]);
  await list.read(true);
  return () => {};
}

/** #/c/<service>/<caller>/<context>. */
export async function showContext(service, caller, context) {
  showView("#view-contexts", "contexts", [shortId(context || "")]);
  heading(`Context ${context || "?"}`, `${service || "?"} · caller ${caller || "?"}: every interaction in this context, in time order.`);
  const root = $("#contexts");
  const mine = ++generation;
  if (!service || !caller || !context) {
    fill(root, [h("p", { class: "empty err", text: "That is not a context's address." })]);
    return () => {};
  }
  fill(root, [h("p", { class: "empty", text: "Loading…" })]);
  let c, tl = null;
  try {
    c = await (offers("contexts") ? gate.context(service, caller, context) : gate.conversation(service, caller, context));
    if (offers("timeline")) tl = await gate.contextTimeline(service, caller, context);
  } catch (err) {
    if (mine === generation) fill(root, [failure(err, "This context")]);
    return () => {};
  }
  if (mine !== generation) return () => {};
  const tasks = Array.isArray(c.tasks) ? c.tasks : Array.isArray(c.items) ? c.items : [];
  // The context's two parties by name: its principal (the timeline's, else its latest task's) and its service.
  const parties = partiesOf((tl && tl.principal) || (tasks[0] && tasks[0].caller) || null, service);
  const parts = [
    h("div", { class: "card", id: "ctxopening" }, [h("div", { class: "bd stack" }, tl
      ? openingNodes(tl.opening, tl.callee_contexts, parties)
      : [idRow("contextId", context, { leg: legOf(parties, false) })])]),
    h("div", { class: "card", id: "ctxtasks" }, [h("div", { class: "hd" }, [h("h3", { text: "Tasks the agent created in it" })]),
      h("div", { class: "bd stack" }, [table(TASK_HEAD, tasks.map(taskRow), "No task in this context.")])]),
  ];
  if (tl) {
    const items = Array.isArray(tl.items) ? tl.items : [];
    parts.push(h("div", { class: "card", id: "ctxtimeline" }, [h("div", { class: "hd" }, [h("h3", { text: "Timeline" })]),
      h("div", { class: "bd stack timeline" }, items.length ? items.map((it) => timelineRow(it, { taskLinks: true, parties })) : [h("p", { class: "empty", text: "Nothing yet." })]),
      tl.more ? h("p", { class: "small muted", text: "Only the first 2000 items are shown." }) : null]));
  }
  fill(root, parts);
  return () => {};
}
