// What the views share: a failure said the same way everywhere, a copy button, dates, a task's
// state in words, a kind's tag and the avatars of services.

import { avatar, errorText, h } from "../dom.js";

/** A refusal or a failure, as an empty state. `what` names what could not be read. */
export function failure(err, what) {
  return h("p", { class: "empty err", role: "alert", text: what ? `${what}: ${errorText(err)}` : errorText(err) });
}

/** An empty state: a sentence, and what to do about it. */
export function empty(text, ...more) {
  return h("div", { class: "card" }, [h("div", { class: "bd stack" }, [h("p", { class: "lead", text }), ...more])]);
}

/** A button that copies `text` (or says how to, where the browser refuses). */
export function copyButton(text, label = "Copy", note = null) {
  const b = h("button", { class: "btn sm", type: "button", text: label });
  b.addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(typeof text === "function" ? text() : text);
      b.textContent = "Copied";
      setTimeout(() => { b.textContent = label; }, 1600);
    } catch {
      if (note) note.textContent = "The browser did not allow copying: select the text and copy it with Ctrl+C (⌘C).";
    }
  });
  return b;
}

/** "1 Nov 2026, 14:02", or "—". */
export function when(iso) {
  if (!iso) return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "—";
  return d.toLocaleString("en-GB", { year: "numeric", month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
}

/** A service's kind as a tag: "a2a" or "mcp". */
export const kindTag = (kind) => h("span", { class: "tag", text: kind === "mcp" ? "mcp" : "a2a" });

/** A service's avatar: the identicon of its name, brand-soft for an MCP server. */
export function serviceAvatar(ref, kind, size = 46) {
  return avatar(ref, size, "av", { soft: kind === "mcp" });
}

// ------------------------------------------------------------------ a task's words

const TERMINAL = new Set(["completed", "failed", "canceled", "rejected"]);
export const isTerminal = (state) => TERMINAL.has(state);

/** A task's state -> [words, jchip class]. */
export function stateChip(state) {
  switch (state) {
    case "submitted": return ["submitted", "running"];
    case "working": return ["working", "running"];
    case "auth-required": return ["auth required", "wait"];
    case "input-required": return ["input required", "wait"];
    case "completed": return ["completed", "ok"];
    case "failed": return ["failed", "bad"];
    case "rejected": return ["rejected", "bad"];
    case "canceled": return ["canceled", "muted"];
    default: return [String(state || "—"), "muted"];
  }
}

/** A task state's chip. */
export const chip = ([text, cls]) => h("span", { class: `jchip ${cls}`, text });

/** How a caller reached the gate (a task's `via`), in words. */
export const VIA_WORDS = Object.freeze({ console: "this console", api_key: "an API key", mcp: "an MCP client", node: "a node", anonymous: "anonymous" });

/** An id, short: its end, "…6f0ac9dd". A UUIDv7 starts with its millisecond, so ids minted together share their
 * start and differ only at the end (its random part); the full id goes in the element's title and copy button. */
export const shortId = (id) => (typeof id === "string" && id.length > 12 ? `…${id.slice(-8)}` : String(id || ""));

/** Seconds -> "24h", "30m", "1d 2h", "90s". */
export function span(seconds) {
  const s = Math.max(0, Math.floor(Number(seconds) || 0));
  if (s === 0) return "0s";
  const d = Math.floor(s / 86400);
  const hh = Math.floor((s % 86400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  const r = s % 60;
  const parts = [];
  if (d) parts.push(`${d}d`);
  if (hh) parts.push(`${hh}h`);
  if (m) parts.push(`${m}m`);
  if (r) parts.push(`${r}s`);
  return parts.slice(0, 2).join(" ");
}

/** How long ago `iso` was, in words ("4s", "2h 14m", "3d"), or "—". */
export function ago(iso, now = Date.now()) {
  if (!iso) return "—";
  const t = new Date(iso).getTime();
  if (Number.isNaN(t)) return "—";
  return span(Math.max(0, (now - t) / 1000));
}

/** A table in a card: `head` the column names, `rows` the <tr> nodes, `none` what an empty list says. */
export function table(head, rows, none, { id = null } = {}) {
  if (!rows.length) return h("p", { class: "empty flush", text: none });
  return h("div", { class: "tablewrap" }, [h("table", { id }, [
    h("thead", {}, [h("tr", {}, head.map((x) => h("th", { text: x })))]),
    h("tbody", {}, rows),
  ])]);
}

/** A list page's state: the items read so far, the cursor of the next page, and a Load more
 * button that reads it with `load(before)` ({ items, next }). `draw()` redraws the page. */
export function pager(load, draw, isCurrent) {
  const st = { items: [], next: null, error: null };
  st.more = h("button", { class: "btn sm", type: "button", text: "Load more", hidden: true });
  st.read = async (first = false) => {
    try {
      const page = await load(first ? undefined : st.next);
      if (!isCurrent()) return;
      st.items = first ? page.items || [] : st.items.concat(page.items || []);
      st.next = page.next || null;
      st.error = null;
    } catch (err) {
      if (!isCurrent()) return;
      st.error = err;
    }
    st.more.hidden = !st.next;
    draw();
  };
  st.more.addEventListener("click", () => st.read(false));
  return st;
}
