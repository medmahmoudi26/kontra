// The gate's tasks (#/, GET /v1/tasks): every caller's and the console's, both protocols, newest
// first, filtered by state, protocol or service. A task reads as its first words (the request's
// text), who called and how, and links to its thread.

import { gate } from "../app.js";
import { $, fill, h } from "../dom.js";
import { showView } from "../nav.js";
import { VIA_WORDS, ago, chip, failure, pager, shortId, stateChip, table } from "./common.js";

let generation = 0;
const filters = { state: "", kind: "", service: "" };

export function leaveTasks() {
  generation += 1;
  const root = $("#tasks");
  if (root) fill(root, []); // a hidden view keeps nothing of what it showed
}

/** A task's first words, short enough for its row (the whole in the link's title). */
const said = (text) => (text.length > 60 ? `${text.slice(0, 59)}…` : text);

/** One task's row: also the conversation view's. */
export function taskRow(t) {
  return h("tr", { dataset: { task: t.task_id } }, [
    h("td", {}, [h("span", { class: "tag", text: t.kind || "a2a" })]),
    h("td", {}, [t.kind === "a2a" || !t.kind ? h("a", { href: `#/run/${encodeURIComponent(t.service)}`, title: `Send a message to ${t.service}`, text: t.service }) : t.service]),
    h("td", {}, [
      h("a", { href: `#/t/${t.task_id}`, title: t.summary || t.task_id, text: t.summary ? said(t.summary) : shortId(t.task_id) }),
      t.summary ? h("span", { class: "muted mono", title: t.task_id, text: ` ${shortId(t.task_id)}` }) : null,
    ]),
    h("td", {}, [chip(stateChip(t.state))]),
    h("td", { class: "muted", text: [t.caller, VIA_WORDS[t.via] || t.via].filter(Boolean).join(" · ") || "—" }),
    h("td", { class: "muted", title: t.created_at || "", text: ago(t.created_at) }),
  ]);
}

export const TASK_HEAD = Object.freeze(["Kind", "Service", "Task", "State", "Principal", "Age"]);

function filterBar(onChange) {
  const sel = (key, label, options) => {
    const s = h("select", { class: "sel", "aria-label": label }, options.map(([v, t]) => h("option", { value: v, text: t })));
    s.value = filters[key];
    s.addEventListener("change", () => { filters[key] = s.value; onChange(); });
    return s;
  };
  const svc = h("input", { type: "text", placeholder: "service", "aria-label": "Service", spellcheck: "false", autocomplete: "off", value: filters.service });
  svc.addEventListener("change", () => { filters.service = svc.value.trim(); onChange(); });
  return h("div", { class: "row filters", id: "taskfilters" }, [
    sel("state", "State", [["", "any state"], ["submitted", "submitted"], ["working", "working"], ["input-required", "input required"], ["auth-required", "auth required"], ["completed", "completed"], ["failed", "failed"], ["canceled", "canceled"], ["rejected", "rejected"]]),
    sel("kind", "Protocol", [["", "both protocols"], ["a2a", "A2A"], ["mcp", "MCP"]]),
    svc,
  ]);
}

/** The #/ page. */
export async function showTasks() {
  showView("#view-tasks", "tasks");
  const root = $("#tasks");
  const mine = ++generation;
  const current = () => mine === generation;
  const bar = filterBar(() => showTasks());
  const list = pager(
    (before) => gate.tasks({ state: filters.state, kind: filters.kind, service: filters.service, before, limit: 50 }),
    () => {
      if (!current()) return;
      fill(root, [bar, list.error && !list.items.length
        ? failure(list.error, "The gate's tasks")
        : h("div", { class: "card", id: "taskscard" }, [h("div", { class: "bd stack" }, [
          table(TASK_HEAD, list.items.map(taskRow), "No task yet: a task any caller or this console starts shows here.", { id: "taskrows" }),
          h("div", { class: "row" }, [list.more]),
        ])])]);
    },
    current,
  );
  fill(root, [bar, h("p", { class: "empty", text: "Loading…" })]);
  await list.read(true);
  return () => {};
}
