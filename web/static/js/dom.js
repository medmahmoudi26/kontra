// DOM helpers. Every string from the gate or an agent reaches the page through textContent or an
// attribute set here, never through HTML parsing: there is no innerHTML, insertAdjacentHTML or
// document.write anywhere in the console, and the page's CSP allows scripts only from the gate's
// own console assets.

const SVG_NS = "http://www.w3.org/2000/svg";
const ATTR_OK = /^(?:[a-z][a-z0-9-]*|aria-[a-z-]+|data-[a-z0-9-]+)$/;

/**
 * h("button", {class: "btn", type: "button", onclick: fn, text: "Go"}, [children])
 * `text` sets textContent; `on*` adds listeners; `dataset` fills data-*; other props are
 * attributes (names checked, so a key that came from data cannot become an event handler).
 */
export function h(tag, props = {}, children = []) {
  const n = document.createElement(tag);
  apply(n, props);
  append(n, children);
  return n;
}

function apply(n, props) {
  for (const [k, v] of Object.entries(props || {})) {
    if (v === null || v === undefined || v === false) continue;
    if (k === "class") n.className = v;
    else if (k === "text") n.textContent = String(v);
    else if (k === "dataset") Object.assign(n.dataset, v);
    else if (k.startsWith("on") && typeof v === "function") n.addEventListener(k.slice(2), v);
    else if (k === "value") n.value = v;
    else if (k === "checked") n.checked = Boolean(v);
    else if (k === "href" || k === "src" || k === "action") {
      if (!safeUrl(String(v))) throw new Error(`refusing ${k} ${String(v).slice(0, 40)}`);
      n.setAttribute(k, String(v));
    }
    else if (ATTR_OK.test(k) && !k.startsWith("on") && k !== "style" && k !== "srcdoc") n.setAttribute(k, v === true ? "" : String(v));
    else throw new Error(`refusing attribute ${k}`);
  }
}

export function append(n, children) {
  for (const c of [].concat(children)) {
    if (c === null || c === undefined || c === false) continue;
    n.appendChild(typeof c === "string" || typeof c === "number" ? document.createTextNode(String(c)) : c);
  }
  return n;
}

/** http(s), blob: or a same-origin path; never javascript:, data: or anything else a link
 * could run or render. */
export function safeUrl(url) {
  if (url.startsWith("/") && !url.startsWith("//")) return true;
  if (url.startsWith("#")) return true;
  try {
    const u = new URL(url);
    return u.protocol === "https:" || u.protocol === "http:" || u.protocol === "blob:";
  } catch {
    return false;
  }
}

export const $ = (sel, root = document) => root.querySelector(sel);
export const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));

/** Replace a node's children. */
export function fill(n, children) {
  n.replaceChildren();
  return append(n, children);
}

// ------------------------------------------------------------------ icons (constant path data)

const ICONS = {
  check: ["m4 12.5 5.2 5L20 7"],
  plus: ["M12 5v14M5 12h14"],
  clock: ["M12 7.5V12l3 2", { circle: [12, 12, 9] }],
  upload: ["M12 16V4m0 0 4 4m-4-4L8 8M4 16v2a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-2"],
  download: ["M12 4v12m0 0 4-4m-4 4-4-4M4 16v2a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-2"],
  key: ["M15.5 7.5a3.5 3.5 0 1 1-7 0 3.5 3.5 0 0 1 7 0ZM12 11v10m0-4h3m-3 3h2"],
  lock: ["M7 11V8a5 5 0 0 1 10 0v3M5 11h14v10H5z"],
  gift: ["M20 12v9H4v-9M2 7h20v5H2zM12 22V7"],
  x: ["M6 6l12 12M18 6 6 18"],
  chev: ["m9 6 6 6-6 6"],
  moon: ["M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8Z"],
  search: ["m21 21-4.3-4.3", { circle: [11, 11, 7] }],
  scale: ["M12 3v18M5 7h14M5 7l-3 7a3 3 0 0 0 6 0L5 7Zm14 0-3 7a3 3 0 0 0 6 0l-3-7ZM8 21h8"],
  out: ["M15 4h4v16h-4M10 8l-4 4 4 4M6 12h10"],
  pulse: ["M3 12h4l3-8 4 16 3-8h4"],
};

/** An inline SVG icon from the constant table above (stroke: currentColor). */
export function icon(name, size = 15, extraClass = "") {
  const svg = document.createElementNS(SVG_NS, "svg");
  svg.setAttribute("width", String(size));
  svg.setAttribute("height", String(size));
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("fill", "none");
  svg.setAttribute("stroke", "currentColor");
  svg.setAttribute("stroke-width", "2");
  svg.setAttribute("stroke-linecap", "round");
  svg.setAttribute("stroke-linejoin", "round");
  svg.setAttribute("aria-hidden", "true");
  if (extraClass) svg.setAttribute("class", extraClass);
  for (const part of ICONS[name] || []) {
    if (typeof part === "string") {
      const p = document.createElementNS(SVG_NS, "path");
      p.setAttribute("d", part);
      svg.appendChild(p);
    } else if (part.circle) {
      const c = document.createElementNS(SVG_NS, "circle");
      c.setAttribute("cx", String(part.circle[0]));
      c.setAttribute("cy", String(part.circle[1]));
      c.setAttribute("r", String(part.circle[2]));
      svg.appendChild(c);
    }
  }
  return svg;
}

