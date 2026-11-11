// Rules the console's code keeps, checked on its text: it never parses a string as HTML and
// stores nothing in localStorage; the page has no inline script, style or handler and loads
// nothing from another origin; and every request goes through a row of routes.js.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import test from "node:test";
import { STATIC, load, sources } from "./helpers.mjs";

test("the console never parses strings as HTML or stores secrets in localStorage", () => {
  const forbidden = [
    "innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function",
    "localStorage", 'setTimeout("', "createContextualFragment", "DOMParser",
  ];
  for (const [name, text] of Object.entries(sources())) {
    for (const word of forbidden) assert.ok(!text.includes(word), `${name} uses ${word}`);
    assert.doesNotMatch(text, /setAttribute\(\s*["'](style|on\w+)/, name);
  }
});

test("index.html has no inline script, style or handler, and loads nothing from another origin", () => {
  const html = readFileSync(join(STATIC, "index.html"), "utf8");
  for (const m of html.matchAll(/<script\b([^>]*)>(.*?)<\/script>/gs)) {
    assert.match(m[1], /src=/, m[0]);
    assert.equal(m[2].trim(), "", m[0]);
  }
  assert.doesNotMatch(html, /\sstyle\s*=/);
  assert.doesNotMatch(html, /\son[a-z]+\s*=/);
  assert.ok(!html.includes("<style"));
  for (const m of html.matchAll(/\s(?:src|href)="([^"]+)"/g)) {
    assert.ok(!/^(?:https?:|\/\/)/.test(m[1]), m[1]);
  }
  // No sign-in markup: the local profile has none.
  assert.doesNotMatch(html, /signin|password|type="email"/i);
  const css = readFileSync(join(STATIC, "console.css"), "utf8");
  assert.ok(!css.includes("@import"));
  assert.ok(!css.includes("url(http"));
  assert.ok(!css.includes("url(//"));
});

test("no module but routes.js names a /v1/ or an A2A path, and only api.js calls fetch", () => {
  for (const [name, text] of Object.entries(sources())) {
    const literals = text.match(/"(?:[^"\\\n]|\\.)*"|'(?:[^'\\\n]|\\.)*'|`[^`]*`/g) || [];
    if (name !== "routes.js") assert.deepEqual(literals.filter((l) => l.includes("/v1/") || l.includes("/.well-known") || l.includes("/agents/")), [], name);
    if (name !== "api.js") assert.doesNotMatch(text, /\bfetch\(/, name);
    assert.ok(!text.includes("XMLHttpRequest"), name);
    assert.ok(!text.includes("EventSource"), name);
    assert.ok(!text.includes("Authorization"), name);
  }
});

test("the console calls only the operations of routes.js, and api.js calls each of them", async () => {
  const { ROUTES } = await load("routes.js");
  const { Gate } = await load("api.js");
  const src = sources();
  assert.deepEqual(Object.keys(ROUTES).sort(), [
    "calls", "cancel_task", "context", "context_timeline", "contexts", "continue_task", "conversation", "conversations",
    "exchange", "exchanges", "info", "service", "service_cards", "services", "start_form", "start_task", "task",
    "task_events", "task_file", "task_message", "task_timeline", "tasks", "tunnels",
  ]);
  // api.js names only rows of the table, and every row.
  const named = new Set([...src["api.js"].matchAll(/this\.(?:call|send)\("([a-z_]+)"/g)].map((m) => m[1]));
  for (const n of named) assert.ok(Object.hasOwn(ROUTES, n), `api.js calls ${n}, which routes.js lacks`);
  assert.deepEqual([...named].sort(), Object.keys(ROUTES).sort());
  // Every gate method a view or module calls is one of the client's.
  const methods = new Set(Object.getOwnPropertyNames(Gate.prototype));
  for (const [name, text] of Object.entries(src)) {
    for (const m of text.matchAll(/\b(?:gate|client)\.([A-Za-z]+)\(/g)) {
      assert.ok(methods.has(m[1]), `${name} calls gate.${m[1]}, which api.js does not offer`);
    }
  }
  // As sent: same origin, no credential, no redirect; a POST is JSON, even with nothing to say.
  const sent = [];
  const gate = new Gate({ fetch: async (url, init) => { sent.push([url, init]); return new Response("{}", { status: 200 }); } });
  await gate.tasks({ state: "input-required", limit: 5 });
  await gate.cancelTask("t/1");
  await gate.startForm("hello");
  await gate.conversation("svc", "anon", "ctx 1");
  await gate.exchanges({ failed: true });
  assert.deepEqual(sent.map(([u]) => u), [
    "/v1/tasks?state=input-required&limit=5", "/v1/tasks/t%2F1/cancel", "/v1/services/hello/form",
    "/v1/conversations/svc/anon/ctx%201", "/v1/exchanges?failed=1",
  ]);
  for (const [, init] of sent) {
    assert.equal(init.credentials, "omit");
    assert.equal(init.redirect, "error");
    assert.ok(!Object.hasOwn(init.headers, "Authorization"));
  }
  assert.equal(sent[1][1].method, "POST");
  assert.equal(sent[1][1].headers["Content-Type"], "application/json");
  assert.equal(sent[1][1].body, "{}");
  assert.equal(sent[0][1].body, undefined);
});

test("a refusal reads as a GateError, flat or under `error`", async () => {
  const { Gate, GateError } = await load("api.js");
  const answer = (status, body) => new Gate({ fetch: async () => new Response(typeof body === "string" ? body : JSON.stringify(body), { status }) });
  const caught = async (g) => { try { await g.task("x"); } catch (e) { return e; } return null; };
  const flat = await caught(answer(404, { code: "task_not_found", message: "no such task" }));
  const nested = await caught(answer(403, { error: { code: "foreign_origin", message: "refused" } }));
  const bare = await caught(answer(502, "<html>"));
  assert.ok(flat instanceof GateError);
  assert.deepEqual([flat.status, flat.code, flat.message], [404, "task_not_found", "no such task"]);
  assert.deepEqual([nested.status, nested.code, nested.message], [403, "foreign_origin", "refused"]);
  assert.deepEqual([bare.status, bare.code, bare.message], [502, "http_error", "HTTP 502"]);
});

test("the playground's A2A routes are A2A 1.0's eleven methods, sent as a stock client sends them", async () => {
  const { A2A_ROUTES, a2aPath } = await load("routes.js");
  const { Gate } = await load("api.js");
  assert.deepEqual(Object.keys(A2A_ROUTES).sort(), [
    "CancelTask", "CreateTaskPushNotificationConfig", "DeleteTaskPushNotificationConfig", "GetExtendedAgentCard",
    "GetTask", "GetTaskPushNotificationConfig", "ListTaskPushNotificationConfigs", "ListTasks", "SendMessage",
    "SendStreamingMessage", "SubscribeToTask",
  ]);
  assert.equal(a2aPath("jsonrpc", "GetTask", { service: "hello" }), "/agents/hello");
  assert.equal(a2aPath("rest", "GetTaskPushNotificationConfig", { service: "hello", task_id: "t/1", id: "p" }), "/agents/hello/tasks/t%2F1/pushNotificationConfigs/p");
  assert.throws(() => a2aPath("rest", "GetTask", { service: "hello" }), /needs id/);
  const sent = [];
  const gate = new Gate({ fetch: async (url, init) => { sent.push([url, init]); return new Response("{}", { status: 200 }); } });
  await gate.a2a("hello", "jsonrpc", "ListTasks", { body: { jsonrpc: "2.0", id: 1, method: "ListTasks", params: {} } });
  await gate.a2a("hello", "rest", "SubscribeToTask", { params: { id: "t1" }, lastEventId: 3, extensions: ["https://x/ext"] });
  assert.deepEqual(sent.map(([u, i]) => [u, i.method]), [["/agents/hello", "POST"], ["/agents/hello/tasks/t1:subscribe", "GET"]]);
  for (const [, init] of sent) {
    assert.equal(init.credentials, "omit");
    assert.equal(init.redirect, "error");
    assert.equal(init.headers["A2A-Version"], "1.0");
    assert.ok(!Object.hasOwn(init.headers, "Authorization"));
  }
  assert.equal(sent[1][1].headers["Last-Event-ID"], "3");
  assert.equal(sent[1][1].headers.Accept, "text/event-stream");
  assert.equal(sent[1][1].headers["A2A-Extensions"], "https://x/ext");
  assert.equal(sent[1][1].body, undefined);
});
