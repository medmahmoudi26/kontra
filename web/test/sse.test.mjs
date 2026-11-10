// js/sse.js reads the task-events stream as the HTML standard's "event stream" interpretation.

import assert from "node:assert/strict";
import test from "node:test";
import { load } from "./helpers.mjs";

test("the event stream is read as the HTML standard reads it", async () => {
  const { readEvents } = await load("sse.js");
  const enc = new TextEncoder();
  const stream = (chunks) => new ReadableStream({
    start(c) { for (const x of chunks) c.enqueue(typeof x === "string" ? enc.encode(x) : x); c.close(); },
  });
  const all = async (chunks) => { const out = []; for await (const e of readEvents(stream(chunks))) out.push(e); return out; };
  const bytes = enc.encode("data: é\n\n");
  // Lines end with CRLF, LF or CR, even split across chunks; comments are skipped; data lines join
  // with LF; an event with no data is not dispatched; one cut short by the stream's end is dropped;
  // the last id is carried.
  assert.deepEqual(await all(["retry: 3000\n\nid: 1\nevent: sta", "tus\ndata: {\"seq\":1,", "\"kind\":\"status\"}\n\n"]),
    [{ id: "1", event: "status", data: '{"seq":1,"kind":"status"}' }]);
  assert.deepEqual(await all(["id: 2\r", "\ndata: a\r\ndata: b\r", "\n\r\n"]), [{ id: "2", event: "message", data: "a\nb" }]);
  assert.deepEqual(await all(["data: x\r\r: keep-alive\r\r"]), [{ id: null, event: "message", data: "x" }]);
  assert.deepEqual(await all(["event: ping\n\n", ": comment\n\n"]), []);
  assert.deepEqual(await all(["data: whole\n\n", "data: half"]), [{ id: null, event: "message", data: "whole" }]);
  assert.deepEqual(await all(["id: 7\ndata: {}\n\n", "event: end\ndata: {\"state\":\"completed\",\"version\":7}\n\n"]), [
    { id: "7", event: "message", data: "{}" },
    { id: "7", event: "end", data: '{"state":"completed","version":7}' },
  ]);
  assert.deepEqual(await all(["data:tight\n\n"]), [{ id: null, event: "message", data: "tight" }]);
  assert.deepEqual(await all([bytes.slice(0, 7), bytes.slice(7)]), [{ id: null, event: "message", data: "é" }]); // é split
});

test("the task-events rows reach describe() with their seq, kind and payload", async () => {
  const { readEvents } = await load("sse.js");
  const { describe } = await load("taskevents.js");
  const row = { seq: 4, kind: "status", at: "2026-11-10T10:00:00Z", stored_apart: false, payload: { statusUpdate: { status: { state: "TASK_STATE_WORKING" } } } };
  const body = new ReadableStream({ start(c) { c.enqueue(new TextEncoder().encode(`id: 4\ndata: ${JSON.stringify(row)}\n\n`)); c.close(); } });
  const got = [];
  for await (const ev of readEvents(body)) got.push(JSON.parse(ev.data));
  assert.deepEqual(got, [row]);
  const d = describe(got[0]);
  assert.equal(d.state, "working");
  assert.equal(d.line, "Now the agent is working");
});
