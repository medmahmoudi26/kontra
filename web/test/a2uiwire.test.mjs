// js/a2uiwire.js, the DOM-free half of A2UI on the thread: which parts are A2UI, the messages they
// hold, the reply an action makes, and a person's answer read as the form labelled it. (js/a2ui.js
// draws with the vendored Lit renderer, which needs a DOM: browser tests cover it, task 5.5.)

import assert from "node:assert/strict";
import test from "node:test";
import { load } from "./helpers.mjs";

const A2UI_CATALOG = "https://a2ui.org/specification/v0_9/catalogs/basic/catalog.json";
const check = (path, message) => ({ condition: { call: "required", args: { value: { path } } }, message });
/** The A2UI example agent's booking form: a title, a name and a date and time each with a
 * `required` check, a choice of guests, and a Book button whose action carries the three. */
const BOOKING = [
  { version: "v0.9.1", createSurface: { surfaceId: "booking", catalogId: A2UI_CATALOG } },
  { version: "v0.9.1", updateComponents: { surfaceId: "booking", components: [
    { id: "root", component: "Column", children: ["title", "name", "guests", "date", "book"] },
    { id: "title", component: "Text", text: "Book a table", variant: "h2" },
    { id: "name", component: "TextField", label: "Name for the booking", value: { path: "/booking/name" }, checks: [check("/booking/name", "A name is required.")] },
    { id: "guests", component: "ChoicePicker", label: "Guests", variant: "mutuallyExclusive",
      options: [1, 2, 3, 4, 5, 6].map((n) => ({ label: `${n} guest${n > 1 ? "s" : ""}`, value: String(n) })), value: { path: "/booking/guests" } },
    { id: "date", component: "DateTimeInput", label: "Date and time", enableDate: true, enableTime: true, value: { path: "/booking/date" }, checks: [check("/booking/date", "A date and time are required.")] },
    { id: "book_label", component: "Text", text: "Book" },
    { id: "book", component: "Button", variant: "primary", child: "book_label",
      checks: [check("/booking/name", "A name is required."), check("/booking/date", "A date and time are required.")],
      action: { event: { name: "book", context: { name: { path: "/booking/name" }, guests: { path: "/booking/guests" }, date: { path: "/booking/date" } } } } },
  ] } },
  { version: "v0.9.1", updateDataModel: { surfaceId: "booking", path: "/booking", value: { name: "", guests: ["2"], date: "" } } },
];
/** A new surface under the booking form's id, which A2UI allows once that form is deleted. */
const RECEIPT = [
  { version: "v0.9.1", createSurface: { surfaceId: "booking", catalogId: A2UI_CATALOG } },
  { version: "v0.9.1", updateComponents: { surfaceId: "booking", components: [
    { id: "root", component: "Column", children: ["title", "said"] },
    { id: "title", component: "Text", text: "Your receipt", variant: "h2" },
    { id: "said", component: "Text", text: "A table is booked." },
  ] } },
];

