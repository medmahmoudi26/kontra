// Conversations (#/conversations, GET /v1/conversations): the gate's tasks grouped by service,
// caller and context id (a context is scoped per caller), newest activity first; and one of them
// (#/c/<service>/<caller>/<context>), its tasks oldest first, each linking to its thread.

import { gate } from "../app.js";
import { $, fill, h } from "../dom.js";
import { showView } from "../nav.js";
import { TASK_HEAD, taskRow } from "./tasks.js";
import { ago, chip, failure, pager, shortId, stateChip, table } from "./common.js";

let generation = 0;

export function leaveConversations() {
  generation += 1;
  const root = $("#conversations");
  if (root) fill(root, []);
}

const convHref = (c) => `#/c/${encodeURIComponent(c.service)}/${encodeURIComponent(c.caller)}/${encodeURIComponent(c.context_id)}`;

function convRow(c) {
  return h("tr", { dataset: { context: c.context_id } }, [
    h("td", { text: c.service }),
    h("td", { class: "muted", text: c.caller || "—" }),
    h("td", {}, [h("a", { href: convHref(c), title: c.context_id, text: shortId(c.context_id) })]),
    h("td", { class: "m", text: String(c.tasks ?? "—") }),
    h("td", {}, [chip(stateChip(c.state))]),
    h("td", { class: "muted", title: c.last_at || "", text: ago(c.last_at) }),
  ]);
}

function heading(title, sub) {
  $("#conversationstitle").textContent = title;
  $("#conversationssub").textContent = sub;
}

/** #/conversations. */
export async function showConversations() {
  showView("#view-conversations", "conversations");
  heading("Conversations", "Tasks grouped by service, caller and context, newest activity first.");
  const root = $("#conversations");
  const mine = ++generation;
  const current = () => mine === generation;
  const list = pager(
    (before) => gate.conversations({ before, limit: 50 }),
    () => {
      if (!current()) return;
      fill(root, [list.error && !list.items.length
        ? failure(list.error, "The gate's conversations")
        : h("div", { class: "card", id: "convcard" }, [h("div", { class: "bd stack" }, [
          table(["Service", "Caller", "Context", "Tasks", "Latest", "Active"], list.items.map(convRow), "No conversation yet."),
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
export async function showConversation(service, caller, context) {
  showView("#view-conversations", "conversations", [shortId(context || "")]);
  heading(`${service || "?"} · ${caller || "?"}`, `Context ${context || "?"}: its tasks, oldest first.`);
  const root = $("#conversations");
  const mine = ++generation;
  if (!service || !caller || !context) {
    fill(root, [h("p", { class: "empty err", text: "That is not a conversation's address." })]);
    return () => {};
  }
  fill(root, [h("p", { class: "empty", text: "Loading…" })]);
  let c;
  try {
    c = await gate.conversation(service, caller, context);
  } catch (err) {
    if (mine === generation) fill(root, [failure(err, "This conversation")]);
    return () => {};
  }
  if (mine !== generation) return () => {};
  const tasks = Array.isArray(c.tasks) ? c.tasks : Array.isArray(c.items) ? c.items : [];
  fill(root, [h("div", { class: "card", id: "convtasks" }, [h("div", { class: "bd stack" }, [
    table(TASK_HEAD, tasks.map(taskRow), "No task in this conversation."),
  ])])]);
  return () => {};
}
