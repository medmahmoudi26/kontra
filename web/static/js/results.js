// What an agent returns, read by its shape: a data part's arrays of objects as tables,
// its scalars as a grid of names and values, the rest kept as JSON; a text part that says it is
// Markdown drawn from a small subset (headings, lists, code, emphasis, https links) through h(),
// never as HTML, in time linear in its length. `shapeOf` is pure; `markdownNodes` builds nodes.

import { h } from "./dom.js";

export const TABLES_MAX = 3;
export const COLUMNS_MAX = 8;
export const ROWS_MAX = 50;
export const CELL_MAX = 60;
export const MARKDOWN_MAX = 200 * 1024;

const obj = (v) => v !== null && typeof v === "object" && !Array.isArray(v);
const scalar = (v) => v === null || typeof v === "string" || typeof v === "number" || typeof v === "boolean";

/** A cell's text: a scalar as it reads, long text cut at CELL_MAX (its whole in `full`). */
export function cellText(v) {
  if (v === null || v === undefined) return { text: "", full: null };
  const s = typeof v === "string" ? v.replace(/\s+/g, " ").trim() : String(v);
  return s.length > CELL_MAX ? { text: `${s.slice(0, CELL_MAX - 1)}…`, full: s.slice(0, 2000) } : { text: s, full: null };
}

/** The table an array of objects reads as: up to COLUMNS_MAX columns, those whose values are
 * scalars (in the order first seen; long text after short), up to ROWS_MAX rows. */
function tableOf(key, list) {
  const order = [];
  const scalarIn = new Map();
  const length = new Map();
  for (const row of list) {
    for (const [k, v] of Object.entries(row)) {
      if (!scalarIn.has(k)) {
        order.push(k);
        scalarIn.set(k, true);
        length.set(k, 0);
      }
      if (!scalar(v)) scalarIn.set(k, false);
      else if (typeof v === "string") length.set(k, Math.max(length.get(k), v.length));
    }
  }
  const usable = order.filter((k) => scalarIn.get(k));
  const short = usable.filter((k) => length.get(k) <= 120);
  const long = usable.filter((k) => length.get(k) > 120);
  const columns = [...short, ...long].slice(0, COLUMNS_MAX);
  if (!columns.length) return null;
  return {
    key,
    columns,
    numeric: columns.map((c) => list.every((r) => r[c] === undefined || r[c] === null || typeof r[c] === "number")),
    rows: list.slice(0, ROWS_MAX).map((r) => columns.map((c) => r[c])),
    total: list.length,
    hidden: order.length - columns.length,
  };
}

const rowsOfObjects = (v) => Array.isArray(v) && v.length > 0 && v.every(obj);

/** How a data part reads: { tables: [{key, columns, numeric, rows, total, hidden}], scalars:
 * [[name, value]], rest: {name: value} | null }. An array of objects (the part itself, or a value
 * of its top level) is a table; the top level's scalars are the grid; anything else is the rest. */
export function shapeOf(data) {
  if (rowsOfObjects(data)) {
    const t = tableOf(null, data);
    return t ? { tables: [t], scalars: [], rest: null } : { tables: [], scalars: [], rest: { value: data } };
  }
  if (!obj(data)) return { tables: [], scalars: scalar(data) ? [["value", data]] : [], rest: scalar(data) ? null : { value: data } };
  const tables = [];
  const scalars = [];
  const rest = {};
  for (const [k, v] of Object.entries(data)) {
    if (scalar(v)) scalars.push([k, v]);
    else if (rowsOfObjects(v) && tables.length < TABLES_MAX) {
      const t = tableOf(k, v);
      if (t) tables.push(t);
      else rest[k] = v;
    } else rest[k] = v;
  }
  return { tables, scalars, rest: Object.keys(rest).length ? rest : null };
}

/** Whether a text part reads as Markdown: its media type or its result's name says so. */
export function isMarkdown(type, name) {
  const t = typeof type === "string" ? type.split(";")[0].trim().toLowerCase() : "";
  return t === "text/markdown" || t === "text/x-markdown" || (typeof name === "string" && /\.(md|markdown)$/i.test(name));
}

// ------------------------------------------------------------------ Markdown, a small subset

