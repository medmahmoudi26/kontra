// The run page's small parts, shared by its cards: the by-line every card
// carries, the "{}" toggle that shows what was sent as it went, a clamp with its "Show all", a row
// of chips, plain text. From console.css as shipped (`.msg`, `.by`, `.chip`, `.linkbtn`,
// `pre.json`); what it lacks through the CSSOM, never a style attribute.

import { h, hhmm } from "../dom.js";
import { serviceAvatar } from "./common.js";

export const phone = () => typeof matchMedia === "function" && matchMedia("(max-width:720px)").matches;
export const reducedMotion = () => typeof matchMedia === "function" && matchMedia("(prefers-reduced-motion: reduce)").matches;

/** "hh:mm" of an ISO time or a Date, "" when there is none. */
export function hm(at) {
  if (!at) return "";
  const d = at instanceof Date ? at : new Date(at);
  return Number.isNaN(d.getTime()) ? "" : d.toLocaleTimeString("en-GB", { hour: "2-digit", minute: "2-digit" });
}

/** Seconds as "m:ss" ("1:02:03" past an hour). */
export function mss(seconds) {
  const s = Math.max(0, Math.floor(seconds));
  const hh = Math.floor(s / 3600);
  const mm = Math.floor((s % 3600) / 60);
  const ss = String(s % 60).padStart(2, "0");
  return hh ? `${hh}:${String(mm).padStart(2, "0")}:${ss}` : `${mm}:${ss}`;
}

/** A card of the thread with its by-line: who (`you`, the agent of `who`, or a plain name), and
 * when. `who`: { you: true } | { ref, kind, name } | { name }. */
export function byCard(who, at, body, { id = null, cls = "" } = {}) {
  const you = who && who.you;
  const av = you
    ? h("span", { class: "av you", text: "you" })
    : who && who.ref ? h("span", { class: "av" }, [serviceAvatar(who.ref, who.kind || "a2a", 22)]) : h("span", { class: "av k", text: "K" });
  const time = h("time", { text: at ? hm(at) : "" });
  if (at) time.title = new Date(at).toLocaleString("en-GB");
  const wrap = h("div", { class: `body stack${cls ? ` ${cls}` : ""}` }, [body]);
  if (phone()) wrap.style.marginLeft = "0";
  const node = h("div", { class: `msg ${you ? "you" : "svc"}`, id }, [
    h("div", { class: "by" }, [av, h("b", { text: you ? "You" : (who && who.name) || "the agent" }), time]),
    wrap,
  ]);
  return { node, body: wrap, time };
}

/** JSON in a box that wraps and scrolls (CSSOM). */
export function jsonBox(text) {
  const pre = h("pre", { class: "json", text });
  pre.style.whiteSpace = "pre-wrap";
  pre.style.overflowWrap = "anywhere";
  pre.style.maxHeight = phone() ? "50vh" : "360px";
  pre.style.overflowY = "auto";
  return pre;
}

/** A value's JSON for the eye: a string longer than 4,096 characters (a file's base64, a data: URI)
 * is cut, with how much more it holds. */
const pretty = (v) => {
  const cut = (_, x) => (typeof x === "string" && x.length > 4096 ? `${x.slice(0, 4096)}… (${(x.length - 4096).toLocaleString("en-US")} more characters)` : x);
  try {
    return typeof v === "string" ? v : JSON.stringify(v, cut, 2);
  } catch {
    return String(v);
  }
};

/** The "{}" toggle: a small button that shows `what()` (a value, its JSON) in a box under `host`
 * once pressed, and hides it again. Returns { button, box }; the box is empty until opened. */
export function rawToggle(label, what) {
  const box = h("div", { class: "stack", hidden: true, dataset: { raw: "1" } });
  const button = h("button", { class: "linkbtn", type: "button", text: "{}", title: label, "aria-label": label, "aria-expanded": "false", dataset: { rawtoggle: "1" } });
  button.style.fontFamily = "var(--mono)";
  button.addEventListener("click", () => {
    const open = box.hidden;
    box.replaceChildren(...(open ? [jsonBox(pretty(what()))] : []));
    box.hidden = !open;
    button.setAttribute("aria-expanded", String(open));
  });
  return { button, box };
}

