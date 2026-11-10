// js/taskevents.js and js/files.js: what an event says, results folded by id as A2A folds them,
// files read and named safely, the node's reconnect wait, and sealed parts kept as envelopes.

import assert from "node:assert/strict";
import test from "node:test";
import { load } from "./helpers.mjs";

test("results keep their id and how they fold", async () => {
  const { describe, foldResult } = await load("taskevents.js");
  const ev = (a, more = {}) => describe({ seq: 1, kind: "artifact", payload: { artifactUpdate: { artifact: a, ...more } } });
  const art = (a, more = {}) => ev(a, more).artifacts[0];
  const f = (raw, name = "log.txt", type = "text/plain") => ({ raw, filename: name, mediaType: type });
  const blob = (ref, name = "big.bin") => ({ url: "kontra-blob:" + ref, filename: name, mediaType: "application/octet-stream" });
  const A = "a".repeat(26), B = "b".repeat(26), C = "c".repeat(26);
  const fold = (events) => events.reduce((r, a) => foldResult(r, a), null);
  const show = (r) => ({ name: r.name, chunked: r.chunked, lastChunk: r.lastChunk, parts: r.parts.map((p) => (p.file ? { name: p.file.name, chunks: p.file.chunks } : Object.keys(p).sort().join(","))) });
  const first = foldResult(null, art({ artifactId: "r1", name: "log.txt", parts: [f("YQ==")] }));
  const second = foldResult(first, art({ artifactId: "r1", parts: [f("Yg==")] }, { append: true }));

  assert.deepEqual([
    ev({ artifactId: "r1", name: "log.txt", parts: [f("YQ==")] }),
    ev({ artifactId: "r1", parts: [f("Yg==")] }, { append: true, lastChunk: true }),
  ].map((d) => [d.line, d.artifacts.map((a) => [a.id, a.name, a.append, a.lastChunk])]), [
    ["A result: log.txt", [["r1", "log.txt", false, null]]],
    ["A result: r1", [["r1", "r1", true, true]]],
  ]);
  // Inline chunks joined; `prev` never changed.
  const log = { name: "log.txt", chunks: [{ raw: "YQ==" }, { raw: "Yg==" }] };
  assert.deepEqual(show(second), { name: "log.txt", chunked: true, lastChunk: null, parts: [log] });
  assert.deepEqual(show(first).parts, [{ name: "log.txt", chunks: [{ raw: "YQ==" }] }]);
  // Chunks over 256 KiB come as the gate's refs: joined all the same, in order.
  assert.deepEqual(show(fold([
    art({ artifactId: "r2", name: "big.bin", parts: [blob(A)] }),
    art({ artifactId: "r2", parts: [blob(B)] }, { append: true }),
    art({ artifactId: "r2", parts: [f("Yw==", "big.bin", "application/octet-stream")] }, { append: true }),
    art({ artifactId: "r2", parts: [blob(C)] }, { append: true, lastChunk: true }),
  ])), { name: "big.bin", chunked: true, lastChunk: true, parts: [
    { name: "big.bin", chunks: [{ ref: A }, { ref: B }, { raw: "Yw==" }, { ref: C }] },
  ] });
  // Another name, or a text between, starts another part.
  assert.deepEqual(show(fold([
    art({ artifactId: "r3", name: "two", parts: [blob(A, "x.bin")] }),
    art({ artifactId: "r3", parts: [blob(B, "y.bin")] }, { append: true }),
    art({ artifactId: "r3", parts: [{ text: "between" }, blob(C, "y.bin")] }, { append: true }),
  ])).parts, [
    { name: "x.bin", chunks: [{ ref: A }] },
    { name: "y.bin", chunks: [{ ref: B }] },
    "text",
    { name: "y.bin", chunks: [{ ref: C }] },
  ]);
  // A replace takes the new parts and the new name.
  assert.deepEqual(show(fold([
    art({ artifactId: "r4", name: "draft", parts: [{ text: "v1" }] }),
    art({ artifactId: "r4", name: "final", parts: [{ text: "v2" }] }),
  ])), { name: "final", chunked: false, lastChunk: null, parts: ["text"] });
});

