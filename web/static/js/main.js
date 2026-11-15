// The console's entry point: the top bar and the hash router. There is no login: the gate serves this
// console only in its local development profile, on loopback.
//
//   #/                               the gate's A2A contexts, newest activity first (the default; Tasks on a backend
//                                    without contexts); #/contexts and #/conversations too
//   #/c/<service>/<caller>/<context> one context: how it was opened, its tasks, its timeline
//   #/tasks                          the gate's tasks, newest first
//   #/t/<task>                       a task's thread, built live from its events
//   #/run/<service>                  send a message to an A2A service, from its card (a new conversation)
//   #/traffic                        the gate's A2A exchanges, both legs
//   #/x/<exchange>                   one exchange: its bodies or frames, validated
//   #/calls                          the gate's MCP tool calls
//   #/cards                          the agents' cards and whether one is reachable now (#/services too)
//   #/cards/<service>                a service's callee card and served card, side by side
//   #/play, #/play/<service>         the playground: any A2A method, on either binding
//
// Any other address shows the landing page.

import { loadInfo, offers, state } from "./app.js";
import { $, errorText, toast } from "./dom.js";
import { hideViews, navLinks } from "./nav.js";
import { leaveCards, showCardCompare, showCards } from "./views/agents.js";
import { leaveCalls, showCalls } from "./views/calls.js";
import { leaveContexts, showContext, showContexts } from "./views/contexts.js";
import { leaveExchange, showExchange } from "./views/exchange.js";
import { leavePlayground, showPlayground } from "./views/playground.js";
import { leaveTasks, showTasks } from "./views/tasks.js";
import { leaveThread, showRun, showThread, shownTask } from "./views/thread.js";
import { leaveTraffic, showTraffic } from "./views/traffic.js";

let dispose = () => {};
let routeSeq = 0; // the newest route() call: an older one that finishes later never shows

/** Leave every page: a stream or a timer of the page being left goes with it. */
function leaveAll() {
  leaveTasks();
  leaveContexts();
  leaveThread();
  leaveTraffic();
  leaveExchange();
  leaveCalls();
  leaveCards();
  leavePlayground();
}

const part = (s) => {
  try {
    return decodeURIComponent(s);
  } catch {
    return null;
  }
};

async function route() {
  // A new conversation's page became its task's (history.replaceState once it started), or a link to
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
  const exchange = /^#\/x\/([^/?#]+)$/.exec(hash);
  const compare = /^#\/cards\/([^/?#]+)$/.exec(hash);
  const play = /^#\/play(?:\/([^/?#]+))?$/.exec(hash);
  let next = null;
  try {
    if (hash === "#/conversations" || hash === "#/contexts") next = await showContexts();
    else if (conv) next = await showContext(part(conv[1]), part(conv[2]), part(conv[3]));
    else if (hash === "#/tasks") next = await showTasks();
    else if (thread) next = await showThread(thread[1].toLowerCase());
    else if (run) next = await showRun(part(run[1]));
    else if (hash === "#/traffic") next = await showTraffic();
    else if (exchange) next = await showExchange(part(exchange[1]));
    else if (hash === "#/calls") next = await showCalls();
    else if (hash === "#/cards" || hash === "#/services") next = await showCards();
    else if (compare) next = await showCardCompare(part(compare[1]));
    else if (play && offers("playground")) next = await showPlayground(play[1] ? part(play[1]) : null);
    else if (offers("contexts")) next = await showContexts();
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