// Linear in the line's length: a link's text stops at the next bracket and its URL at
// the next bracket or space, so no two attempts read the same characters; an https URL only, the
// console's rule for every link an agent gives (js/files.js, js/taskevents.js).
const INLINE = /(`[^`\n]+`)|(\*\*[^*\n]+\*\*)|(\[[^[\]\n]{1,300}\]\((https:\/\/[^\s)[\]]{1,2000})\))/g;

function safeHref(url) {
  try {
    const u = new URL(url);
    return u.protocol === "https:" ? u.href : null;
  } catch {
    return null;
  }
}

/** One line's inline marks: `code`, **bold** and [a link](https://…) -- the rest as text (a link
 * to anything but https stays text). */
export function inlineNodes(text) {
  const out = [];
  let at = 0;
  for (const m of text.matchAll(INLINE)) {
    if (m.index > at) out.push(text.slice(at, m.index));
    if (m[1]) {
      const c = h("code", { class: "mono", text: m[1].slice(1, -1) });
      c.style.fontSize = "0.92em";
      out.push(c);
    } else if (m[2]) {
      out.push(h("b", { text: m[2].slice(2, -2) }));
    } else {
      const href = safeHref(m[4]);
      const label = m[3].slice(1, m[3].indexOf("]("));
      out.push(href ? h("a", { href, target: "_blank", rel: "noopener noreferrer", text: label }) : m[0]);
    }
    at = m.index + m[0].length;
  }
  if (at < text.length) out.push(text.slice(at));
  return out;
}

/** The longest line read as a heading; a longer one is a paragraph. */
export const HEADING_MAX = 1000;
const HEADING_OPEN = /^(#{1,6})[ \t]/;

/** A heading line's level and text, or null: one to six #, a space, its text, closing #s (after a
 * space) left out. No regex past the opening, so a line of any length reads in linear time
 * (a heading pattern like `^(#{1,6})\s+(.+?)\s*#*\s*$` backtracked in cubic time on "# a", spaces, "x"). */
export function headingOf(line) {
  if (typeof line !== "string" || line.length > HEADING_MAX) return null;
  const m = HEADING_OPEN.exec(line);
  if (!m) return null;
  let text = line.slice(m[0].length).trim();
  let j = text.length;
  while (j > 0 && text[j - 1] === "#") j -= 1;
  if (j < text.length && (j === 0 || text[j - 1] === " " || text[j - 1] === "\t")) text = text.slice(0, j).trimEnd();
  return text ? { level: m[1].length, text } : null;
}

const BULLET = /^\s*[-*+]\s+(.*)$/;
const NUMBERED = /^\s*\d{1,4}[.)]\s+(.*)$/;
const FENCE = /^\s*```/;

/** Markdown as nodes: headings, bulleted and numbered lists, fenced code, paragraphs (line breaks
 * kept), and the inline marks above. Never HTML: a tag in the text stays text. */
export function markdownNodes(text) {
  const lines = String(text).slice(0, MARKDOWN_MAX).split(/\r?\n/);
  const out = [];
  let para = [];
  let list = null;
  const flushPara = () => {
    if (!para.length) return;
    const p = h("p", { class: "plain" });
    para.forEach((line, i) => {
      if (i) p.appendChild(document.createElement("br"));
      for (const n of inlineNodes(line)) p.append(n);
    });
    out.push(p);
    para = [];
  };
  const flushList = () => {
    if (!list) return;
    list.node.style.margin = "0";
    list.node.style.paddingLeft = "20px";
    out.push(list.node);
    list = null;
  };
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    if (FENCE.test(line)) {
      flushPara();
      flushList();
      const body = [];
      i += 1;
      while (i < lines.length && !FENCE.test(lines[i])) body.push(lines[i++]);
      const pre = h("pre", { class: "json", text: body.join("\n") });
      pre.style.whiteSpace = "pre-wrap";
      pre.style.overflowWrap = "anywhere";
      out.push(pre);
      continue;
    }
    const head = headingOf(line);
    const bullet = BULLET.exec(line);
    const numbered = bullet ? null : NUMBERED.exec(line);
    if (head) {
      flushPara();
      flushList();
      const { level } = head;
      const n = h(level <= 2 ? "h3" : "h4", {}, inlineNodes(head.text));
      n.style.fontSize = level === 1 ? "15px" : level === 2 ? "14px" : "13px";
      n.style.margin = "4px 0 0";
      out.push(n);
    } else if (bullet || numbered) {
      flushPara();
      const kind = bullet ? "ul" : "ol";
      if (!list || list.kind !== kind) {
        flushList();
        list = { kind, node: h(kind, { class: "plain" }) };
      }
      list.node.appendChild(h("li", {}, inlineNodes((bullet || numbered)[1])));
    } else if (!line.trim()) {
      flushPara();
      flushList();
    } else {
      flushList();
      para.push(line);
    }
  }
  flushPara();
  flushList();
  return out;
}