test("files are read and named safely", async () => {
  const f = await load("files.js");
  const { partsOf } = await load("taskevents.js");
  const ref = "abcdefghijklmnopqrstuvwxyz";
  const [raw, blob, link, js, http, v03, v03blob] = partsOf([
    { raw: btoa("hello"), filename: "a.txt", mediaType: "text/plain" },
    { url: "kontra-blob:" + ref, filename: "b.zip", mediaType: "application/zip" },
    { url: "https://files.example/c.pdf", filename: "c.pdf" },
    { url: "javascript:alert(1)", filename: "d" },
    { url: "http://plain.example/e" },
    { kind: "file", file: { bytes: btoa("v03"), name: "old.json", mimeType: "application/json" } },
    { kind: "file", file: { uri: "kontra-blob:" + ref, name: "old.zip" } },
  ]).map((p) => p.file);
  assert.deepEqual([raw.name, raw.type, raw.raw], ["a.txt", "text/plain", "aGVsbG8="]);
  assert.deepEqual([blob.ref, blob.href, blob.url], [ref, null, null]);
  assert.deepEqual([link.href, link.ref], ["https://files.example/c.pdf", null]);
  assert.deepEqual([js.href, js.url], [null, "javascript:alert(1)"]); // text, never a link
  assert.deepEqual([http.href, http.url], [null, "http://plain.example/e"]);
  assert.deepEqual([v03.name, v03.type, v03.raw], ["old.json", "application/json", "djAz"]);
  assert.equal(v03blob.ref, ref);

  const dec = (s) => { const b = f.decodeBase64(s); return b === null ? null : [...b]; };
  assert.deepEqual([dec("aGk="), dec("aGk"), dec("-_8"), dec("+/8="), dec(" aG k= "), dec("@@"), dec("a")],
    [[104, 105], [104, 105], [251, 255], [251, 255], [104, 105], null, null]);
  assert.equal(f.base64Size(btoa("hello world")), 11);
  assert.deepEqual(["../../etc/passwd", ".hidden", "a\u0007b\\c", "", "x".repeat(300), "report.json"].map(f.safeName),
    ["_.._etc_passwd", "hidden", "a_b_c", "file", "x".repeat(150), "report.json"]);
  assert.deepEqual(["application/json", "application/ld+json; charset=utf-8", "text/json", "text/plain"].map(f.isJson), [true, true, true, false]);
  assert.deepEqual(["text/markdown", "text/csv", "application/x-ndjson", "application/yaml", "image/png", "application/zip", "application/xml", ""].map(f.isTextual),
    [true, true, true, true, false, false, true, false]);
  assert.deepEqual(f.previewOf(new TextEncoder().encode('{"a":[1,2]}'), "application/json"), { text: '{\n  "a": [\n    1,\n    2\n  ]\n}', more: false });
  assert.deepEqual(f.previewOf(new TextEncoder().encode("{oops"), "application/json"), { text: "{oops", more: false });
  const cut = f.previewOf(new Uint8Array(f.PREVIEW_MAX + 10).fill(65), "text/plain");
  assert.deepEqual([cut.text.length, cut.more], [1024 * 1024, true]);
  assert.deepEqual([...f.joinBytes([new Uint8Array([1, 2]), new Uint8Array([3])])], [1, 2, 3]);
  assert.deepEqual([0, 1, 999, 6100, 411000, 2300000].map(f.sizeText), ["0 bytes", "1 byte", "999 bytes", "6.1 KB", "411 KB", "2.3 MB"]);
  assert.deepEqual(["https://a.example/x", "http://a.example/x", "javascript:1"].map(f.httpsHost), ["a.example", null, null]);
  assert.deepEqual([f.piecesOf({ raw: "YQ==" }), f.piecesOf({ ref }), f.piecesOf({ href: "https://x" }), f.piecesOf({ chunks: [{ raw: "YQ==" }, { ref }] })],
    [[{ raw: "YQ==" }], [{ ref }], [], [{ raw: "YQ==" }, { ref }]]);
  assert.deepEqual([f.piecesSize([{ raw: "YWJj" }, { raw: "ZA==" }]), f.piecesSize([{ raw: "YQ==" }, { ref }])], [4, null]);

  // A file in chunks, inline and stored apart, read in order, one fetch at a time.
  const asked = [];
  let open = 0;
  let most = 0;
  const client = { taskFile: async (task, r) => {
    open += 1; most = Math.max(most, open); asked.push([task, r]);
    await new Promise((ok) => setTimeout(ok, 5));
    open -= 1;
    return new Response(new Uint8Array(r === ref ? [2, 3] : [4]));
  } };
  const other = "zyxwvutsrqponmlkjihgfedcba";
  const bytes = await f.readPieces(client, "t-1", [{ raw: btoa("\u0001") }, { ref }, { ref: other }]);
  assert.deepEqual([...bytes], [1, 2, 3, 4]);
  assert.deepEqual(asked, [["t-1", ref], ["t-1", other]]);
  assert.equal(most, 1);
  await assert.rejects(f.readPieces(client, "t-1", [{ raw: "@@" }]), { message: "the agent's bytes are not base64" });
  // A ref that is not the gate's is never asked for; a refusal is the API's error.
  await assert.rejects(f.fetchFile(client, "t-1", "../x"), { message: "not a file of the gate's" });
  const refusing = { taskFile: async () => new Response(JSON.stringify({ code: "file_not_found", message: "gone" }), { status: 404 }) };
  await assert.rejects(f.fetchFile(refusing, "t-1", ref), { status: 404, code: "file_not_found" });
});

