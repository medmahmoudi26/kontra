// A task's page: from the request to what the agent delivers.
//
//   #/run/<service>   a new task on that service: the request card in the thread, nothing sent
//   #/t/<task>        a task's thread
//
// One page for both. Sending the request (POST /v1/tasks, which the console API sends to the gate's
// own A2A route as the shared caller) folds it and shows the Running card at once; the page then
// becomes the task's (`history.replaceState`, no redraw). The page is a sidebar (the agent, the
// task's live state, recent tasks of the service, its id and the event log) and one column of cards
// -- the request, Running (a live dot, the elapsed time, a log of what the agent says while it
// works, Cancel), the agent's questions with their forms, what it delivered, and a composer once it
// ended (Continue, Run again).
//
// **Built as the events come**: the page keeps the task as a model and each event of the stream
// (GET /v1/tasks/{id}/events, Server-Sent Events read with fetch) updates or appends only the nodes
// it is about. Nothing is drawn again, so what the person types, where their caret is and where
// they scrolled stay. The column follows new content while the person is at its bottom; otherwise a
// "New activity" pill says so. The task's metadata (GET /v1/tasks/{id}: its `actions`) is read
// again after an event that changes its state, at most once a second; nothing else polls.
//
// **The console acts only where the task's `actions` say so**: reply while it waits for input,
// cancel while it runs, continue once it ended -- on the tasks the console started. Every other task
// is watch-only: its questions show, its forms and A2UI surfaces are drawn inert, and no composer or
// Cancel is offered. An agent's question is answered in words, or in its form -- the A2A samples'
// form part and Structured Input's `input_schema` (one data part), an A2UI surface (its action;
// every reply names the catalogs the console draws) or AgentStack's form request (`form_response`).
// Results are kept by artifact id as A2A folds them; a file is read, opened and saved in the page,
// one the gate stores apart fetched through the console API. A part sealed end to end shows as a
// labelled envelope with its metadata, never opened. Cancel is A2A's CancelTask. While the agent's
// node is disconnected and the gate waits for it to come back (relay.md §6.4, D341: a status
// event's `kontra.reconnect`), the Running card says so in one warning line counted down to the
// wait's end, gone when the node is back.

import { CATALOGS, FormBook, actionMessage, actionReply, answerOf, formReply, surfacesOf, textReply } from "../a2uiwire.js";
import { collect, formSpec, refusal } from "../agentschema.js";
import { answeredValues, fulfilledValues, normalizeForm, requestedForm, valueRows } from "../agentstack.js";
import { agentForm } from "../agentform.js";
import { gate } from "../app.js";
import { GateError, errorFrom } from "../api.js";
import { $, errorText, fill, h, icon, toast } from "../dom.js";
import { fieldNode } from "../formfields.js";
import { crumb, showView } from "../nav.js";
import { readEvents } from "../sse.js";
import { FORM_MAX, describe, foldResult } from "../taskevents.js";
import { byCard, chipRow, enterSends, hm, hms, jsonBox, mss, oneLine, phone, plainText, rawToggle, reducedMotion } from "./cards.js";
import { VIA_WORDS, copyButton, failure, isTerminal, serviceAvatar, shortId, stateChip, when } from "./common.js";
import { dataView, fileRow, sealedRow, textView } from "./outputs.js";
import { requestCard, requestRow, requestRows } from "./request.js";
import { ap2Card } from "./ap2.js";

const TASK_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const NAME_RE = /^[a-z0-9][a-z0-9-]{0,62}$/;
/** A surface the person answered: its id in one generation of js/a2uiwire.js FormBook. */
const sentKey = (id, gen) => `${gen}:${id}`;
/** Seconds of silence from the agent before the Running card says so. */
export const QUIET_SECONDS = 15;
/** The service's recent tasks the sidebar lists. */
export const RUNS_MAX = 8;
/** A short label and ": " at the start of a run's summary (its first line). */
const LABEL_RE = /^[^:\r\n]{1,40}: (?=\S)/;

/** A task's summary as "Recent tasks" says it: a task started through a form is summarised as the
 * form's "Label: value" lines, so every task of it would start with the same label; the first
 * line's label goes, and its value tells the tasks apart. The row's title keeps the whole summary. */
export const runSummary = (summary) => String(summary || "").replace(LABEL_RE, "");
/** How long what the person clicked in the thread stays put while the thread grows. */
const ANCHOR_MS = 1500;
/** How long after the stream opens its events are the task's history, read at once. */
const REPLAY_MS = 1500;
const LIVE = new Set(["submitted", "working"]);
/** Whether the agent is at work in `state` (none yet: it starts so). */
const flowing = (state) => LIVE.has(state || "submitted");

let generation = 0;
let stop = () => {};
/** The task the page shows (null on a new task): main.js does not rebuild it for its own hash. */
let shown = null;
/** A task a Continue started -> the task it continues (this tab only). */
const continued = new Map();
/** What a Run again or a Continue sent, for the new task's page to show at once. */
let handoff = null;

export const shownTask = () => shown;

export function leaveThread() {
  generation += 1;
  shown = null;
  stop();
  stop = () => {};
  for (const id of ["#thread", "#threadctx"]) {
    const n = $(id);
    if (n) fill(n, []); // a hidden view keeps nothing of what it showed
  }
}

const textOf = (m) => (m ? m.parts.filter((p) => p.text !== undefined).map((p) => p.text).join("\n").trim() : "");
const onlyText = (m) => m.parts.every((p) => p.text !== undefined);

/** A flat object's names and values. */
function rowsNode(rows) {
  const node = h("div", { class: "terms2 kv" }, rows.map(([k, v]) => h("div", { class: "tr" }, [h("b", { text: k }), h("span", { text: v })])));
  node.style.maxWidth = "560px";
  return node;
}

// ------------------------------------------------------------------ the stream

/** Follow the task's events from seq `after` until it ends or `signal` aborts: reconnects after a
 * stream's natural end (a few minutes) or a broken one (the last seq seen as `after`); stops on a
 * refusal (404: the task is gone) and the "end" event. */
async function follow(taskId, after, signal, { onOpen, onEvent, onEnd, onTrouble }) {
  let last = after;
  let wait = 1000;
  while (!signal.aborted) {
    let resp;
    try {
      resp = await gate.taskEvents(taskId, { after: last, signal });
    } catch (err) {
      if (signal.aborted) return;
      onTrouble(errorText(err));
      await new Promise((r) => setTimeout(r, wait));
      wait = Math.min(wait * 2, 15000);
      continue;
    }
    if (!resp.ok) {
      const err = await errorFrom(resp);
      if (err.status === 429) {
        await new Promise((r) => setTimeout(r, 5000));
        continue;
      }
      onTrouble(errorText(err));
      return;
    }
    wait = 1000;
    onOpen();
    try {
      for await (const ev of readEvents(resp.body, { signal })) {
        if (ev.event === "end") {
          onEnd(JSON.parse(ev.data));
          return;
        }
        let data;
        try {
          data = JSON.parse(ev.data);
        } catch {
          continue;
        }
        if (typeof data.seq === "number" && data.seq > last) {
          last = data.seq;
          onEvent(data);
        }
      }
    } catch (err) {
      if (signal.aborted) return;
      onTrouble(errorText(err));
      await new Promise((r) => setTimeout(r, wait));
    }
  }
}

// ------------------------------------------------------------------ the routes

/** #/run/<service>: a new task on that service. `prefill`: { text, data, values }. */
export async function showRun(name, { prefill = null } = {}) {
  leaveThread();
  const mine = ++generation;
  showView("#view-thread", "services", [typeof name === "string" ? name : "?"]);
  if (typeof name !== "string" || !NAME_RE.test(name)) {
    fill($("#thread"), [h("p", { class: "empty err", text: "That is not a service's address." })]);
    return () => {};
  }
  const page = openPage(mine, { name, taskId: null });
  await page.newRun(prefill);
  return () => {};
}

/** #/t/<task>: a task's thread. */
export async function showThread(taskId) {
  leaveThread();
  const mine = ++generation;
  showView("#view-thread", "tasks", shortId(taskId));
  const root = $("#thread");
  if (!TASK_RE.test(taskId)) {
    fill(root, [h("p", { class: "empty err", text: "That is not a task's address." })]);
    return () => {};
  }
  shown = taskId;
  const ho = handoff && handoff.taskId === taskId ? handoff : null;
  handoff = null;
  if (ho) {
    // Sent from the page before (Run again, Continue): drawn at once, read in the background.
    const page = openPage(mine, { name: ho.name, taskId, from: ho.from });
    page.started(ho);
    page.attach(null);
    return () => {};
  }
  fill(root, [h("p", { class: "empty", text: "Loading…" })]);
  let t;
  try {
    t = await gate.task(taskId);
  } catch (err) {
    if (mine !== generation) return () => {};
    fill(root, [err instanceof GateError && (err.status === 404 || err.code === "task_not_found")
      ? h("div", { class: "card", id: "notfound" }, [h("div", { class: "bd stack" }, [h("p", { class: "lead", text: "No such task: it does not exist, or retention deleted it." })])])
      : failure(err, "This task and its events")]);
    return () => {};
  }
  if (mine !== generation) return () => {};
  const page = openPage(mine, { name: t.service, taskId, kind: t.kind || "a2a", from: continued.get(taskId) || null });
  page.attach(t);
  return () => {};
}

// ------------------------------------------------------------------ the page