test("a2ui parts are found, read and answered", async () => {
  const w = await load("a2uiwire.js");
  const { describe } = await load("taskevents.js");
  const m = { version: "v0.9.1", deleteSurface: { surfaceId: "s" } };
  const action = { name: "greet", surfaceId: "greet", sourceComponentId: "go", timestamp: "2026-11-02T10:00:00Z", context: { name: "Ada" }, catalogId: "x", userMessage: "y" };
  assert.deepEqual([
    { data: [m], metadata: { mimeType: "application/a2ui+json" } },
    { data: [m], mediaType: "application/json+a2ui" },
    { kind: "data", data: m, mimeType: "Application/A2UI+JSON; charset=utf-8" },
    { data: [m], metadata: { mimeType: "application/json" } },
    { text: "x", metadata: { mimeType: "application/a2ui+json" } },
    null,
  ].map(w.isA2uiPart), [true, true, true, false, false, false]);
  assert.deepEqual([[m], m, { messages: [m] }, { value: [m] }, [], [m, 1], "x", { value: [m], more: 1 }, new Array(501).fill(m)].map((d) => w.a2uiMessages(d)),
    [[m], [m], [m], [m], null, null, null, [{ value: [m], more: 1 }], null]);
  assert.deepEqual(w.createdSurfaces([{ createSurface: { surfaceId: "a" } }, m, { createSurface: { surfaceId: 7 } }, { createSurface: { surfaceId: "b" } }]), ["a", "b"]);
  // Exactly A2UI v0.9's five members: nothing else of the renderer's event.
  const sent = w.actionMessage(action, "v0.9.1");
  assert.deepEqual(sent, { version: "v0.9.1", action: { name: "greet", surfaceId: "greet", sourceComponentId: "go", timestamp: "2026-11-02T10:00:00Z", context: { name: "Ada" } } });
  assert.deepEqual([w.actionMessage(action, "1.0"), w.actionMessage({ ...action, name: "" }, "v0.9"), w.actionMessage({ ...action, context: { big: "x".repeat(70000) } }, "v0.9")], [null, null, null]);
  assert.deepEqual(w.actionReply(w.actionMessage(action, "v0.9"), { version: "v0.9", surfaces: { greet: {} } }), {
    a2ui: { ...sent, version: "v0.9" },
    a2ui_catalogs: [A2UI_CATALOG],
    a2ui_data_model: { version: "v0.9", surfaces: { greet: {} } },
  });
  assert.deepEqual(Object.keys(w.actionReply(w.actionMessage(action, "v0.9"))).sort(), ["a2ui", "a2ui_catalogs"]);
  assert.deepEqual(w.textReply("hi"), { text: "hi", a2ui_catalogs: [A2UI_CATALOG] });

  const event = describe({ seq: 3, kind: "status", payload: { statusUpdate: { status: { state: "TASK_STATE_INPUT_REQUIRED", message: { role: "ROLE_AGENT", parts: [
    { text: "Fill it in" },
    { data: [m], metadata: { mimeType: "application/a2ui+json" } },
    { data: { not: "a2ui" }, mediaType: "application/a2ui+json" },
    { data: { q: 1 } },
  ] } } } } });
  const [said] = event.messages;
  assert.deepEqual(said.parts[0], { text: "Fill it in" });
  assert.deepEqual(said.parts[1], { data: JSON.stringify([m], null, 2), a2ui: [m] });
  assert.deepEqual(said.parts[2].a2ui, [{ not: "a2ui" }]); // A2UI's type: the renderer refuses it
  assert.deepEqual(said.parts[3], { data: '{\n  "q": 1\n}', rows: [["q", "1"]] });
  const [mine] = describe({ seq: 4, kind: "message", payload: { message: { role: "ROLE_USER", parts: [
    { data: { version: "v0.9.1", action }, metadata: { mimeType: "application/a2ui+json" } },
  ] } } }).messages;
  assert.equal(mine.role, "you");
  assert.equal(mine.parts[0].actions[0].name, "greet");
  // A status is the agent's, whatever role it names: the person's messages are `message` events.
  assert.deepEqual(describe({ seq: 5, kind: "status", payload: { statusUpdate: { status: { state: "TASK_STATE_INPUT_REQUIRED", message: { role: "ROLE_USER", parts: [
    { data: { version: "v0.9.1", action }, metadata: { mimeType: "application/a2ui+json" } },
  ] } } } } }).messages.map((x) => x.role), ["svc"]);
});

