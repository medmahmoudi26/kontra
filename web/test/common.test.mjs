// js/views/common.js: short ids keep their end (spec: Identifiers Shown In Full, "Ids minted in the same millisecond").

import assert from "node:assert/strict";
import test from "node:test";
import { load } from "./helpers.mjs";

test("two UUIDv7s minted in the same millisecond get different short forms", async () => {
  const { shortId } = await load("views/common.js");
  const task = "01a1c7e2-9b10-7c3d-8e4f-5a6b7c8d9e0f";
  const ctx = "01a1c7e2-9b10-7a11-b2c3-d4e5f6a7b8c9";
  assert.equal(task.slice(0, 12), ctx.slice(0, 12));
  assert.notEqual(shortId(task), shortId(ctx));
  assert.equal(shortId(task), "…7c8d9e0f");
});

test("a short id is shown whole, and nothing is shown as nothing", async () => {
  const { shortId } = await load("views/common.js");
  assert.equal(shortId("t-0001"), "t-0001");
  assert.equal(shortId("a-twelve-chr"), "a-twelve-chr");
  assert.equal(shortId(""), "");
  assert.equal(shortId(undefined), "");
});