function openPage(mine, { name, taskId, kind = "a2a", from = null }) {
  const ref = name;
  const who = { ref, kind, name };
  const root = $("#thread");
  const side = $("#threadctx");
  const ctrl = new AbortController();
  const alive = () => mine === generation;
  const P = {
    taskId,
    t: null,
    form: null,
    formP: null,
    state: null,
    createdAt: null,
    terminalAt: null,
    lastEventAt: Date.now(),
    /** While the agent's node is disconnected and the gate waits for it (D341): when the
     * wait ends (ms), from the status event's `kontra.reconnect`; null otherwise. */
    reconnectUntil: null,
    replayUntil: 0,
    created: false,
    wait: null,
    events: 0,
    req: null,
    live: null,
    resultsCard: null,
    results: new Map(),
    inputs: [],
    ended: false,
    canceling: false,
    first: null,
    resultCount: 0,
    metaKey: { composer: null },
  };

  // ---- The column: the cards, then the composer.
  const list = h("div", { class: "stack", id: "messages" });
  list.style.gap = "14px";
  const tail = h("div", { class: "stack", id: "tail" });
  tail.style.gap = "14px";
  const composerSlot = h("div", { class: "stack" });
  const againSlot = h("div", { class: "stack" });
  tail.append(composerSlot, againSlot);
  const head = from
    ? h("p", { class: "small", id: "continues" }, ["Continues ", h("a", { href: `#/t/${from}`, text: shortId(from) })])
    : null;
  fill(root, [head, list, tail]);
  const scroller = root.closest(".thread-scroll");
  const column = root.closest(".thread");

  // ---- Following new content, or saying it came.
  const pill = h("button", { class: "btn sm primary", type: "button", id: "newactivity", text: "New activity ↓", hidden: true });
  pill.style.position = "absolute";
  pill.style.left = "50%";
  pill.style.bottom = "18px";
  pill.style.transform = "translateX(-50%)";
  pill.style.boxShadow = "var(--shadow-lg)";
  pill.style.zIndex = "6";
  if (column) {
    column.style.position = "relative";
    column.appendChild(pill);
  }
  const atBottom = () => !scroller || scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight < 80;
  const toBottom = () => {
    if (!scroller) return;
    scroller.style.scrollBehavior = "auto";
    scroller.scrollTop = scroller.scrollHeight;
    scroller.style.scrollBehavior = "";
    pill.hidden = true;
  };
  pill.addEventListener("click", toBottom);
  // Following: while the person is at the bottom, the thread stays there whatever grows
  // -- a new card, a line of the Running card, a result's next chunk, a history drawn at once --
  // and once they scrolled up it stays where they are, a new card saying "New activity".
  // Only the person lets go of the bottom (a wheel, a touch, a key, the scrollbar): a scroll the
  // page makes, or the browser's when a card shrinks, never does.
  let pinned = true;
  let intent = 0;
  const meant = () => { intent = Date.now(); };
  const onScroll = () => {
    if (atBottom()) {
      pinned = true;
      pill.hidden = true;
    } else if (Date.now() - intent < 1500) pinned = false;
  };
  const MEANT = ["wheel", "touchmove", "pointerdown"];
  if (scroller) {
    scroller.addEventListener("scroll", onScroll, { passive: true });
    for (const ev of MEANT) scroller.addEventListener(ev, meant, { passive: true });
  }
  document.addEventListener("keydown", meant);
  // What the person clicked in the thread stays in view while what the click opened grows
  //: a "{}", a log, "Show all" near the bottom open under it -- the thread follows only
  // as far as the clicked control stays on screen -- instead of the control being followed off the
  // top. A click whose target goes (Send, Run, Cancel) is followed as before.
  let anchor = null;
  const clicked = (e) => {
    const n = e.target instanceof Element ? e.target : null;
    anchor = n ? { node: n, top: n.getBoundingClientRect().top, until: Date.now() + ANCHOR_MS } : null;
  };
  root.addEventListener("click", clicked, true);
  const held = () => {
    if (!anchor || Date.now() > anchor.until || !anchor.node.isConnected || !anchor.node.getClientRects().length) return false;
    if (!scroller) return true;
    let by = anchor.node.getBoundingClientRect().top - anchor.top; // back where it was
    if (pinned) {
      const rest = scroller.scrollHeight - scroller.clientHeight - (scroller.scrollTop + by);
      const room = anchor.top - scroller.getBoundingClientRect().top - 12;
      by += Math.max(0, Math.min(rest, room));
    }
    if (by) {
      scroller.style.scrollBehavior = "auto";
      scroller.scrollTop += by;
      scroller.style.scrollBehavior = "";
    }
    anchor.top = anchor.node.getBoundingClientRect().top; // a later growth keeps it there
    pinned = atBottom();
    return true;
  };
  const keeper = typeof ResizeObserver === "function" ? new ResizeObserver(() => { if (!held() && pinned) toBottom(); }) : null;
  if (keeper) keeper.observe(root);
  /** Cards that follow one another from one sender share its name line: the first of
   * them says who and when, as a chat does. Kept true as cards come, go, show and hide. */
  const regroup = () => {
    let prev = null;
    for (const n of list.children) {
      if (n.hidden) continue;
      if (!n.classList.contains("msg")) {
        if (n.childElementCount) prev = null;
        continue;
      }
      const by = n.querySelector(":scope > .by");
      const said = by ? `${n.classList.contains("you") ? "you" : "svc"}:${(by.querySelector("b") || {}).textContent || ""}` : null;
      const same = said !== null && said === prev;
      if (by && by.hidden !== same) by.hidden = same;
      prev = said;
    }
  };
  new MutationObserver(regroup).observe(list, { childList: true, subtree: true, attributes: true, attributeFilter: ["hidden"] });
  /** Run `change` (it adds to the page), then follow it or say it came. */
  const grew = (change) => {
    const stick = pinned;
    const out = change();
    if (stick) {
      pinned = true;
      if (!keeper) requestAnimationFrame(toBottom);
    } else pill.hidden = false;
    return out;
  };

  // ------------------------------------------------------------------ the sidebar

  const dot = h("span", { class: "dot idle" });
  const chipText = h("span", { text: "new task" });
  const chipTime = h("span", { class: "mono" });
  const chip = h("span", { class: "statechip", id: "threadstate", dataset: { state: "new" } }, [dot, chipText, chipTime]);
  const plainChip = () => {
    chip.className = "statechip";
    chip.style.background = "var(--surface-2)";
    chip.style.color = "var(--ink-3)";
  };
  plainChip();
  const blurb = h("p", { class: "blurb", id: "ctxblurb" });
  blurb.style.margin = "0";
  blurb.style.setProperty("display", "-webkit-box");
  blurb.style.setProperty("-webkit-box-orient", "vertical");
  blurb.style.setProperty("-webkit-line-clamp", "3");
  blurb.style.overflow = "hidden";
  const blurbMore = h("button", { class: "linkbtn", type: "button", text: "more", hidden: true });
  blurbMore.style.paddingLeft = "0";
  blurbMore.style.alignSelf = "flex-start";
  blurbMore.addEventListener("click", () => {
    const open = blurb.style.getPropertyValue("-webkit-line-clamp") !== "unset";
    blurb.style.setProperty("-webkit-line-clamp", open ? "unset" : "3");
    blurbMore.textContent = open ? "less" : "more";
  });
  const about = h("div", { class: "ctxsec", id: "ctxabout", hidden: true }, [blurb, blurbMore]);
  const runsList = h("div", { class: "ctxruns", id: "ctxruns" });
  const runs = h("div", { class: "ctxsec", id: "recenttasks", hidden: true }, [h("h3", { text: "Recent tasks" }), runsList]);
  const runHref = `#/run/${encodeURIComponent(name)}`;
  const newRun = h("a", { class: "newbtn", id: "newrun", href: runHref }, [icon("plus", 13), "New task"]);
  const newSec = h("div", { class: "ctxsec", hidden: kind !== "a2a" || !taskId }, [newRun]);
  const newSmall = h("a", { class: "btn sm", id: "newrunsm", href: runHref, text: "New task", hidden: true });
  newSmall.style.textDecoration = "none";
  const footTerms = h("div", { class: "terms2", id: "ctxfoot" });
  const logCount = h("span", { text: "Event log" });
  const log = h("div", { class: "events", id: "eventlog" });
  log.style.marginTop = "8px";
  const logBox = h("details", { class: "keyimport", id: "eventsbox" }, [h("summary", {}, [logCount]), log]);
  logBox.style.marginTop = "12px";
  const trouble = h("p", { class: "small", id: "streamtrouble", hidden: true });
  const foot = h("div", { class: "ctxsec", id: "ctxtask", hidden: true }, [h("h3", { text: "Task" }), footTerms, logBox, trouble]);
  const sideHead = h("div", { class: "svcid" }, [
    serviceAvatar(ref, kind, 44),
    h("div", { class: "minw0" }, [
      h("div", { class: "nm" }, [h("span", { id: "ctxname", text: name }), h("span", { class: "v", text: kind })]),
    ]),
  ]);
  fill(side, [sideHead, chip, newSmall, about, runs, newSec, foot]);
  const narrow = typeof matchMedia === "function" ? matchMedia("(max-width:1080px)") : null;
  const sideName = $("#ctxname", sideHead);
  const fitSide = () => {
    const small = Boolean(narrow && narrow.matches);
    newSmall.hidden = !small || newSec.hidden;
    // On a narrow screen the agent's name, its state and New run share one line: the
    // name gives way, cut with an ellipsis, rather than pushing New run under it.
    sideHead.style.flex = small ? "1 1 0" : "";
    sideHead.style.minWidth = small ? "0" : "";
    sideHead.lastElementChild.style.minWidth = small ? "0" : "";
    sideName.parentElement.style.flexWrap = small ? "nowrap" : "";
    if (sideName.nextElementSibling) sideName.nextElementSibling.hidden = small;
    for (const [k, v] of [["overflow", "hidden"], ["textOverflow", "ellipsis"], ["whiteSpace", "nowrap"], ["minWidth", "0"]]) sideName.style[k] = small ? v : "";
  };
  if (narrow) narrow.addEventListener("change", fitSide);
  fitSide();

  const setBlurb = (text) => {
    blurb.textContent = text || "";
    about.hidden = !text;
    requestAnimationFrame(() => { blurbMore.hidden = !(blurb.scrollHeight > blurb.clientHeight + 2); });
  };

  /** The state the page shows: the events', else the task's. */
  const shownState = () => P.state || (P.t ? P.t.state : null);
  const drawChip = () => {
    const s = shownState();
    if (!s) return;
    const live = LIVE.has(s) && !P.ended;
    const cls = live || s === "completed" ? "online" : isTerminal(s) ? "offline" : "busy";
    chip.className = `statechip ${cls}`;
    chip.style.background = "";
    chip.style.color = "";
    chip.dataset.state = s;
    // Only a live run's dot pulses (console.css's .dot.online); a finished one is still.
    dot.className = live ? `dot ${reducedMotion() ? "busy" : "online"}` : `dot ${s === "completed" ? "idle" : isTerminal(s) ? "offline" : "busy"}`;
    dot.style.background = !live && s === "completed" ? "var(--ok)" : "";
    chipText.textContent = s === "submitted" && !P.taskId ? "starting" : stateChip(s)[0];
    chipTime.hidden = !live;
    const cur = runsList.querySelector("[aria-current=page] .jchip");
    if (cur) {
      const [w, c] = stateChip(s);
      cur.className = `jchip ${c}`;
      cur.textContent = w;
    }
  };
  const footRows = () => {
    const t = P.t;
    if (!t) return;
    foot.hidden = false;
    const idRow = h("div", { class: "tr" }, [h("b", { text: "id" }), h("span", {}, [shortId(t.task_id), " "])]);
    const copy = copyButton(t.task_id, "copy");
    copy.className = "linkbtn";
    idRow.lastChild.appendChild(copy);
    fill(footTerms, [
      idRow,
      h("div", { class: "tr" }, [h("b", { text: "started" }), h("span", { text: when(t.created_at) })]),
      h("div", { class: "tr" }, [h("b", { text: "duration" }), h("span", { id: "ctxduration", text: "" })]),
      h("div", { class: "tr" }, [h("b", { text: "caller" }), h("span", { text: [t.caller, VIA_WORDS[t.via] || t.via].filter(Boolean).join(" · ") || "—" })]),
      t.context_id ? h("div", { class: "tr" }, [h("b", { text: "context" }), h("span", { title: t.context_id, text: shortId(t.context_id) })]) : null,
    ]);
    tick();
  };

  const runRow = (item, current) => {
    const [w, c] = stateChip(item.state);
    const said = runSummary(item.summary);
    const row = h("a", { class: "ctxrun", href: `#/t/${item.task_id}`, dataset: { task: item.task_id }, title: item.summary || shortId(item.task_id) }, [
      h("span", { class: `jchip ${c}`, text: w }),
      h("span", { class: "minw0", text: said ? said.slice(0, 40) : shortId(item.task_id) }),
      h("span", { class: "id", text: hm(item.created_at) }),
    ]);
    row.children[1].style.overflow = "hidden";
    row.children[1].style.textOverflow = "ellipsis";
    row.children[1].style.whiteSpace = "nowrap";
    if (current) {
      row.setAttribute("aria-current", "page");
      row.style.borderColor = "var(--brand)";
      row.style.background = "var(--brand-soft)";
    }
    return row;
  };
  let runItems = [];
  const drawRuns = () => {
    const items = runItems.slice(0, RUNS_MAX);
    runs.hidden = !items.length;
    fill(runsList, items.map((x) => runRow(x, x.task_id === P.taskId)));
  };
  const loadRuns = () => {
    if (kind !== "a2a") return;
    gate.tasks({ service: name, limit: RUNS_MAX }).then((page) => {
      if (!alive()) return;
      const mineOnly = (page.items || []).filter((x) => x.service === name);
      const known = new Set(mineOnly.map((x) => x.task_id));
      runItems = [...runItems.filter((x) => !known.has(x.task_id) && x.local), ...mineOnly];
      drawRuns();
      drawChip();
    }).catch(() => {
      if (alive()) runs.hidden = true;
    });
  };

  const logLine = (ev, d) => {
    P.events += 1;
    logCount.textContent = `Event log (${P.events})`;
    const at = log.scrollHeight - log.scrollTop - log.clientHeight < 8;
    const row = h("div", { class: "ev" }, [h("time", { text: hms(ev.at) }), h("p", { text: d.line })]);
    row.style.gridTemplateColumns = "56px minmax(0,1fr)";
    log.appendChild(row);
    if (at) log.scrollTop = log.scrollHeight;
  };

  // ------------------------------------------------------------------ the clock

  const tick = () => {
    const now = Date.now();
    const from = P.createdAt || (P.live ? P.live.startedAt : null);
    const end = P.terminalAt || now;
    const s = shownState();
    const live = s && LIVE.has(s) && !P.ended;
    if (from) chipTime.textContent = live ? mss((now - from) / 1000) : "";
    const dur = $("#ctxduration", side);
    if (dur && from) dur.textContent = mss((end - from) / 1000);
    // The card counts its turn (as it does once settled), the sidebar's chip the whole run.
    if (P.live && !P.live.done) P.live.tick(now, P.live.startedAt);
  };
  const ticker = setInterval(tick, 1000);

  // ------------------------------------------------------------------ cards

  /** Add a card to the thread: before the live Running card while it is the last one. */
  const place = (node) => grew(() => {
    if (P.live && !P.live.done && P.live.node.parentElement === list && P.live.node === list.lastElementChild) list.insertBefore(node, P.live.node);
    else list.appendChild(node);
  });

  /** The Running card: a live dot, Starting or Working, the elapsed time, a bar, the agent's words
   * while it works, the silence counted after QUIET_SECONDS, Cancel. Settled when the turn ends: one
   * line, its log behind a toggle. */
  function liveCard(at, phase) {
    const startedAt = at ? Date.parse(at) || Date.now() : Date.now();
    const d = h("span", { class: reducedMotion() ? "dot busy" : "dot online" });
    const title = h("h3", { text: phase === "working" ? "Working" : "Starting" });
    const elapsed = h("span", { class: "r", text: "0:00" });
    const actSlot = h("span", { class: "row" });
    actSlot.style.marginLeft = "10px";
    const fillBar = h("i", { class: reducedMotion() ? "" : "indet" });
    if (reducedMotion()) {
      fillBar.style.width = "100%";
      fillBar.style.opacity = ".35";
      fillBar.style.background = "var(--brand)";
    }
    // The bar runs under the header, the card's width: the log is the body.
    const bar = h("div", { class: "bar" }, [fillBar]);
    bar.style.height = "2px";
    bar.style.borderRadius = "0";
    const lines = h("div", { class: "events", dataset: { log: "1" }, hidden: true });
    lines.style.marginTop = "0";
    lines.style.borderTop = "0";
    const quiet = h("p", { class: "small", dataset: { quiet: "1" }, hidden: true });
    quiet.style.marginTop = "6px";
    // The node is gone and the gate waits for it: one warning line, counted down.
    const away = h("p", { class: "warnline", dataset: { reconnect: "1" }, hidden: true });
    away.style.margin = "8px 15px 0";
    const bd = h("div", { class: "bd" }, [lines, quiet]);
    bd.style.padding = "6px 15px 10px";
    const hd = h("div", { class: "hd" }, [d, title, elapsed, actSlot]);
    hd.style.borderBottom = "0";
    const card = h("div", { class: "card", id: "live", dataset: { live: "running" } }, [hd, bar, away, bd]);
    const fitBody = () => { bd.hidden = lines.hidden && quiet.hidden; };
    fitBody();
    const msg = byCard(who, at || new Date().toISOString(), card);
    const c = {
      node: msg.node,
      startedAt,
      done: false,
      lines: 0,
      last: null,
      lastAt: Date.now(),
      phase(p) {
        if (c.done) return;
        title.textContent = p === "working" ? "Working" : "Starting";
        card.dataset.live = p === "working" ? "working" : "running";
      },
      log(when, text) {
        const atEnd = lines.scrollHeight - lines.scrollTop - lines.clientHeight < 8;
        const row = h("div", { class: "ev" }, [h("time", { text: hms(when) }), oneLine(text)]);
        row.style.gridTemplateColumns = "58px minmax(0,1fr)";
        lines.hidden = false;
        lines.appendChild(row);
        c.lines += 1;
        c.last = text;
        fitBody();
        if (atEnd) lines.scrollTop = lines.scrollHeight;
      },
      tick(now, from) {
        elapsed.textContent = mss((now - from) / 1000);
        const silent = (now - P.lastEventAt) / 1000;
        const waiting = P.reconnectUntil !== null;
        away.hidden = !waiting;
        d.className = waiting || reducedMotion() ? "dot busy" : "dot online";
        if (waiting) away.textContent = `Node disconnected · reconnecting · ${mss(Math.max(0, (P.reconnectUntil - now) / 1000))} left`;
        quiet.hidden = waiting || !(P.taskId && silent >= QUIET_SECONDS);
        if (!quiet.hidden) quiet.textContent = `No update from the agent for ${mss(silent)}`;
        fitBody();
      },
      cancel(can) {
        if (c.done || P.canceling) return;
        if (!can) {
          actSlot.replaceChildren();
          return;
        }
        if (actSlot.childElementCount) return;
        const btn = h("button", { class: "btn sm", type: "button", id: "cancelrun", text: "Cancel" });
        btn.addEventListener("click", () => {
          const yes = h("button", { class: "btn sm danger", type: "button", id: "cancelyes", text: "Yes" });
          const no = h("button", { class: "btn sm", type: "button", text: "No" });
          const ask = h("span", { class: "small", text: "Cancel this task?" });
          actSlot.replaceChildren(ask, yes, no);
          no.addEventListener("click", () => actSlot.replaceChildren(btn));
          yes.addEventListener("click", () => cancelRun(actSlot, btn));
        });
        actSlot.replaceChildren(btn);
      },
      say(text) {
        actSlot.replaceChildren(h("span", { class: "small", dataset: { cancelnote: "1" }, text }));
      },
      settle(when, state) {
        if (c.done) return;
        c.done = true;
        const end = when ? Date.parse(when) || Date.now() : Date.now();
        card.dataset.live = "done";
        card.removeAttribute("id");
        d.className = state === "failed" || state === "rejected" || state === "canceled" ? "dot offline" : "dot idle";
        title.textContent = "Worked";
        elapsed.textContent = mss((end - startedAt) / 1000);
        bar.remove();
        quiet.remove();
        away.remove();
        actSlot.replaceChildren();
        if (!c.lines) {
          // A turn the agent said nothing in (it asked at once, or answered): the card that was
          // its working indicator says nothing more than the cards around it, so it goes.
          msg.node.remove();
          return;
        }
        lines.hidden = true;
        bd.hidden = true;
        const toggle = h("button", { class: "linkbtn", type: "button", text: `${c.lines} update${c.lines === 1 ? "" : "s"}`, dataset: { act: "log" } });
        toggle.addEventListener("click", () => {
          lines.hidden = !lines.hidden;
          bd.hidden = lines.hidden;
          hd.style.borderBottom = lines.hidden ? "0" : "";
        });
        title.after(toggle);
      },
      remove() {
        c.done = true;
        msg.node.remove();
      },
    };
    return c;
  }

  const ensureLive = (at, phase) => {
    if (P.live && !P.live.done) {
      P.live.phase(phase);
      return P.live;
    }
    P.live = liveCard(at, phase);
    P.resultsCard = null; // a new turn's results come under it
    grew(() => list.appendChild(P.live.node));
    P.live.cancel(Boolean(P.t && P.t.actions.includes("cancel")));
    tick();
    return P.live;
  };
  const settleLive = (at, state) => {
    if (P.live && !P.live.done) P.live.settle(at, state);
  };

  async function cancelRun(slot, btn) {
    P.canceling = true;
    slot.replaceChildren(h("span", { class: "small", text: "Canceling…" }));
    try {
      const r = await gate.cancelTask(P.taskId);
      if (!alive()) return;
      if (r && r.state && isTerminal(r.state)) scheduleRefresh();
    } catch (e) {
      if (!alive()) return;
      P.canceling = false;
      const words = e instanceof GateError && e.code === "task_not_cancelable" ? "Not canceled: it ended, or the agent refused." : errorText(e);
      slot.replaceChildren(h("span", { class: "small", dataset: { cancelnote: "1" }, text: words }), btn);
    }
  }

  // ---- The results of a turn, then what it delivered.

  function resultsCard(at) {
    const ic = icon("download", 14);
    const title = h("h3", { text: "Results" });
    const right = h("span", { class: "r", text: "" });
    const hd = h("div", { class: "hd" }, [ic, title, right]);
    const secs = h("div", {});
    const last = h("div", { class: "bd topline", hidden: true, dataset: { final: "1" } });
    last.style.padding = "12px 15px";
    const card = h("div", { class: "card", dataset: { results: "1" } }, [hd, secs, last]);
    const msg = byCard(who, at, card);
    const c = {
      node: msg.node,
      card,
      secs,
      add(node) { grew(() => secs.appendChild(node)); },
      receiving(on) { if (!c.delivered) right.textContent = on ? "receiving…" : ""; },
      deliver(when, m, artifactTexts) {
        c.delivered = true;
        card.dataset.delivered = "1";
        card.id = "delivered";
        const check = icon("check", 15);
        check.style.color = "var(--ok)";
        ic.replaceWith(check);
        title.textContent = "Delivered";
        const end = Date.parse(when) || Date.now();
        const from = P.createdAt || end;
        right.textContent = `in ${mss((end - from) / 1000)}`;
        msg.time.textContent = hm(when);
        if (m) {
          // The agent's last words, once: not again when they repeat a result's text.
          const parts = m.parts.filter((p) => p.text === undefined || !artifactTexts.has(p.text.trim()));
          if (parts.length) {
            last.hidden = false;
            grew(() => last.append(...partNodes(parts, "svc", partCtx).filter(Boolean)));
          }
        }
        if (!secs.childElementCount && last.hidden) hd.style.borderBottom = "0";
      },
    };
    return c;
  }
  const ensureResults = (at) => {
    if (P.resultsCard) return P.resultsCard;
    P.resultsCard = resultsCard(at);
    grew(() => list.appendChild(P.resultsCard.node));
    return P.resultsCard;
  };

  /** One result's section: its name, "receiving" while its chunks come, its "{}", its parts. */
  const drawResult = (r) => {
    const receiving = r.chunked && r.lastChunk !== true && flowing(P.state);
    const raw = rawToggle(`${r.name}: as the agent sent it`, () => r.raw);
    const files = r.parts.filter((p) => p.file);
    const fileOnly = files.length === r.parts.length && files.length > 0;
    const ctx = { taskId: P.taskId, signal: ctrl.signal };
    const nodes = [];
    for (const part of r.parts) {
      if (part.file) nodes.push(fileRow(part.file, ctx, { under: r.name, receiving, extra: fileOnly && part === files[files.length - 1] ? [raw.button] : [] }));
      else if (part.text !== undefined) nodes.push(textView(part, r.name));
      else if (part.data !== undefined && !part.a2ui && !part.form && !part.actions) nodes.push(dataView(part.data));
      else nodes.push(...partNodes([part], "svc", partCtx));
    }
    const inner = h("div", { class: "in stack" }, [...nodes, raw.box]);
    inner.style.gap = "8px";
    const header = fileOnly ? null : h("header", {}, [
      h("h4", { text: r.name }),
      receiving ? h("span", { class: "tag", text: "receiving" }) : null,
      h("span", { class: "ty", text: r.parts.some((p) => p.data !== undefined) ? "data" : "text" }),
      raw.button,
    ]);
    // Files one after another read as one list: no rule and 8px between their rows.
    const prev = r.node.previousElementSibling;
    const joined = fileOnly && Boolean(prev && prev.dataset.fileonly === "1");
    if (fileOnly) {
      r.node.dataset.fileonly = "1";
      inner.style.padding = joined ? "0 15px 8px" : "10px 15px 8px";
    } else delete r.node.dataset.fileonly;
    r.node.style.borderTop = joined ? "0" : "";
    r.node.replaceChildren(...[header, inner].filter(Boolean));
    // The card this result is in says "receiving…" while one of its own results is: a file that
    // began in an earlier turn (an appended log) ends there, not in the turn that is drawn now.
    if (r.card) r.card.receiving([...P.results.values()].some((x) => x.card === r.card && x.chunked && x.lastChunk !== true && flowing(P.state)));
  };
  const addArtifact = (a, at) => {
    const known = a.id !== null && P.results.get(a.id);
    if (!known) {
      const card = ensureResults(at);
      const node = h("section", { class: "sec", dataset: { result: a.id || "" } });
      const r = { node, card, id: a.id, raw: [...a.source], ...foldResult(null, a) };
      if (a.id !== null) P.results.set(a.id, r);
      P.resultCount += 1;
      card.add(node);
      drawResult(r);
      return;
    }
    Object.assign(known, foldResult(known, a));
    known.raw = a.append ? [...known.raw, ...a.source] : [...a.source];
    drawResult(known);
  };
  const artifactTexts = () => {
    const out = new Set();
    for (const r of P.results.values()) for (const p of r.parts) if (p.text !== undefined && p.text.trim()) out.add(p.text.trim());
    return out;
  };

  // ---- The ends.

  const END_WORDS = { failed: "Failed", rejected: "Rejected", canceled: "Canceled" };
  const endCard = (state, m, at) => {
    const parts = m ? m.parts : [];
    const body = parts.length
      ? h("div", { class: "bd stack" }, partNodes(parts, "svc", partCtx))
      : h("div", { class: "bd" }, [h("p", { class: "plain", text: state === "canceled" && P.canceling ? "Canceled from this console." : state === "canceled" ? "Canceled." : "The agent said nothing more." })]);
    body.style.padding = "11px 15px";
    const end = Date.parse(at) || Date.now();
    const card = h("div", { class: "card", id: "ended", dataset: { end: state } }, [
      h("div", { class: "hd" }, [h("span", { class: "dot offline" }), h("h3", { text: END_WORDS[state] || state }), h("span", { class: "r", text: P.createdAt ? `after ${mss((end - P.createdAt) / 1000)}` : "" })]),
      body,
    ]);
    place(byCard(who, at, card).node);
  };

  // ------------------------------------------------------------------ the agent's questions

  const forms = []; // every form drawn on the thread, in order: the newest one takes the answer
  let lastSent = {};
  const ui = { load: null, surfaces: null, max: 0, queue: Promise.resolve(), sending: false, boxes: new Map(), book: new FormBook(), answered: new Set(), gone: [] };
  const canReply = () => Boolean(P.t && P.t.actions.includes("reply"));

  /** What answers the newest question now, and the words box of each question. */
  const liven = () => {
    const open = canReply() && !ui.sending;
    for (const [id, b] of ui.boxes) {
      const sent = ui.answered.has(sentKey(id, b.gen));
      b.node.inert = !open;
      b.note.hidden = open || ui.sending || (!sent && ui.book.takesInput(id, b.gen) === false);
      b.note.textContent = sent ? "Form sent." : "Takes no input now.";
    }
    forms.forEach((f, i) => {
      const newest = i === forms.length - 1;
      let note = null;
      if (!newest) note = "Asked again below.";
      else if (f.status === "sent") note = "Sent.";
      else if (f.status === "answered") note = "Answered.";
      else if (isTerminal(P.state || "")) note = "Not answered: the task ended.";
      else if (!canReply()) note = P.state === "input-required" && P.t ? "Watch-only: another client started this task." : "Takes no input now.";
      const takes = note === null && open;
      f.node.inert = !takes;
      f.node.dataset.form = takes ? "live" : f.status === "open" ? "closed" : f.status;
      f.send.hidden = note !== null;
      f.note.textContent = note || "";
      f.note.hidden = note === null;
      // A question the run ended without folds as an answered one does.
      const done = f.status !== "open" || !newest || isTerminal(P.state || "");
      f.body.hidden = done && !f.shown;
      f.toggle.hidden = !done;
      f.toggle.textContent = f.body.hidden ? "Show the form" : "Hide the form";
    });
    P.inputs.forEach((c, i) => c.liven(i === P.inputs.length - 1));
  };

  const formNode = (part) => {
    const form = part.form;
    const why = part.data.length > FORM_MAX ? "it is larger than 64 KiB"
      : form.badId ? "its request id is not one the console can send back"
        : refusal(form.schema);
    if (why) return h("div", { class: "stack", dataset: { form: "raw" } }, [h("p", { class: "small", text: `Not drawn (${why}): answer in words.` })]);
    const initial = { ...lastSent };
    for (const [k, v] of Object.entries(form.initial || {})) if (v !== null && v !== undefined && v !== "") initial[k] = v;
    const spec = formSpec(form.schema, { initial });
    const values = {};
    const prefix = `form${forms.length + 1}`;
    const err = h("p", { class: "ferr", role: "alert" });
    const note = h("p", { class: "small", hidden: true });
    const send = h("button", { class: "btn primary", type: "button", text: "Send" });
    const toggle = h("button", { class: "linkbtn", type: "button", hidden: true, text: "Show the form" });
    const body = h("div", { class: "stack" }, [
      form.instructions ? h("p", { class: "small", text: form.instructions }) : spec.hint ? h("p", { class: "small", text: spec.hint }) : null,
      h("div", { class: "form" }, spec.fields.map((f) => fieldNode(f, values, prefix, { wide: false }))),
      err,
      h("div", { class: "row" }, [send]),
    ]);
    body.style.gap = "12px";
    const node = h("div", { class: "stack", dataset: { form: "live", convention: form.convention } }, [h("div", { class: "row" }, [note, toggle]), body]);
    const entry = { node, send, note, toggle, body, shown: false, status: "open", order: spec.fields.map((f) => f.name), labels: new Map(spec.fields.map((f) => [f.name, f.label || f.name])) };
    toggle.addEventListener("click", () => {
      entry.shown = body.hidden;
      liven();
    });
    send.addEventListener("click", () => sendForm(entry, spec, values, form, err));
    enterSends(body, send);
    forms.push(entry);
    return node;
  };
  async function sendForm(entry, spec, values, form, err) {
    err.textContent = "";
    if (!canReply() || ui.sending || entry !== forms[forms.length - 1]) {
      err.textContent = "Takes no input now.";
      return;
    }
    // Structured Input's agent validates the answer (its §7): an optional text field left empty
    // is left out, never sent as "" (the A2A samples' and the Inspector's send it).
    const got = collect(spec, values, { extra: form.initial, blanks: form.convention !== "structured" });
    if (got.error) {
      err.textContent = got.error;
      return;
    }
    await sending(entry, () => gate.replyTask(P.taskId, formReply(got.data, form, ui.surfaces ? ui.surfaces.dataModels() : null)), err, () => { lastSent = got.data; });
  }

  /** AgentStack's form request: drawn from the status message's metadata, answered as the reply's
   * `form_response`. */
  const agentStackNode = (raw) => {
    const { form, note: why } = normalizeForm(raw);
    if (!form) return h("p", { class: "small", dataset: { form: "raw" }, text: `Not drawn (${why}): answer in words.` });
    const af = agentForm(form, { prefix: `ask${forms.length + 1}` });
    const err = h("p", { class: "ferr", role: "alert" });
    const note = h("p", { class: "small", hidden: true });
    const send = h("button", { class: "btn primary", type: "button", text: form.submit_label || "Send" });
    const toggle = h("button", { class: "linkbtn", type: "button", hidden: true, text: "Show the form" });
    const body = h("div", { class: "stack" }, [form.description ? h("p", { class: "small", text: form.description }) : null, af.node, err, h("div", { class: "row" }, [send])]);
    body.style.gap = "12px";
    const node = h("div", { class: "stack", dataset: { form: "live", convention: "agentstack" } }, [h("div", { class: "row" }, [note, toggle]), body]);
    const entry = { node, send, note, toggle, body, shown: false, status: "open", order: form.fields.map((f) => f.id), agentstack: form };
    toggle.addEventListener("click", () => {
      entry.shown = body.hidden;
      liven();
    });
    send.addEventListener("click", async () => {
      err.textContent = "";
      af.clearErrors();
      if (!canReply() || ui.sending || entry !== forms[forms.length - 1]) {
        err.textContent = "Takes no input now.";
        return;
      }
      const got = af.collect();
      if (got.error) {
        af.showError(got.field, got.error);
        return;
      }
      await sending(entry, () => gate.replyTask(P.taskId, { form_response: { values: got.values }, a2ui_catalogs: [...CATALOGS] }), err);
    });
    enterSends(body, send);
    forms.push(entry);
    return node;
  };

  /** An answer went: the Running card at once, unless the agent already moved on. The silence is
   * counted from the answer: the person's time to answer was not the agent's. */
  const answered = () => {
    P.lastEventAt = Date.now();
    if (P.state === "input-required") ensureLive(new Date().toISOString(), "submitted");
    scheduleRefresh();
  };

  /** Send an answer: the form waits, the Running card starts at once, the error stays with it. */
  async function sending(entry, call, err, done = () => {}) {
    ui.sending = true;
    entry.send.disabled = true;
    liven();
    try {
      await call();
      if (!alive()) return;
      done();
      entry.status = "sent";
      answered();
    } catch (e) {
      if (!alive()) return;
      err.textContent = errorText(e);
    } finally {
      ui.sending = false;
      entry.send.disabled = false;
      if (alive()) liven();
    }
  }

  /** The words box of a question: a textarea and Send, in the question's card. */
  function wordsBox(primary) {
    const text = h("textarea", { class: "reason", rows: "2", maxlength: "65536", placeholder: "Your answer", "aria-label": "Your answer" });
    text.style.minHeight = "56px";
    const err = h("p", { class: "ferr", id: null });
    const btn = h("button", { class: `btn${primary ? " primary" : ""}`, type: "button", text: "Send" });
    btn.addEventListener("click", () => replyWords(text, err, btn));
    const node = h("div", { class: "stack", dataset: { words: "1" } }, [text, h("div", { class: "row" }, [btn]), err]);
    enterSends(node, btn);
    return { node, text, btn, err };
  }
  async function replyWords(text, err, btn) {
    if (!text.value.trim()) {
      err.textContent = "Write your answer first.";
      return;
    }
    err.textContent = "";
    btn.disabled = true;
    try {
      await gate.replyTask(P.taskId, textReply(text.value, ui.surfaces ? ui.surfaces.dataModels() : null));
      if (!alive()) return;
      text.value = "";
      answered();
    } catch (e) {
      if (!alive()) return;
      err.textContent = errorText(e);
    } finally {
      btn.disabled = false;
    }
  }

  /** An agent's question (input-required): its words, its form, the words box, its "{}". */
  const inputCard = (m, at) => {
    const raw = rawToggle("What the agent sent", () => (m ? m.raw : null));
    raw.button.style.marginLeft = "auto";
    const asks = m ? requestedForm(m.metadata) : null;
    const parts = m ? m.parts : [];
    const hasForm = Boolean(asks) || parts.some((p) => p.form || (p.a2ui !== undefined && p.a2ui));
    const body = h("div", { class: "bd stack" });
    body.style.gap = "12px";
    if (asks && m && parts.length && parts.every((p) => p.text !== undefined)) {
      // AgentStack's form request names its form in the text: the form's title.
      body.append(h("b", { text: textOf(m) }));
    } else {
      body.append(...partNodes(parts, "svc", partCtx).filter(Boolean));
    }
    if (asks) body.append(agentStackNode(asks));
    const words = wordsBox(!hasForm);
    const link = h("button", { class: "linkbtn", type: "button", text: "Answer in words", dataset: { act: "words" } });
    link.style.paddingLeft = "0";
    let wordsOpen = !hasForm;
    link.addEventListener("click", () => {
      wordsOpen = true;
      c.liven(true);
      words.text.focus();
    });
    // Cancel while it waits: A2A CancelTask, as the Running card's, confirmed in place.
    const cancelSlot = h("span", { class: "row", dataset: { cancelslot: "1" } });
    cancelSlot.style.gap = "8px";
    const cancelLink = h("button", { class: "linkbtn", type: "button", text: "Cancel the task", dataset: { act: "cancel" } });
    cancelLink.addEventListener("click", () => {
      const yes = h("button", { class: "btn sm danger", type: "button", text: "Yes", dataset: { act: "cancelyes" } });
      const no = h("button", { class: "btn sm", type: "button", text: "No" });
      cancelSlot.replaceChildren(h("span", { class: "small", text: "Cancel this task?" }), yes, no);
      no.addEventListener("click", () => cancelSlot.replaceChildren(cancelLink));
      yes.addEventListener("click", () => cancelRun(cancelSlot, cancelLink));
    });
    cancelSlot.append(cancelLink);
    const links = h("div", { class: "row" }, [link, cancelSlot]);
    links.style.gap = "16px";
    links.style.flexWrap = "wrap";
    const elsewhere = h("p", { class: "small", id: null, hidden: true, text: "Watch-only: another client started this task, and answers it." });
    body.append(links, words.node, elsewhere);
    const state = h("span", { class: "small" });
    const card = h("div", { class: "card", dataset: { input: "1" } }, [
      h("div", { class: "hd" }, [icon("pulse", 14), h("h3", { text: `${name} needs your input` }), state, raw.button]),
      body,
      raw.box,
    ]);
    raw.box.style.padding = "0 15px 15px";
    const c = {
      node: byCard(who, at, card).node,
      answered: false,
      liven(latest) {
        const open = latest && !c.answered && canReply();
        words.node.id = open ? "reply" : "";
        words.btn.id = open ? "replysend" : "";
        words.node.hidden = !open || !wordsOpen;
        link.hidden = !open || wordsOpen;
        cancelSlot.hidden = !(latest && !c.answered && P.state === "input-required" && (P.canceling || Boolean(P.t && P.t.actions.includes("cancel"))));
        links.hidden = link.hidden && cancelSlot.hidden;
        elsewhere.id = latest ? "replynote" : "";
        elsewhere.hidden = !(latest && !c.answered && !canReply() && P.t && P.state === "input-required");
        state.textContent = c.answered ? "answered" : "";
        card.dataset.input = open ? "live" : "closed";
      },
    };
    return c;
  };

  // ------------------------------------------------------------------ parts

  /** The person's own answer to a surface, in words: the button they pressed and the
   * action's context as names and values, labelled as the form labelled its fields. */
  function actionsNode(part, ctx) {
    const said = part.actions.map((a) => {
      if (typeof a.surfaceId === "string") ctx.answered(a.surfaceId);
      const answer = answerOf(a, typeof a.surfaceId === "string" ? ctx.formOf(a.surfaceId) : null);
      const line = h("p", { class: "small" }, [answer.empty ? "You pressed " : "You sent the form ", h("span", { class: "tag", text: answer.pressed })]);
      if (answer.empty) return [line];
      return [line, answer.rows ? rowsNode(answer.rows) : jsonBox(JSON.stringify(a.context, null, 2))];
    });
    return h("div", { class: "stack", dataset: { a2ui: "action" } }, said.flat());
  }
  /** A data part of a message (not a result): names and values for a flat object, else JSON. */
  function dataNode(part, role, ctx) {
    if (role === "you") {
      const order = ctx.formOrder();
      const rank = (k) => (order && order.includes(k) ? order.indexOf(k) : Number.MAX_SAFE_INTEGER);
      const rows = part.rows ? [...part.rows].sort((a, b) => rank(a[0]) - rank(b[0])) : null;
      // One row of chips, as the request folds: the form's labels, the values given.
      const labels = ctx.formLabels();
      const chips = rows ? chipRow(rows.filter(([, v]) => v !== "").map(([k, v]) => [(labels && labels.get(k)) || k, v]), 12) : null;
      return h("div", { class: "stack", dataset: { sent: "data" } }, [h("p", { class: "small", text: order ? "You sent the form" : "You sent these parameters" }), chips || jsonBox(part.data)]);
    }
    if (part.rows) return h("div", { class: "stack", dataset: { data: "rows" } }, [rowsNode(part.rows)]);
    return jsonBox(part.data);
  }
  /** A message's parts as nodes; `ctx` draws what needs the thread (A2UI, forms, files). */
  function partNodes(parts, role, ctx) {
    return parts.map((part) => {
      if (part.sealed) return sealedRow(part.sealed);
      if (part.text !== undefined) return plainText(part.text);
      // An A2UI action is the person's answer only in their own message.
      if (part.actions) return role === "you" ? actionsNode(part, ctx) : dataNode(part, role, ctx);
      if (part.a2ui !== undefined && ctx.a2ui) return ctx.a2ui(part);
      if (part.form) return role === "you" ? jsonBox(part.data) : ctx.form(part);
      if (part.invalid) {
        return h("div", { class: "note", dataset: { invalid: "1" } }, [
          h("span", { text: "Not taken:" }),
          ...part.invalid.map((e) => h("span", { class: "small", text: `${e.at || "the form"}: ${e.error}` })),
        ]);
      }
      if (part.file) return ctx.file(part.file);
      if (part.ap2) return ap2Card(part.ap2, part.data);
      if (part.data !== undefined) return dataNode(part, role, ctx);
      return null;
    });
  }
  const partCtx = {
    a2ui: null,
    form: (part) => formNode(part),
    file: (f) => fileRow(f, { taskId: P.taskId, signal: ctrl.signal }),
    formOrder: () => (forms.length ? forms[forms.length - 1].order : null),
    formLabels: () => (forms.length ? forms[forms.length - 1].labels || null : null),
    formOf: (id) => ui.book.components(id),
    answered: (id) => ui.answered.add(sentKey(id, ui.book.generation(id))),
  };

  // ---- A2UI: one renderer for the thread, fed each A2UI part in the order its events came.
  const loadUi = () => {
    if (!ui.load) {
      ui.load = import("../a2ui.js").then(({ Surfaces, NODES_MAX }) => {
        if (!alive()) throw new Error("the thread was left");
        ui.max = NODES_MAX;
        const close = (id, text) => {
          const b = ui.boxes.get(id);
          if (!b) return;
          ui.boxes.delete(id);
          b.node.inert = true;
          b.node.dataset.closed = "1";
          fill(b.node, [h("p", { class: "small", text })]);
        };
        ui.surfaces = new Surfaces({
          onAction: (ev, version) => sendAction(ev, version),
          onLarge: (id) => close(id, `Too large to draw (more than ${NODES_MAX.toLocaleString("en-US")} components): its JSON is in "{}".`),
        });
        ui.surfaces.onDeleted((id) => {
          const b = ui.boxes.get(id);
          if (!b) return;
          ui.boxes.delete(id);
          ui.gone.push([id, b]);
        });
        return ui.surfaces;
      });
    }
    return ui.load;
  };
  /** The boxes of the surfaces a part deleted, closed in words once the part applied. */
  const closeGone = (created) => {
    for (const [id, b] of ui.gone.splice(0)) {
      b.node.inert = true;
      b.node.dataset.closed = created.includes(id) ? "replaced" : ui.answered.has(sentKey(id, b.gen)) ? "sent" : "closed";
      const title = ui.book.title(id, b.gen);
      fill(b.node, [b.node.dataset.closed === "sent"
        ? h("p", { class: "small", dataset: { formSent: "1" } }, [icon("check", 13), " ", title ? `Form sent · ${title}` : "Form sent"])
        : h("p", { class: "small", text: b.node.dataset.closed === "replaced" ? "Replaced by the form below." : "The agent closed this form." })]);
    }
  };
  const surfaceBox = (sf, id, gen) => {
    const err = h("p", { class: "ferr", role: "alert" });
    const note = h("p", { class: "small", text: "Takes no input now.", hidden: true });
    const node = h("div", { class: "stack", dataset: { surface: id } }, [sf.element(id), err, note]);
    ui.boxes.set(id, { node, err, note, gen });
    return node;
  };
  partCtx.a2ui = (part) => {
    const holder = h("div", { class: "stack", dataset: { a2ui: "pending" } });
    const raw = (why) => {
      holder.dataset.a2ui = "raw";
      fill(holder, [h("p", { class: "small", text: why }), jsonBox(part.data)]);
    };
    if (!part.a2ui) {
      raw("A2UI the console cannot read, as it came:");
      return holder;
    }
    ui.book.note(part.a2ui);
    const gens = new Map(surfacesOf(part.a2ui).map((id) => [id, ui.book.generation(id)]));
    ui.queue = ui.queue.then(async () => {
      let sf;
      try {
        sf = await loadUi();
      } catch (e) {
        if (alive()) raw(`The A2UI renderer did not load (${errorText(e)}): the agent sent this.`);
        return;
      }
      if (!alive()) return;
      // Each message applies on its own (A2UI's processing rules).
      const listMsgs = part.a2ui;
      const before = new Set(ui.boxes.keys());
      const { created, refused, large, shown: drawn } = sf.feed(listMsgs);
      closeGone(created);
      if (refused.length === listMsgs.length) {
        raw(`Not drawn (${refused[0].why}): the agent sent this.`);
        liven();
        return;
      }
      const nodes = drawn.map((id) => surfaceBox(sf, id, gens.get(id) || ui.book.generation(id)));
      if (large.length) {
        holder.dataset.a2ui = "large";
        nodes.push(
          h("p", { class: "small", text: `Not drawn: ${large.length === 1 ? "this form" : "these forms"} would be more than ${ui.max.toLocaleString("en-US")} components. The agent sent this:` }),
          jsonBox(part.data),
        );
      } else if (refused.length) {
        holder.dataset.a2ui = "partial";
        const n = refused.length;
        nodes.push(
          h("p", { class: "small", text: `${n} of the ${listMsgs.length} A2UI messages did not apply (${refused[0].why}); the rest did:` }),
          jsonBox(JSON.stringify(refused.map((r) => listMsgs[r.index]), null, 2)),
        );
      } else {
        holder.dataset.a2ui = nodes.length ? "surface" : "update";
      }
      if (!nodes.length && surfacesOf(listMsgs).some((id) => before.has(id) && ui.boxes.has(id))) {
        nodes.push(h("p", { class: "small", text: "Updated the form above." }));
      }
      if (!nodes.length) holder.hidden = true;
      grew(() => fill(holder, nodes));
      liven();
    }).catch((e) => {
      if (!alive()) return;
      closeGone([]);
      raw(`Not drawn (${errorText(e)}): the agent sent this.`);
    });
    return holder;
  };
  async function sendAction(ev, version) {
    const b = ui.boxes.get(ev.surfaceId);
    const say = (text) => {
      if (b) b.err.textContent = text;
      else toast(text);
    };
    if (!canReply() || ui.sending) {
      say("Takes no input now.");
      return;
    }
    const message = actionMessage(ev, version);
    if (!message) {
      say("Not sent: not an A2UI action, or too large.");
      return;
    }
    say("");
    ui.sending = true;
    liven();
    try {
      await gate.replyTask(P.taskId, actionReply(message, ui.surfaces.dataModels()));
      if (!alive()) return;
      ui.answered.add(sentKey(ev.surfaceId, b ? b.gen : ui.book.generation(ev.surfaceId)));
      answered();
    } catch (e) {
      if (!alive()) return;
      say(errorText(e));
    } finally {
      ui.sending = false;
      if (alive()) liven();
    }
  }

  // ------------------------------------------------------------------ the person's messages

  /** The first request as it reads folded: from what was sent (the start), or from the gate's log. */
  const firstOf = (m) => {
    const text = textOf(m);
    const data = m.parts.find((x) => x.data !== undefined && !x.form && !x.a2ui && !x.actions);
    let parsed = null;
    try { parsed = data ? JSON.parse(data.data) : null; } catch { parsed = null; }
    const values = fulfilledValues(m.metadata);
    // The text an initial form's values make (the console API's "Label: value" lines, when the
    // person wrote none) is the chips' already: not the person's words, not filled in again.
    const made = values && text && text.split("\n").every((l) => /^[^:\n]{1,200}: /.test(l));
    return { text: made ? "" : text, data: parsed && typeof parsed === "object" && !Array.isArray(parsed) ? parsed : null, values };
  };
  let reqRaw = null;
  const drawRequest = (first, at, raw) => {
    reqRaw = raw;
    const rows = requestRows(P.form, { data: first.data, values: first.values });
    const row = requestRow({ rows, text: first.text, raw: () => reqRaw });
    if (!P.req) {
      P.req = byCard({ you: true }, at, row, { id: "request" });
      list.prepend(P.req.node);
    } else {
      P.req.body.replaceChildren(row);
      if (at) P.req.time.textContent = hm(at);
    }
  };
  /** A later message of the person's: their answer, as compact as their request. */
  const youCard = (m, at) => {
    for (const f of forms) if (f.status === "open") f.status = "answered";
    for (const c of P.inputs) c.answered = true;
    const values = answeredValues(m.metadata);
    let body;
    if (values) {
      const asked = [...forms].reverse().find((f) => f.agentstack);
      body = requestRow({ label: "Answer", rows: valueRows(asked ? asked.agentstack : null, values), raw: () => m.raw });
    } else if (m.parts.length === 1 && m.parts[0].text !== undefined) {
      body = requestRow({ label: "Answer", text: m.parts[0].text, raw: () => m.raw });
    } else {
      const raw = rawToggle("What was sent", () => m.raw);
      raw.button.style.position = "absolute";
      raw.button.style.top = "9px";
      raw.button.style.right = "12px";
      body = h("div", { class: "card" }, [h("div", { class: "bd stack" }, [...partNodes(m.parts, "you", partCtx), raw.box]), raw.button]);
      body.style.position = "relative";
      body.firstChild.style.padding = "11px 40px 11px 14px";
    }
    place(byCard({ you: true }, at, body).node);
    liven();
  };
  /** An agent's message (not a status): its parts, under its name. */
  const agentCard = (m, at) => {
    const body = h("div", { class: "card" }, [h("div", { class: "bd stack" }, partNodes(m.parts, "svc", partCtx))]);
    body.firstChild.style.padding = "11px 14px";
    place(byCard(who, at, body).node);
  };

  // ------------------------------------------------------------------ events

  const setState = (state, at, m) => {
    if (!state) {
      if (m) agentCard(m, at);
      return;
    }
    const was = P.state;
    P.state = state;
    // A result whose last chunk has not come is "receiving" only while the run runs: not while it
    // waits for an answer, nor once it ended (a canceled run's log).
    if (flowing(state) !== flowing(was)) for (const r of P.results.values()) if (r.chunked && r.lastChunk !== true) drawResult(r);
    if (LIVE.has(state)) {
      const live = ensureLive(at, state);
      if (m) {
        const said = textOf(m);
        if (said) live.log(at, said);
        if (!onlyText(m)) agentCard({ ...m, parts: m.parts.filter((p) => p.text === undefined) }, at);
      }
    } else if (state === "input-required") {
      settleLive(at, state);
      const c = inputCard(m, at);
      P.inputs.push(c);
      place(c.node);
      liven();
    } else if (state === "auth-required") {
      settleLive(at, state);
      const card = h("div", { class: "card", dataset: { auth: "1" } }, [
        h("div", { class: "hd" }, [h("span", { class: "dot busy" }), h("h3", { text: `${name} needs an authorization` })]),
        h("div", { class: "bd stack" }, [...(m ? partNodes(m.parts, "svc", partCtx) : []), h("p", { class: "small", text: "Give it from a client that can." })]),
      ]);
      place(byCard(who, at, card).node);
    } else if (state === "completed") {
      settleLive(at, state);
      P.terminalAt = Date.parse(at) || Date.now();
      ensureResults(at).deliver(at, m, artifactTexts());
    } else if (isTerminal(state)) {
      settleLive(at, state);
      P.terminalAt = Date.parse(at) || Date.now();
      endCard(state, m, at);
    }
    drawChip();
  };

  const onContent = (ev, d) => {
    if (ev.kind === "task") {
      if (!P.createdAt) P.createdAt = Date.parse(ev.at) || null;
      const ms = [...d.messages];
      const firstYou = ms.findIndex((m) => m.role === "you");
      if (firstYou >= 0) {
        const m = ms.splice(firstYou, 1)[0];
        P.first = firstOf(m);
        drawRequest(P.first, ev.at, m.raw);
      }
      for (const m of ms) (m.role === "you" ? youCard : agentCard)(m, ev.at);
      P.created = true;
      setState(d.state, ev.at, null);
    } else if (ev.kind === "status") {
      // The node's reconnect wait: the warning line says it, not the gate's words in the
      // log; the agent's status the resume repeats is not logged twice. Any other status means the
      // agent is heard from again.
      const rc = d.reconnect || null;
      P.reconnectUntil = rc && rc.state === "waiting" ? rc.until : null;
      let m = d.messages[0] || null;
      if (rc && rc.state === "waiting" && LIVE.has(d.state)) m = null;
      else if (rc && rc.state === "resumed" && m && onlyText(m) && P.live && !P.live.done && textOf(m) === P.live.last) m = null;
      setState(d.state, ev.at, m);
    } else if (ev.kind === "message") {
      for (const m of d.messages) (m.role === "you" ? youCard : agentCard)(m, ev.at);
    } else if (ev.kind === "artifact") {
      for (const a of d.artifacts) addArtifact(a, ev.at);
    }
  };

  const onEvent = (ev) => {
    // The silence counts from the agent's last event: an event of the history read at the page's
    // open by its own time (never later than now), a live one by when it came, so a
    // page opened during a silence says so at once.
    const at = Date.parse(ev.at);
    P.lastEventAt = Date.now() < P.replayUntil && at ? Math.min(Date.now(), at) : Date.now();
    const d = describe(ev);
    logLine(ev, d);
    onContent(ev, d);
    if (ev.kind === "message") liven();
    else if (ev.kind !== "artifact") scheduleRefresh();
    tick();
  };

  // ------------------------------------------------------------------ the task's metadata

  let lastRefresh = 0;
  let refreshTimer = null;
  let refreshing = null;
  let again = false;
  const refreshNow = async () => {
    if (!P.taskId) return;
    if (refreshing) {
      again = true;
      return;
    }
    lastRefresh = Date.now();
    refreshing = (async () => {
      try {
        const t = await gate.task(P.taskId);
        if (alive()) applyMeta(t);
      } catch {
        // the next event tries again
      } finally {
        refreshing = null;
        if (again && alive()) {
          again = false;
          scheduleRefresh();
        }
      }
    })();
  };
  function scheduleRefresh() {
    if (refreshTimer || !alive()) return;
    refreshTimer = setTimeout(() => {
      refreshTimer = null;
      refreshNow();
    }, Math.max(0, lastRefresh + 1000 - Date.now()));
  }

  function applyMeta(t) {
    const before = P.t;
    P.t = { ...t, actions: Array.isArray(t.actions) ? t.actions : [] };
    t = P.t;
    if (t.created_at) P.createdAt = Date.parse(t.created_at) || P.createdAt;
    if (t.terminal_at) P.terminalAt = Date.parse(t.terminal_at) || P.terminalAt;
    if (!before || before.state !== t.state) footRows();
    if (P.live && !P.live.done) P.live.cancel(t.actions.includes("cancel") && !isTerminal(t.state));
    drawComposer(t);
    liven();
    drawChip();
    tick();
  }

  // ---- After it ended: Continue, Run again. Drawn again only when they change.
  const changed = (slot, key) => {
    const k = JSON.stringify(key);
    if (P.metaKey[slot] === k) return false;
    P.metaKey[slot] = k;
    return true;
  };
  const runAgainAllowed = (t) => isTerminal(t.state) && (t.kind || "a2a") === "a2a";
  function drawComposer(t) {
    const can = t.actions.includes("continue");
    // Run again, like Continue, only on the console's own tasks: any other task is watch-only.
    const again = can && runAgainAllowed(t);
    if (!changed("composer", [can, again])) return;
    if (!can && !again) {
      fill(composerSlot, []);
      return;
    }
    const input = h("input", { type: "text", id: "continuetext", maxlength: "65536", placeholder: "Continue…", "aria-label": "Your next message", autocomplete: "off" });
    input.style.flex = "1 1 200px";
    input.style.minWidth = "0";
    input.style.background = "var(--surface)";
    input.style.border = "1px solid var(--line-2)";
    input.style.borderRadius = "9px";
    input.style.padding = "8px 11px";
    const err = h("p", { class: "ferr", id: "continueerr" });
    const send = h("button", { class: "btn primary", type: "button", id: "continuesend", text: "Continue" });
    send.addEventListener("click", () => more(input, err, send));
    input.addEventListener("keydown", (e) => {
      if (e.key === "Enter" && !e.isComposing) send.click();
    });
    const rerun = h("button", { class: "btn", type: "button", id: "runagain", text: "Run again" });
    rerun.addEventListener("click", runAgain);
    const row = h("div", { class: "row" }, [can ? input : null, can ? send : null, again ? rerun : null]);
    row.style.flexWrap = "nowrap";
    if (phone()) row.style.flexWrap = "wrap";
    fill(composerSlot, [h("div", { class: "stack", id: "continue" }, [row, err])]);
  }
  async function more(input, err, btn) {
    const text = input.value;
    if (!text.trim()) {
      err.textContent = "Write your next message first.";
      input.focus();
      return;
    }
    err.textContent = "";
    btn.disabled = true;
    try {
      const started = await gate.continueTask(P.taskId, { text, a2ui_catalogs: [...CATALOGS] });
      if (!alive()) return;
      continued.set(started.task_id, P.taskId);
      handoff = { taskId: started.task_id, name, from: P.taskId, summary: { rows: [], text }, body: { text }, state: started.state, at: new Date().toISOString() };
      location.hash = `#/t/${started.task_id}`;
    } catch (e) {
      if (!alive()) return;
      err.textContent = errorText(e);
      btn.disabled = false;
    }
  }
  async function runAgain() {
    const open = againSlot.querySelector("#startform");
    if (open) {
      open.scrollIntoView({ block: "nearest" });
      return;
    }
    let form;
    try {
      form = await loadForm();
    } catch (e) {
      toast(errorText(e));
      return;
    }
    if (!alive()) return;
    const card = requestCard(form, {
      service: name,
      prefill: P.first,
      discard: () => fill(againSlot, []),
      send: async (body, summary) => {
        const started = await gate.startTask(body);
        if (!alive()) return;
        handoff = { taskId: started.task_id, name, summary, body, state: started.state, at: new Date().toISOString() };
        location.hash = `#/t/${started.task_id}`;
      },
    });
    grew(() => fill(againSlot, [byCard({ you: true }, null, card.node).node]));
    card.focus();
  }

  // ------------------------------------------------------------------ the agent's card

  const loadForm = () => {
    if (!P.formP) {
      P.formP = gate.startForm(name).then((f) => {
        P.form = f;
        return f;
      });
      P.formP.catch(() => { P.formP = null; });
    }
    return P.formP;
  };
  const readCard = () => {
    if (kind !== "a2a") return;
    loadForm().then((f) => {
      if (!alive()) return;
      setBlurb(f.description);
      if (f.name) $("#ctxname", side).textContent = f.name;
      // The folded request reads with the form's labels, once they are known.
      if (P.first && P.req) drawRequest(P.first, null, reqRaw);
    }).catch(() => {});
  };

  // ------------------------------------------------------------------ starting a task

  /** Show what was sent as the folded request and the Running card, before the gate answers. */
  const startShown = (summary, body, at, phase) => {
    reqRaw = body;
    const row = requestRow({ rows: summary.rows, text: summary.text, raw: () => reqRaw });
    if (P.req) P.req.node.remove();
    P.req = byCard({ you: true }, at, row, { id: "request" });
    list.prepend(P.req.node);
    P.state = "submitted";
    ensureLive(at, phase);
    drawChip();
  };

  /** Read the task, then follow its events. */
  const attach = async (t) => {
    if (t) applyMeta(t);
    else {
      try {
        const got = await gate.task(P.taskId);
        if (!alive()) return;
        applyMeta(got);
      } catch {
        // its events say what happens; the metadata is read again after them
      }
    }
    if (!alive()) return;
    readCard();
    loadRuns();
    follow(P.taskId, 0, ctrl.signal, {
      onOpen: () => {
        trouble.hidden = true;
        if (!P.replayUntil) P.replayUntil = Date.now() + REPLAY_MS;
      },
      onEvent,
      onEnd: (end) => {
        P.ended = true;
        P.streamDone = true;
        if (P.t && end.state !== P.t.state) refreshNow();
        else scheduleRefresh();
        drawChip();
        tick();
      },
      onTrouble: (text) => {
        trouble.hidden = false;
        trouble.textContent = `Stream stopped: ${text}`;
      },
    });
  };

  stop = () => {
    ctrl.abort();
    clearInterval(ticker);
    clearTimeout(refreshTimer);
    if (narrow) narrow.removeEventListener("change", fitSide);
    if (scroller) {
      scroller.removeEventListener("scroll", onScroll);
      for (const ev of MEANT) scroller.removeEventListener(ev, meant);
    }
    document.removeEventListener("keydown", meant);
    root.removeEventListener("click", clicked, true);
    if (keeper) keeper.disconnect();
    pill.remove();
    if (column) column.style.position = "";
    if (ui.surfaces) ui.surfaces.dispose();
  };

  return {
    attach,
    /** A task sent from the page before (handoff): its request folded, its Running card. */
    started(ho) {
      startShown(ho.summary, ho.body, ho.at, "submitted");
      runItems = [{ task_id: ho.taskId, state: ho.state, created_at: ho.at, summary: ho.summary.text || null, local: true }];
      drawRuns();
    },
    /** The request card of a new task (#/run). */
    async newRun(prefill) {
      fill(list, [h("p", { class: "empty", text: "Loading…" })]);
      loadRuns();
      let form;
      try {
        form = await loadForm();
      } catch (err) {
        if (!alive()) return;
        fill(list, [failure(err, "This service's card")]);
        return;
      }
      if (!alive()) return;
      setBlurb(form.description);
      if (form.name) $("#ctxname", side).textContent = form.name;
      chipText.textContent = "new task";
      const card = requestCard(form, {
        service: name,
        prefill,
        send: async (body, summary) => {
          // Optimistic: folded and Starting at once; the form comes back with the gate's error.
          const at = new Date().toISOString();
          const held = editing.node;
          held.hidden = true;
          startShown(summary, body, at, "submitted");
          let started;
          try {
            started = await gate.startTask(body);
          } catch (e) {
            if (alive()) {
              if (P.live) P.live.remove();
              P.live = null;
              P.req.node.remove();
              P.req = null;
              P.state = null;
              held.hidden = false;
              plainChip();
              dot.className = "dot idle";
              chipText.textContent = "new task";
              chipTime.textContent = "";
            }
            throw e;
          }
          if (!alive()) return;
          editing.node.remove();
          P.taskId = started.task_id;
          shown = started.task_id;
          history.replaceState(null, "", `#/t/${started.task_id}`);
          crumb("tasks", shortId(started.task_id));
          newSec.hidden = false;
          fitSide();
          runItems = [{ task_id: started.task_id, state: started.state, created_at: at, summary: summary.text || null, local: true }, ...runItems];
          drawRuns();
          attach(null);
        },
      });
      const editing = byCard({ you: true }, null, card.node);
      fill(list, [editing.node]);
      card.focus();
    },
  };
}
