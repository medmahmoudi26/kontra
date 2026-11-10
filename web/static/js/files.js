// An agent's files on the thread: read, opened and saved in the page.
//
// A file part's bytes come inline (A2A 1.0 `raw`, v0.3 `file.bytes`: base64, decoded here) or, for
// a part over 256 KiB, as the gate's `kontra-blob:<ref>`, which the console API serves for that
// task only (`task_file`). **Download** is a Blob of type
// application/octet-stream behind an object URL and an `a[download]` that carries the filename made
// safe; **Open** shows JSON pretty-printed and text (Markdown, CSV, YAML, XML, NDJSON...) as plain
// text, through textContent only, never HTML, at most 1 MiB of it. No image is previewed (the CSP's
// img-src stays 'self'), and nothing is ever navigated to.

import { errorFrom } from "./api.js";

/** The most of a file the page shows as text. */
export const PREVIEW_MAX = 1024 * 1024;
/** A blob ref of the gate's (base32, lower case, 26 characters). */
export const REF_RE = /^[a-z2-7]{26}$/;
const NAME_MAX = 150;

/** The bytes of base64 text (standard or URL-safe, padded or not, whitespace ignored), or null. */
export function decodeBase64(text) {
  if (typeof text !== "string") return null;
  const s = text.replace(/\s+/g, "").replaceAll("-", "+").replaceAll("_", "/");
  if (!/^[A-Za-z0-9+/]*={0,2}$/.test(s) || s.replace(/=+$/, "").length % 4 === 1) return null;
  const padded = s.replace(/=+$/, "") + "===".slice(0, (4 - (s.replace(/=+$/, "").length % 4)) % 4);
  let bin;
  try {
    bin = atob(padded);
  } catch {
    return null;
  }
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

/** How many bytes base64 text decodes to (without decoding it). */
export function base64Size(text) {
  if (typeof text !== "string") return 0;
  const s = text.replace(/\s+/g, "").replace(/=+$/, "");
  return Math.floor((s.length * 3) / 4);
}

/** Several chunks' bytes, in order, as one. */
export function joinBytes(chunks) {
  const total = chunks.reduce((n, c) => n + c.length, 0);
  const out = new Uint8Array(total);
  let at = 0;
  for (const c of chunks) {
    out.set(c, at);
    at += c.length;
  }
  return out;
}

/** A filename to save under: no path separator or control character, no leading dot, at most 150
 * characters; "file" when nothing is left. */
export function safeName(name) {
  const raw = typeof name === "string" ? name : "";
  // eslint-disable-next-line no-control-regex
  const cleaned = raw.replace(/[\x00-\x1f\x7f/\\]/g, "_").trim().replace(/^\.+/, "").trim();
  return cleaned.slice(0, NAME_MAX) || "file";
}

const bare = (type) => (typeof type === "string" ? type.split(";")[0].trim().toLowerCase() : "");

/** A JSON media type: application/json, any +json, text/json. */
export function isJson(type) {
  const t = bare(type);
  return t === "application/json" || t === "text/json" || t.endsWith("+json");
}

const TEXTUAL = new Set([
  "application/xml", "application/x-ndjson", "application/ndjson", "application/jsonl", "application/x-yaml",
  "application/yaml", "application/toml", "application/csv", "application/sql", "application/javascript",
  "application/x-sh", "application/graphql",
]);

/** A type the page may show as text: every text/*, JSON, XML, YAML, CSV, NDJSON and their kin. A
 * type it does not know is not shown (it is downloaded). */
export function isTextual(type) {
  const t = bare(type);
  return t.startsWith("text/") || isJson(t) || t.endsWith("+xml") || TEXTUAL.has(t);
}

/** What Open shows of `bytes` of `type`: { text, more } -- the first PREVIEW_MAX bytes as UTF-8,
 * JSON pretty-printed when the whole of it parses; `more` when the file is longer. */
export function previewOf(bytes, type) {
  const more = bytes.length > PREVIEW_MAX;
  const head = more ? bytes.subarray(0, PREVIEW_MAX) : bytes;
  const text = new TextDecoder("utf-8", { fatal: false }).decode(head);
  if (!more && isJson(type)) {
    try {
      return { text: JSON.stringify(JSON.parse(text), null, 2), more };
    } catch {
      // shown as it came
    }
  }
  return { text, more };
}

/** "6.1 KB", "411 KB", "2.3 MB", "120 bytes". */
export function sizeText(n) {
  if (typeof n !== "number" || !Number.isFinite(n) || n < 0) return "";
  if (n < 1000) return `${n} byte${n === 1 ? "" : "s"}`;
  if (n < 1e6) return `${(n / 1e3).toFixed(n < 1e4 ? 1 : 0)} KB`;
  return `${(n / 1e6).toFixed(n < 1e7 ? 1 : 0)} MB`;
}

/** Save `bytes` as `name` (made safe): a Blob of type application/octet-stream, so the browser
 * never renders it, behind an object URL revoked after a minute. */
export function saveBytes(bytes, name) {
  const url = URL.createObjectURL(new Blob([bytes], { type: "application/octet-stream" }));
  const a = document.createElement("a");
  a.href = url;
  a.download = safeName(name);
  a.rel = "noopener";
  a.hidden = true;
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 60_000);
}

/** The bytes of the gate's file `ref` of task `taskId`, fetched through the console API
 * (`task_file`); a GateError when it refuses. */
export async function fetchFile(client, taskId, ref, { signal } = {}) {
  if (!REF_RE.test(ref)) throw new Error("not a file of the gate's");
  const resp = await client.taskFile(taskId, ref, { signal });
  if (!resp.ok) throw await errorFrom(resp);
  return new Uint8Array(await resp.arrayBuffer());
}

/** A file's bytes in order: `chunks` ([{raw} | {ref}], a result's joined chunks) or, for a part
 * seen alone, its own `raw` or `ref`. */
export function piecesOf(f) {
  if (Array.isArray(f.chunks) && f.chunks.length) return f.chunks;
  if (typeof f.raw === "string") return [{ raw: f.raw }];
  if (f.ref) return [{ ref: f.ref }];
  return [];
}

/** The size of a file's pieces once decoded, or null while one is kept by the gate (fetched on
 * Download or Open). */
export function piecesSize(pieces) {
  if (pieces.some((p) => p.ref)) return null;
  return pieces.reduce((n, p) => n + base64Size(p.raw), 0);
}

/** A file's bytes: inline pieces decoded, the gate's fetched through the console API one at a
 * time, joined in order. */
export async function readPieces(client, taskId, pieces, { signal } = {}) {
  const out = [];
  for (const p of pieces) {
    if (p.ref) {
      out.push(await fetchFile(client, taskId, p.ref, { signal }));
    } else {
      const b = decodeBase64(p.raw);
      if (b === null) throw new Error("the agent's bytes are not base64");
      out.push(b);
    }
  }
  return out.length === 1 ? out[0] : joinBytes(out);
}

/** An https link's host, for "the agent's link": null for anything that is not https. */
export function httpsHost(url) {
  if (typeof url !== "string") return null;
  try {
    const u = new URL(url);
    return u.protocol === "https:" && u.host ? u.host : null;
  } catch {
    return null;
  }
}
