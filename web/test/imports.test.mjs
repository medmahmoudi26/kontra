// The module graph is closed: every relative import of js/**/*.js names a file that exists, and no
// module imports one of those the local console left out (sign-in, keys, money, the catalog, the
// withdrawn input-schema extension).

import assert from "node:assert/strict";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join, relative, resolve } from "node:path";
import test from "node:test";
import { JS, code, walk } from "./helpers.mjs";

const REMOVED = [
  "inputschema.js", "prices.js", "session.js", "devicekey.js", "jobkeys.js", "keysync.js", "confirm.js",
  "hire.js", "login.js", "passkey.js", "consent.js", "listing.js", "mcpconfig.js", "age.js",
  "views/apikeys.js", "views/billing.js", "views/catalog.js", "views/disputes.js", "views/devices.js",
  "views/network.js", "views/oauth.js", "views/passkeys.js", "views/recipients.js", "views/service.js",
  "views/signin.js",
];

/** The specifiers a module imports: static `import … from "x"`, `export … from "x"`, bare
 * `import "x"`, and dynamic `import("x")`. */
function specifiers(text) {
  const src = code(text);
  const out = [];
  for (const re of [/\b(?:import|export)\s[^;]*?\bfrom\s*["']([^"']+)["']/gs, /\bimport\s*["']([^"']+)["']/g, /\bimport\(\s*["']([^"']+)["']\s*\)/g]) {
    for (const m of src.matchAll(re)) out.push(m[1]);
  }
  return out;
}

test("every relative import names a file that exists", () => {
  const files = walk(JS);
  assert.ok(files.length >= 20, `only ${files.length} modules`);
  let n = 0;
  for (const file of files) {
    for (const spec of specifiers(readFileSync(file, "utf8"))) {
      if (!spec.startsWith(".")) continue; // vendor bundle's own specifiers are not ours
      n += 1;
      const target = resolve(dirname(file), spec);
      assert.ok(existsSync(target), `${relative(JS, file)} imports ${spec}, which does not exist`);
    }
  }
  assert.ok(n > 40, `only ${n} relative imports found: the parser missed some`);
});

test("no module imports a removed module, and none of them is there", () => {
  for (const file of walk(JS)) {
    for (const spec of specifiers(readFileSync(file, "utf8"))) {
      if (!spec.startsWith(".")) continue;
      const target = relative(JS, resolve(dirname(file), spec)).split("\\").join("/");
      assert.ok(!REMOVED.includes(target), `${relative(JS, file)} imports the removed ${target}`);
    }
  }
  for (const r of REMOVED) assert.ok(!existsSync(join(JS, r)), `${r} is still in js/`);
});

test("the graph from main.js reaches every module but the lazily loaded renderer's own", () => {
  const seen = new Set();
  const visit = (file) => {
    if (seen.has(file)) return;
    seen.add(file);
    for (const spec of specifiers(readFileSync(file, "utf8"))) {
      if (spec.startsWith(".") && spec.endsWith(".js") && !spec.includes("/vendor/")) visit(resolve(dirname(file), spec));
    }
  };
  visit(join(JS, "main.js"));
  const unreached = walk(JS).filter((f) => !seen.has(f)).map((f) => relative(JS, f));
  assert.deepEqual(unreached, [], `modules nothing imports: ${unreached.join(", ")}`);
});

test("every named import is an export of its module", async () => {
  // Loading a module links it: a named import its target lacks fails the load. main.js (it starts
  // the page) and a2ui.js (the vendored Lit renderer needs a DOM) are checked by their text instead.
  const { pathToFileURL } = await import("node:url");
  const exportsOf = new Map();
  for (const file of walk(JS)) {
    const rel = relative(JS, file).split("\\").join("/");
    if (rel === "main.js" || rel === "a2ui.js") continue;
    exportsOf.set(file, new Set(Object.keys(await import(pathToFileURL(file).href))));
  }
  for (const rel of ["main.js", "a2ui.js"]) {
    const file = join(JS, rel);
    for (const m of code(readFileSync(file, "utf8")).matchAll(/\bimport\s*\{([^}]*)\}\s*from\s*["'](\.[^"']+)["']/g)) {
      const target = resolve(dirname(file), m[2]);
      if (target.includes(`${join("static", "vendor")}`)) continue;
      const names = m[1].split(",").map((s) => s.trim().split(/\s+as\s+/)[0]).filter(Boolean);
      for (const n of names) assert.ok(exportsOf.get(target).has(n), `${rel} imports ${n} from ${m[2]}, which does not export it`);
    }
  }
});
