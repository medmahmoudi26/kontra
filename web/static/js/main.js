// The console's entry point: the top bar and the hash router. There is no login: the gate serves this
// console only in its local development profile, on loopback.
//
//   #/                               the gate's tasks, newest first (the default)
//   #/conversations                  tasks grouped by service, caller and context
//   #/c/<service>/<caller>/<context> one conversation: its tasks, oldest first
//   #/t/<task>                       a task's thread, built live from its events
//   #/run/<service>                  start a task on an A2A service, from its card
//   #/traffic                        the gate's A2A exchanges
//   #/calls                          the gate's MCP tool calls
//   #/services                       the gate's services and whether an agent is reachable now
//
// Any other address shows the tasks.

import { loadInfo, state } from "./app.js";
import { $, errorText, toast } from "./dom.js";
import { hideViews, navLinks } from "./nav.js";
import { leaveCalls, showCalls } from "./views/calls.js";
import { leaveConversations, showConversation, showConversations } from "./views/conversations.js";
import { leaveServices, showServices } from "./views/services.js";
import { leaveTasks, showTasks } from "./views/tasks.js";
import { leaveThread, showRun, showThread, shownTask } from "./views/thread.js";
import { leaveTraffic, showTraffic } from "./views/traffic.js";

let dispose = () => {};
let routeSeq = 0; // the newest route() call: an older one that finishes later never shows

/** Leave every page: a stream or a timer of the page being left goes with it. */
function leaveAll() {
  leaveTasks();
  leaveConversations();
  leaveThread();
  leaveTraffic();
  leaveCalls();
  leaveServices();
}

const part = (s) => {
  try {
    return decodeURIComponent(s);
  } catch {
    return null;
  }
};

async function route() {
  // A new task's page became that task's (history.replaceState once it started), or a link to
  // the task it shows was followed: the page stays as it is, never drawn again.
  const same = /^#\/t\/([^/?#]+)$/.exec(location.hash);
  if (same && shownTask() !== null && shownTask() === same[1].toLowerCase() && !$("#view-thread").hidden) return;
  const mine = ++routeSeq;
  dispose();
  dispose = () => {};
  leaveAll();
  hideViews();
  const hash = location.hash;
  const thread = /^#\/t\/([^/?#]+)$/.exec(hash);
  const run = /^#\/run\/([^/?#]+)$/.exec(hash);
  const conv = /^#\/c\/([^/?#]+)\/([^/?#]+)\/([^/?#]+)$/.exec(hash);
  let next = null;
  try {
    if (hash === "#/conversations") next = await showConversations();
    else if (conv) next = await showConversation(part(conv[1]), part(conv[2]), part(conv[3]));
    else if (thread) next = await showThread(thread[1].toLowerCase());
    else if (run) next = await showRun(part(run[1]));
    else if (hash === "#/traffic") next = await showTraffic();
    else if (hash === "#/calls") next = await showCalls();
    else if (hash === "#/services") next = await showServices();
    else next = await showTasks();
  } catch (err) {
    if (mine === routeSeq) toast(errorText(err));
  }
  if (mine !== routeSeq) {
    // Superseded while it loaded (the person clicked on): close what it opened.
    if (typeof next === "function") next();
    return;
  }
  if (typeof next === "function") dispose = next;
}

function initTopbar() {
  $("#themebtn").addEventListener("click", () => {
    const root = document.documentElement;
    const cur = root.getAttribute("data-theme");
    const dark = window.matchMedia("(prefers-color-scheme: dark)").matches;
    const now = cur || (dark ? "dark" : "light");
    root.setAttribute("data-theme", now === "dark" ? "light" : "dark");
  });
}

async function boot() {
  initTopbar();
  navLinks(null);
  window.addEventListener("hashchange", route);
  await loadInfo();
  const info = state.info;
  if (info && typeof info.profile === "string") {
    $("#devbadge").textContent = `${info.profile} gate`;
    $("#devbadge").hidden = false;
  }
  route();
  document.documentElement.dataset.ready = "1";
}

boot();