test("an a2ui answer reads as the form labelled it", async () => {
  const w = await load("a2uiwire.js");
  const book = new w.FormBook();
  book.note(BOOKING);
  book.note([{ version: "v0.9.1", deleteSurface: { surfaceId: "booking" } }]);
  const action = { name: "book", surfaceId: "booking", sourceComponentId: "book", timestamp: "2026-11-09T18:00:00Z", context: { date: "2026-11-09T19:30", guests: ["4"], name: "Ada" } };
  const extra = new w.FormBook();
  extra.note([{ version: "v0.9.1", createSurface: { surfaceId: "x", catalogId: "c" } }, { version: "v0.9.1", updateComponents: { surfaceId: "x", components: [
    { id: "ok", component: "CheckBox", label: "Agree", value: { path: "/f/ok" } },
    { id: "n", component: "Slider", label: "Count", value: { path: "/f/n" } },
    { id: "go", component: "Button", child: "gol", action: { event: { name: "send", context: { ok: { path: "/f/ok" }, n: { path: "/f/n" }, lit: "x", missing: { path: "/f/none" } } } } },
    { id: "gol", component: "Text", text: { path: "/label" } },
  ] } }]);
  const renewed = new w.FormBook();
  renewed.note(BOOKING);
  renewed.note([{ version: "v0.9.1", createSurface: { surfaceId: "booking", catalogId: "c" } }]);
  const reused = new w.FormBook();
  reused.note(BOOKING);
  const first = reused.generation("booking");
  reused.note([{ version: "v0.9.1", deleteSurface: { surfaceId: "booking" } }]);
  reused.note(RECEIPT);
  const many = new w.FormBook();
  for (let i = 0; i < 150; i++) many.note([{ createSurface: { surfaceId: `s${i}`, catalogId: "c" } }]);
  const big = new w.FormBook();
  big.note([{ updateComponents: { surfaceId: "b", components: Array.from({ length: 3000 }, (_, i) => ({ id: `c${i}`, component: "Text", text: "x" })) } }]);

  assert.equal(book.title("booking"), "Book a table");
  assert.deepEqual(w.answerOf(action, book.components("booking")), {
    pressed: "Book",
    rows: [["Name for the booking", "Ada"], ["Guests", "4 guests"], ["Date and time", "2026-11-09 19:30"]],
    empty: false,
  });
  // A form the thread never saw: the action's name and the context's keys in its order.
  assert.deepEqual(w.answerOf(action, null), { pressed: "book", rows: [["date", "2026-11-09T19:30"], ["guests", "4"], ["name", "Ada"]], empty: false });
  // A switch reads Yes or No; a label that is a binding, a literal and an unbound path fall back.
  assert.deepEqual(w.answerOf({ name: "send", sourceComponentId: "go", context: { ok: true, n: 3, lit: "x", missing: null } }, extra.components("x")), {
    pressed: "send", rows: [["Agree", "Yes"], ["Count", "3"], ["lit", "x"], ["missing", "—"]], empty: false,
  });
  assert.equal(w.answerOf({ name: "send", context: { a: { b: 1 } } }, null).rows, null);
  assert.equal(w.answerOf({ name: "send", context: Object.fromEntries(Array.from({ length: 41 }, (_, i) => [`k${i}`, i])) }, null).rows, null);
  assert.deepEqual(w.answerOf({ name: "cancel", sourceComponentId: "nothing", context: {} }, book.components("booking")), { pressed: "cancel", rows: [], empty: true });
  assert.deepEqual(w.answerOf(null, null), { pressed: "an action", rows: [], empty: true });
  assert.equal(renewed.components("booking").size, 0);
  assert.deepEqual([many.forms.size, many.gens.size, many.components("s0"), many.components("s149") !== null, big.components("b").size], [100, 100, null, true, 2000]);
  assert.deepEqual([
    first > 0, reused.generation("booking") > first, reused.title("booking", first), reused.title("booking"),
    w.answerOf(action, reused.components("booking", first)).rows.length, reused.takesInput("booking", first),
    reused.takesInput("booking"), reused.takesInput("nothing"), reused.generation("nothing"),
  ], [true, true, "Book a table", "Your receipt", 3, true, false, null, 0]);
  assert.deepEqual(["2026-11-09T19:30", "2026-11-09T19:30:00Z", "2026-11-09T19:30:00+01:00", "tomorrow"].map((d) => w.answerOf({ name: "x", sourceComponentId: "book", context: { date: d } }, book.components("booking")).rows[0][1]),
    ["2026-11-09 19:30", "2026-11-09 19:30 UTC", "2026-11-09 19:30 +01:00", "tomorrow"]);
});
