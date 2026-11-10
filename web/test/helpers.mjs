// Shared by the console's node:test files (no external dependencies): where the files are, and
// their code without comments, for rules about code rather than prose.

import { readFileSync, readdirSync } from "node:fs";
import { dirname, join, relative } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

export const STATIC = join(dirname(fileURLToPath(import.meta.url)), "..", "static");
export const JS = join(STATIC, "js");
/** The js/ directory's file URL, for dynamic import(). */
export const JS_URL = pathToFileURL(JS).href;
export const load = (rel) => import(`${JS_URL}/${rel}`);

/** Every .js file under `dir`, recursively, as absolute paths, sorted. */
export function walk(dir) {
  const out = [];
  for (const e of readdirSync(dir, { withFileTypes: true })) {
    const p = join(dir, e.name);
    if (e.isDirectory()) out.push(...walk(p));
    else if (e.name.endsWith(".js")) out.push(p);
  }
  return out.sort();
}

/** `text` without its comments (strings kept). */
export function code(text) {
  let out = "";
  let quote = "";
  for (let i = 0; i < text.length; i++) {
    const c = text[i];
    if (quote) {
      out += c;
      if (c === "\\") {
        out += text[i + 1] ?? "";
        i += 1;
      } else if (c === quote) quote = "";
    } else if (c === '"' || c === "'" || c === "`") {
      quote = c;
      out += c;
    } else if (text.startsWith("//", i)) {
      const j = text.indexOf("\n", i);
      if (j < 0) break;
      i = j - 1;
    } else if (text.startsWith("/*", i)) {
      i = text.indexOf("*/", i) + 1;
    } else out += c;
  }
  return out;
}

/** { "views/thread.js": its code without comments, ... } for every module of js/. */
export function sources() {
  return Object.fromEntries(walk(JS).map((p) => [relative(JS, p).split("\\").join("/"), code(readFileSync(p, "utf8"))]));
}