// ------------------------------------------------------------------ avatars (identicons)

function fnv(s) {
  let x = 2166136261;
  for (let i = 0; i < s.length; i++) {
    x ^= s.charCodeAt(i);
    x = Math.imul(x, 16777619);
  }
  return Math.abs(x);
}

/** A brand-tinted 5x5 identicon for a service's name, as SVG nodes. `soft`:
 * on the brand's soft tint (console.css's --brand-soft and --brand-line, through CSSOM, which the
 * CSP allows: never a style attribute), as the console draws an MCP server. */
export function avatar(seed, size = 46, cls = "av", { soft = false } = {}) {
  const svg = document.createElementNS(SVG_NS, "svg");
  svg.setAttribute("viewBox", "0 0 64 64");
  svg.setAttribute("width", String(size));
  svg.setAttribute("height", String(size));
  svg.setAttribute("class", cls);
  svg.setAttribute("aria-hidden", "true");
  const hsh = fnv(seed);
  const hue = (((185 + (hsh % 80)) - (hsh % 2 ? 40 : 0)) + 360) % 360;
  const hue2 = (hue + 34) % 360;
  const rect = (x, y, w, hgt, rx, color) => {
    const r = document.createElementNS(SVG_NS, "rect");
    for (const [k, v] of Object.entries({ x, y, width: w, height: hgt, rx, fill: color })) r.setAttribute(k, String(v));
    svg.appendChild(r);
  };
  rect(0, 0, 64, 64, 15, `hsl(${hue} 44% 15%)`);
  if (soft) {
    const bg = svg.lastChild;
    bg.style.fill = "var(--brand-soft)";
    bg.style.stroke = "var(--brand-line)";
    bg.style.strokeWidth = "2";
  }
  for (let y = 0; y < 5; y++) {
    for (let x = 0; x < 3; x++) {
      if (fnv(`${seed}:${x}:${y}`) % 10 <= 4) continue;
      const color = (x + y) % 2 ? `hsl(${hue2} 66% 50%)` : `hsl(${hue} 74% 62%)`;
      for (const xx of new Set([x, 4 - x])) {
        rect(8 + xx * 10, 8 + y * 10, 8, 8, 2.2, color);
        if (soft) svg.lastChild.style.fill = (x + y) % 2 ? "var(--brand)" : "var(--brand-ink)";
      }
    }
  }
  return svg;
}

// ------------------------------------------------------------------ formatting

export const n0 = (x) => Number(x).toLocaleString("en-US");

export function fmtBytes(b) {
  if (b >= 1024 ** 3) return (b / 1024 ** 3).toFixed(1) + " GB";
  if (b >= 1024 ** 2) return (b / 1024 ** 2).toFixed(1) + " MB";
  if (b >= 1024) return (b / 1024).toFixed(1) + " KB";
  return b + " B";
}

/** Seconds -> "1:02:03" / "02:03". */
export function clock(s) {
  s = Math.max(0, Math.floor(s));
  const hh = Math.floor(s / 3600);
  const mm = Math.floor((s % 3600) / 60);
  const ss = s % 60;
  const p = (v) => String(v).padStart(2, "0");
  return hh ? `${hh}:${p(mm)}:${p(ss)}` : `${p(mm)}:${p(ss)}`;
}

/** Seconds -> "6h", "1d 2h", "45m", "30s". */
export function duration(s) {
  if (!Number.isFinite(s)) return "—";
  const d = Math.floor(s / 86400);
  const hh = Math.floor((s % 86400) / 3600);
  const mm = Math.floor((s % 3600) / 60);
  if (d) return hh ? `${d}d ${hh}h` : `${d}d`;
  if (hh) return mm ? `${hh}h ${mm}m` : `${hh}h`;
  if (mm) return `${mm}m`;
  return `${Math.max(0, Math.floor(s))}s`;
}

export const hhmm = (d) => d.toLocaleTimeString("en-GB", { hour: "2-digit", minute: "2-digit", second: "2-digit" });

// ------------------------------------------------------------------ toasts

/** A short message at the bottom right. `parts` are strings or nodes. */
export function toast(parts, ms = 3600) {
  const host = $("#toasts");
  if (!host) return;
  const t = h("div", { class: "toast", role: "status" }, parts);
  host.appendChild(t);
  setTimeout(() => {
    t.classList.add("leaving");
    setTimeout(() => t.remove(), 320);
  }, ms);
}

/** Text of an error for a person: the gate's message, or the exception's. */
export function errorText(err) {
  if (!err) return "Something went wrong.";
  if (err.name === "AbortError") return "Canceled.";
  return err.message || String(err);
}