test("a reconnect wait is read from the status metadata", async () => {
  const { describe, reconnectOf } = await load("taskevents.js");
  const st = (md, state = "TASK_STATE_WORKING") => ({ seq: 2, kind: "status", payload: { statusUpdate: {
    status: { state, message: { role: "ROLE_AGENT", parts: [{ text: "The agent's node disconnected." }] } },
    ...(md === undefined ? {} : { metadata: md }) } } });
  const wait = { "kontra.reconnect": { state: "waiting", since: "2026-11-05T10:00:00Z", until: "2026-11-05T10:15:00Z" } };
  const d = describe(st(wait));
  assert.deepEqual(d.reconnect, { state: "waiting", until: 1793873700000 });
  assert.equal(d.line, "The node disconnected");
  const resumed = describe(st({ "kontra.reconnect": { state: "resumed", at: "2026-11-05T10:03:00Z" } }));
  assert.deepEqual(resumed.reconnect, { state: "resumed", until: null });
  assert.equal(resumed.line, "The node reconnected");
  assert.deepEqual(describe(st({ "kontra.reconnect": { state: "ended", at: "2026-11-05T10:15:02Z" } }, "TASK_STATE_FAILED")).reconnect, { state: "ended", until: null });
  assert.deepEqual([describe(st(undefined)).reconnect ?? null, describe(st({ other: 1 })).reconnect ?? null, "reconnect" in describe(st(undefined))], [null, null, false]);
  assert.deepEqual([
    reconnectOf({ metadata: { "kontra.reconnect": { state: "waiting", until: "soon" } } }),
    reconnectOf({ metadata: { "kontra.reconnect": "waiting" } }),
    reconnectOf(null),
  ], [null, null, null]);
  assert.equal(describe({ seq: 3, kind: "artifact", payload: { artifactUpdate: { artifact: { artifactId: "a", parts: [] } } } }).reconnect ?? null, null);
});

test("a sealed part is an envelope with its metadata, never its content", async () => {
  const { partsOf, sealedOf, SEALED_TYPE } = await load("taskevents.js");
  const meta = { "kontra.sealed": { original: "text/plain" } };
  const parts = partsOf([
    { raw: btoa("ciphertext!"), mediaType: `${SEALED_TYPE};v=1`, filename: "notes.txt", metadata: meta },
    { url: "kontra-blob:" + "a".repeat(26), mediaType: `${SEALED_TYPE};v=1`, metadata: meta },
    { text: "plain", metadata: { other: 1 } },
    // An A2UI mark stays readable on a sealed part: still an envelope.
    { raw: "AAAA", mediaType: `${SEALED_TYPE};v=1`, metadata: { mimeType: "application/a2ui+json", "kontra.sealed": { original: "data" } } },
  ]);
  assert.deepEqual(Object.keys(parts[0]), ["sealed"]);
  assert.deepEqual(parts[0].sealed, { original: "text/plain", name: "notes.txt", type: `${SEALED_TYPE};v=1`, size: 11, metadata: meta });
  assert.equal(parts[1].sealed.size, null);
  assert.deepEqual(parts[2], { text: "plain" });
  assert.equal(parts[3].sealed.original, "data");
  for (const p of [parts[0], parts[1], parts[3]]) assert.ok(!JSON.stringify(p).includes("Y2lwaGVy"), "no ciphertext is kept");
  assert.equal(sealedOf({ text: "x" }), null);
});
