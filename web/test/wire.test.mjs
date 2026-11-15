// js/taskevents.js's A2A wire (D357, D360): every item labelled `<party> · <A2A type>`, the party by its name (the
// principal, the service, or Gate) from the gate's record of its origin, the object the principal received beside it, a conversion shown, the ids and the routing members of a
// message; the spec's scenarios of "A2A Labels On Every Item" and "Caller-Facing Object Shown".

import assert from "node:assert/strict";
import test from "node:test";
import { load } from "./helpers.mjs";

const T = "01a1cad2-aa81-7d3b-ae21-cdd7f6adc9dd";
const P = { principal: "anonymous", service: "red" };

test("a streamed task: SendStreamingMessage, then the callee's status and artifact updates, each by its name", async () => {
  const { wireOf } = await load("taskevents.js");
  const ev = (kind, payload, origin) => ({ kind, payload, origin, caller_type: undefined });
  const items = [
    ev("task", { task: { id: T, contextId: "c1", status: { state: "TASK_STATE_SUBMITTED" } } },
      { actor: "caller", type: "SendStreamingMessage", method: "SendStreamingMessage", context_assigned: true }),
    ev("status", { statusUpdate: { taskId: T, contextId: "c1", status: { state: "TASK_STATE_WORKING" } } },
      { actor: "callee", type: "TaskStatusUpdateEvent", method: "SendStreamingMessage", exchange_id: "x1" }),
    ev("artifact", { artifactUpdate: { taskId: T, contextId: "c1", artifact: { artifactId: "a" } } },
      { actor: "callee", type: "TaskArtifactUpdateEvent", method: "SendStreamingMessage", exchange_id: "x1" }),
    ev("status", { statusUpdate: { taskId: T, contextId: "c1", status: { state: "TASK_STATE_COMPLETED" } } },
      { actor: "callee", type: "TaskStatusUpdateEvent", method: "SendStreamingMessage", exchange_id: "x1" }),
  ].map((e) => wireOf(e, P));
  assert.deepEqual(items.map((w) => w.label), [
    "anonymous · SendStreamingMessage", "red · TaskStatusUpdateEvent", "red · TaskArtifactUpdateEvent", "red · TaskStatusUpdateEvent",
  ]);
  assert.deepEqual(items.map((w) => [w.party, w.typeName]).slice(0, 2), [["anonymous", "SendStreamingMessage"], ["red", "TaskStatusUpdateEvent"]]);
  assert.deepEqual(items.map((w) => w.callerType), ["Task", "TaskStatusUpdateEvent", "TaskArtifactUpdateEvent", "TaskStatusUpdateEvent"]);
  assert.deepEqual(items[1].ids, { taskId: T, contextId: "c1" });
  assert.equal(items[1].exchangeId, "x1");
  assert.equal(items[0].contextAssigned, true);
  assert.ok(items.every((w) => w.recorded && !w.convertedTo));
});

test("the service answered an exposed task with a Message: red · Message, delivered as a TaskStatusUpdateEvent (D362)", async () => {
  const { wireOf } = await load("taskevents.js");
  const w = wireOf({ kind: "status", caller_type: "TaskStatusUpdateEvent",
    payload: { statusUpdate: { taskId: T, contextId: "c1", status: { state: "TASK_STATE_COMPLETED", message: { messageId: "m", role: "ROLE_AGENT", parts: [] } } } },
    origin: { actor: "callee", type: "Message", method: "SendMessage", converted_to: "TaskStatusUpdateEvent" } }, P);
  assert.equal(w.label, "red · Message");
  assert.equal(w.callerType, "TaskStatusUpdateEvent");
  assert.equal(w.convertedTo, "TaskStatusUpdateEvent");
});

test("a follow-up: key:kk_ab12cd34 · SendMessage, with the message's taskId and contextId", async () => {
  const { wireOf, routingOf } = await load("taskevents.js");
  const m = { messageId: "m2", role: "ROLE_USER", taskId: T, contextId: "c1", parts: [{ text: "the blue one" }] };
  const w = wireOf({ kind: "message", payload: { message: m }, origin: { actor: "caller", type: "SendMessage", method: "SendMessage" } }, { principal: "key:kk_ab12cd34", service: "notes" });
  assert.equal(w.label, "key:kk_ab12cd34 · SendMessage");
  assert.equal(w.callerType, "Message");
  assert.deepEqual(w.ids, { taskId: T, contextId: "c1" });
  assert.deepEqual(routingOf(m), { contextId: "c1", taskId: T, referenceTaskIds: [] });
  assert.deepEqual(routingOf({ contextId: "c1", referenceTaskIds: ["t1", 3] }), { contextId: "c1", taskId: null, referenceTaskIds: ["t1"] });
});

test("the gate's own events, events from before origins were recorded, and calls that carried no event", async () => {
  const { wireOf, wireOfExchange, validationWords } = await load("taskevents.js");
  const gate = wireOf({ kind: "status", payload: { statusUpdate: { taskId: T, status: { state: "TASK_STATE_FAILED" } } },
    origin: { actor: "gate", type: "TaskStatusUpdateEvent" }, validation: { version: "1.0", validated: true, valid: false, issues: [{ path: "$.statusUpdate.contextId" }] } });
  assert.equal(gate.label, "Gate · TaskStatusUpdateEvent");
  assert.equal(gate.party, "Gate");
  assert.equal(validationWords(gate.validation), "invalid A2A 1.0: 1 issue");
  const old = wireOf({ kind: "status", payload: { statusUpdate: { taskId: T } }, origin: null });
  assert.equal(old.recorded, false);
  assert.equal(old.label, "TaskStatusUpdateEvent");
  assert.equal(wireOfExchange({ leg: "caller", method: "GetTask", id: "x", http_status: 200 }, P).label, "anonymous · GetTask");
  // An exchange names its own principal and service when the view passes none.
  assert.equal(wireOfExchange({ leg: "caller", method: "GetTask", id: "x", principal: "key:kk_ab12cd34", service: "notes" }).label, "key:kk_ab12cd34 · GetTask");
  assert.equal(wireOfExchange({ leg: "caller", method: "GetTask", id: "x" }).label, "unknown · GetTask");
  const up = wireOfExchange({ leg: "callee", method: "GetTask", id: "y", http_status: 502, reason: "upstream_unreachable", failed: true }, P);
  assert.equal(up.label, "Gate → red · GetTask");
  assert.equal(up.outcome, "502 · upstream_unreachable");
  assert.equal(validationWords({ version: "1.0", validated: true, valid: true }), "valid A2A 1.0");
  assert.equal(validationWords({ version: "1.0", validated: false, note: "not validated: truncated" }), "not validated: truncated");
});

test("the playground takes an HTTP+JSON call's path ids from its params, the rest as its query or body", async () => {
  const { restParts, draft } = await load("views/playground.js");
  assert.deepEqual(restParts("SubscribeToTask", { id: "t1" }), { params: { id: "t1" }, query: {}, body: undefined });
  assert.deepEqual(restParts("GetTask", { id: "t1", historyLength: 3 }), { params: { id: "t1" }, query: { historyLength: 3 }, body: undefined });
  assert.deepEqual(restParts("CreateTaskPushNotificationConfig", { taskId: "t1", url: "u" }), { params: { task_id: "t1" }, query: {}, body: { url: "u" } });
  assert.equal(JSON.parse(draft("jsonrpc", "ListTasks")).method, "ListTasks");
});