/** Text, its line breaks kept; clamped to `lines` with a "Show all" that opens it (shown only when
 * the text is longer). */
export function clamped(nodes, lines, { cls = "plain" } = {}) {
  const p = h("div", { class: cls }, nodes);
  p.style.whiteSpace = "pre-wrap";
  p.style.overflowWrap = "anywhere";
  p.style.overflow = "hidden";
  p.style.maxHeight = `${lines * 1.45}em`;
  const more = h("button", { class: "linkbtn", type: "button", text: "Show all", hidden: true, dataset: { act: "more" } });
  more.style.paddingLeft = "0";
  more.style.alignSelf = "flex-start";
  more.addEventListener("click", () => {
    const open = p.style.maxHeight !== "none";
    p.style.maxHeight = open ? "none" : `${lines * 1.45}em`;
    more.textContent = open ? "Show less" : "Show all";
  });
  // "Show all" only for text the clamp cuts: measured once the node is in the page.
  let tries = 0;
  const measure = () => {
    if (!p.isConnected || !p.clientHeight) {
      if (tries++ < 20) setTimeout(measure, 150);
      return;
    }
    more.hidden = p.scrollHeight <= p.clientHeight + 2;
  };
  requestAnimationFrame(() => requestAnimationFrame(measure));
  const node = h("div", { class: "stack" }, [p, more]);
  node.style.gap = "2px";
  return node;
}

/** One line of text cut to its first line, the rest a click away (the running card's log). */
export function oneLine(text) {
  const first = String(text).split(/\r?\n/)[0];
  const p = h("p", { text: first });
  const whole = String(text).trim();
  if (whole !== first.trim() || first.length > 160) {
    p.style.cursor = "pointer";
    p.title = "Show all";
    p.style.whiteSpace = "nowrap";
    p.style.overflow = "hidden";
    p.style.textOverflow = "ellipsis";
    p.addEventListener("click", () => {
      const open = p.dataset.open === "1";
      p.dataset.open = open ? "" : "1";
      p.textContent = open ? first : whole;
      p.style.whiteSpace = open ? "nowrap" : "pre-wrap";
    });
  } else {
    p.style.overflowWrap = "anywhere";
  }
  return p;
}

/** Chips "label value": at most `max`, then "+N more". */
export function chipRow(rows, max = 6) {
  const shown = rows.slice(0, max).map(([k, v]) => {
    const c = h("span", { class: "chip", title: `${k}: ${v}` }, [h("b", { text: k }), h("span", { text: v })]);
    c.style.maxWidth = "260px";
    return c;
  });
  if (rows.length > max) shown.push(h("span", { class: "chip", dataset: { more: String(rows.length - max) } }, [h("span", { text: `+${rows.length - max} more` })]));
  return h("div", { class: "chips" }, shown);
}

/** A part's text, as it came: line breaks kept, never Markdown or HTML. */
export function plainText(text) {
  const p = h("p", { class: "plain", text });
  p.style.whiteSpace = "pre-wrap";
  return p;
}

/** The flex room a note's words take beside its chip or icon. */
export function grow(node) {
  node.style.flex = "1 1 0";
  return node;
}

/** "hh:mm:ss" of an ISO time. */
export const hms = (at) => (at ? hhmm(new Date(at)) : "");

/** The text-like inputs Enter sends from, as a form's implicit submission does. */
const SINGLE_LINE = new Set(["text", "search", "url", "email", "tel", "number", "password", "date", "datetime-local", "month", "week", "time"]);

/** Enter in one of `root`'s one-line inputs presses `button` (a form's implicit submission, as
 * AgentStack's renderer does); in a textarea, Ctrl or Cmd+Enter does, Enter alone is a new line.
 * A select keeps Enter for its list, a button its own. */
export function enterSends(root, button) {
  root.addEventListener("keydown", (e) => {
    if (e.key !== "Enter" || e.isComposing || e.defaultPrevented || e.altKey || e.shiftKey) return;
    const t = e.target;
    const send = t instanceof HTMLTextAreaElement ? e.ctrlKey || e.metaKey : t instanceof HTMLInputElement && SINGLE_LINE.has(t.type);
    if (!send || button.disabled || button.hidden || button.closest("[hidden]")) return;
    e.preventDefault();
    button.click();
  });
}
