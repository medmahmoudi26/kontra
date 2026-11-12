// js/taskevents.js's runs of an artifact's parts (spec: Streamed Text Shown Continuously, Artifacts Column).

import assert from "node:assert/strict";
import test from "node:test";
import { load } from "./helpers.mjs";

const art = (id, parts, { append = false, lastChunk = null, name = "live" } = {}) =>
  ({ id, name, append, lastChunk, parts, source: [] });

test("a sentence split across appended chunks is one text, nothing trimmed or inserted", async () => {
  const { foldResult, runsOf } = await load("taskevents.js");
  let r = foldResult(null, art("a", [{ text: "Read the co" }]));
  r = foldResult(r, art("a", [{ text: "de. Then" }], { append: true }));
  r = foldResult(r, art("a", [{ text: " curl it" }], { append: true, lastChunk: true }));
  const runs = runsOf(r.parts);
  assert.equal(runs.length, 1);
  assert.equal(runs[0].kind, "text");
  assert.equal(runs[0].text, "Read the code. Then curl it");
  assert.equal(r.lastChunk, true);
});

test("a part's own leading and trailing spaces and newlines are kept", async () => {
  const { runsOf } = await load("taskevents.js");
  assert.equal(runsOf([{ text: "a " }, { text: " b\n" }, { text: "\nc" }])[0].text, "a  b\n\nc");
});

test("text, text, data is two blocks; a new block only where the kind changes", async () => {
  const { runsOf } = await load("taskevents.js");
  const runs = runsOf([{ text: "one " }, { text: "two" }, { data: "{\"a\":1}" }, { data: "{\"b\":2}" }, { text: "three" }]);
  assert.deepEqual(runs.map((x) => x.kind), ["text", "data", "text"]);
  assert.equal(runs[0].text, "one two");
  assert.equal(runs[1].parts.length, 2);
});

test("an A2UI or form part is its own block, never joined", async () => {
  const { runsOf } = await load("taskevents.js");
  const runs = runsOf([{ data: "[]", a2ui: true }, { data: "[]", a2ui: true }]);
  assert.deepEqual(runs.map((x) => x.kind), ["other", "other"]);
});

test("a replacement swaps the parts: the entry shows the second content", async () => {
  const { foldResult, runsOf } = await load("taskevents.js");
  let r = foldResult(null, art("report", [{ text: "draft" }], { name: "report" }));
  r = foldResult(r, art("report", [{ text: "final" }], { name: "report" }));
  assert.equal(runsOf(r.parts)[0].text, "final");
});
