// An agent's forms as data: the A2A samples' form part and a Structured Input request
// (js/agentschema.js, answered as the A2A Inspector answers them), and AgentStack's forms
// (js/agentstack.js: the card's initial form and a form an agent asks with).

import assert from "node:assert/strict";
import test from "node:test";
import { load } from "./helpers.mjs";

const FORM = {
  type: "object",
  properties: {
    full_page: { type: "boolean", title: "Full page" },
    keywords: { type: "string", examples: ["docs, faq"] },
    max_depth: { type: "integer", minimum: 0, maximum: 5 },
    mode: { type: "string", description: "quick | standard | deep" },
    note: { type: ["string", "null"], description: "Anything else, in a sentence." },
    output: { oneOf: [{ const: "json", title: "JSON" }, { const: "md" }] },
    ratio: { type: "number", exclusiveMaximum: 1 },
    since: { type: "string", format: "date" },
    url: { type: "string", format: "uri", title: "Page URL" },
    who: { type: "string", format: "email", readOnly: true },
  },
  required: ["url", "mode"],
};

test("an agent's form is read and answered as the Inspector answers it", async () => {
  const { collect, formSpec, refusal, suggestionsOf } = await load("agentschema.js");
  const { partsOf, STRUCTURED_INPUT } = await load("taskevents.js");
  const { formReply } = await load("a2uiwire.js");
  const schema = (id) => ({ data: { type: "object", properties: { a: { type: "string" } } }, metadata: { [STRUCTURED_INPUT]: id === undefined ? { type: "input_schema" } : { type: "input_schema", id } } });
  const samples = { type: "form", form: FORM, form_data: { url: "https://example.org", extra: 7 }, instructions: "Fill it in." };
  const parts = partsOf([
    { text: "Review the parameters." },
    { data: samples },
    schema("req-1"),
    { data: { valid: false, errors: [{ instanceLocation: "/a", error: "too short" }] }, metadata: { [STRUCTURED_INPUT]: { type: "validation_error", id: "req-1" } } },
    { data: { type: "form", form: { properties: "nope" } } },
    { data: { type: "form", form: "not a form" } },
    { data: { pages: 3, ok: true } },
    schema(undefined),
    schema("réq ✓"),
    schema("bad\nid"),
    schema(7),
  ]);
  const found = parts[1].form;
  const spec = formSpec(found.schema, { initial: found.initial });
  // As the page holds them: the agent's URL in its field, unless the person changed it.
  const fill = (v) => collect(spec, { url: "https://example.org", ...v }, { extra: found.initial });
  const many = { type: "object", properties: Object.fromEntries([...Array(61)].map((_, i) => ["f" + i, { type: "string" }])) };

  assert.deepEqual(parts.map((p) => Object.keys(p).sort().join(",")), [
    "text", "data,form", "data,form", "data,invalid", "data,form", "data,rows", "data,rows", "data,form", "data,form", "data,form", "data,form",
  ]);
  assert.equal(parts[2].form.inputId, "req-1");
  // The extension makes the id optional; any text without a control character goes back.
  assert.deepEqual(parts.slice(7).map((p) => [p.form.inputId, p.form.badId]), [[null, false], ["réq ✓", false], [null, true], [null, true]]);
  assert.deepEqual(parts[3].invalid, [{ at: "/a", error: "too short" }]);
  const refused = [refusal(parts[4].form.schema), refusal(many), refusal(found.schema)];
  assert.equal(refused[0], "its fields (properties) are not a JSON object");
  assert.match(refused[1], /61 fields/);
  assert.equal(refused[2], null);
  assert.deepEqual(parts[6].rows, [["pages", "3"], ["ok", "true"]]);
  // Required first (the gate sorts a data part's keys), then the rest as they came.
  assert.deepEqual(spec.fields.map((f) => [f.name, f.kind, f.required, f.initial]), [
    ["url", "url", true, "https://example.org"],
    ["mode", "text", true, ""],
    ["full_page", "boolean", false, ""],
    ["keywords", "text", false, ""],
    ["max_depth", "integer", false, ""],
    ["note", "text", false, ""],
    ["output", "enum", false, ""],
    ["ratio", "number", false, ""],
    ["since", "date", false, ""],
    ["who", "email", false, ""],
  ]);
  const field = (n) => spec.fields.find((f) => f.name === n);
  assert.deepEqual(field("mode").suggest, ["quick", "standard", "deep"]);
  assert.equal(field("keywords").placeholder, "docs, faq");
  assert.deepEqual(field("output").labels, ["JSON", "md"]);
  assert.equal(field("who").readOnly, true);
  assert.equal(suggestionsOf("Pick quick or deep, as you like."), null);
  // Accepting the agent's values is an answer: switches false, text "", numbers left out, and
  // form_data's keys that are no field passed back.
  assert.deepEqual(fill({ url: "https://example.org", mode: "quick" }), { data: {
    extra: 7, url: "https://example.org", mode: "quick", full_page: false, keywords: "", note: "", since: "", who: "",
  } });
  assert.deepEqual(fill({}), { error: "mode is required." });
  assert.deepEqual(fill({ url: "", mode: "quick" }), { error: "Page URL is required." });
  assert.deepEqual(fill({ url: "example.org", mode: "quick" }), { error: "Page URL: a full address, with its scheme (https://…)." });
  assert.deepEqual(fill({ mode: "quick", who: "nobody" }), { error: "who: an email address." });
  assert.deepEqual(fill({ mode: "quick", max_depth: "9" }), { error: "max_depth: at most 5." });
  assert.deepEqual(fill({ mode: "quick", ratio: "1" }), { error: "ratio: less than 1." });
  assert.deepEqual(fill({ mode: "quick", since: "tomorrow" }), { error: "since: a date (YYYY-MM-DD)." });
  const full = fill({ url: "https://example.org", mode: "deep", full_page: true, max_depth: "2", output: "0", ratio: "0.5", since: "2026-11-03", keywords: "docs" });
  assert.deepEqual([full.data.max_depth, full.data.ratio, full.data.output, full.data.full_page], [2, 0.5, "json", true]);
  // Structured Input's agent validates: an optional text left empty is left out.
  assert.deepEqual(collect(spec, { url: "https://example.org", mode: "quick" }, { blanks: false }), { data: { url: "https://example.org", mode: "quick", full_page: false } });
  // Every Structured Input answer is tagged, with the request's id when it had one.
  const reply = formReply({ a: 1 }, parts[2].form);
  assert.deepEqual(reply.data, { a: 1 });
  assert.deepEqual(reply.structured_input, { id: "req-1" });
  assert.deepEqual(formReply({ a: 1 }, parts[7].form).structured_input, {});
  assert.ok(!("structured_input" in formReply({ a: 1 }, found)));
  assert.ok(!("structured_input" in formReply({ a: 1 })));
});

test("AgentStack forms are read, filled and answered", async () => {
  const a = await load("agentstack.js");
  const form = {
    title: "Plan", description: "How.", columns: 2, submit_label: "Start", extra: 1,
    fields: [
      { id: "site", type: "text", label: "Site", required: true, col_span: 2, placeholder: "https://…", auto_resize: false },
      { id: "since", type: "date", label: "Since" },
      { id: "depth", type: "singleselect", label: "Depth", required: true, options: [{ id: "a", label: "A" }, { id: "b", label: "B" }, { id: "a", label: "again" }], default_value: "b" },
      { id: "kinds", type: "multiselect", label: "Kinds", options: [{ id: "x", label: "X" }, { id: "y", label: "Y" }], default_value: ["y", "z"] },
      { id: "ok", type: "checkbox", label: "OK", content: "I agree", required: true },
      { id: "more", type: "checkbox_group", label: "More", fields: [{ id: "m1", type: "checkbox", label: "M1", content: "one", default_value: true }, { id: "m2", type: "checkbox", label: "M2" }] },
      { id: "seed", type: "file", label: "Seed", accept: ["text/plain"] },
      { id: "stars", type: "rating", label: "Stars" },
    ],
  };
  const { form: f, note } = a.normalizeForm(form);
  assert.equal(note, "1 field of a type the console does not draw left out");
  assert.deepEqual([f.title, f.description, f.columns, f.submit_label], ["Plan", "How.", 2, "Start"]);
  assert.ok(!("extra" in f));
  assert.deepEqual(f.fields.map((x) => x.id), ["site", "since", "depth", "kinds", "ok", "more", "seed"]);
  const [site, since, depth, kinds, ok, more, seed] = f.fields;
  assert.deepEqual(site, { id: "site", label: "Site", type: "text", required: true, col_span: 2, placeholder: "https://…", default_value: null, auto_resize: false, options: null, content: null, accept: null, fields: null });
  assert.deepEqual([since.required, since.col_span], [false, null]);
  assert.deepEqual(depth.options, [{ id: "a", label: "A" }, { id: "b", label: "B" }]);
  assert.equal(depth.default_value, "b");
  assert.deepEqual(kinds.default_value, ["y"]); // an id no option has is dropped
  assert.deepEqual([ok.content, ok.default_value], ["I agree", false]);
  assert.deepEqual(more.fields.map((c) => c.id), ["m1", "m2"]);
  assert.equal(more.fields[0].default_value, true);
  assert.deepEqual(seed.accept, ["text/plain"]);
  const many = { fields: Array.from({ length: 65 }, (_, i) => ({ id: "f" + i, type: "text", label: "F" })) };
  const big = { fields: [{ id: "t", type: "text", label: "T", default_value: "x".repeat(70000) }] };
  assert.deepEqual([
    a.normalizeForm(null).note,
    a.normalizeForm({ fields: [{ id: "bad id!", type: "text", label: "x" }] }).note,
    a.normalizeForm({ fields: [{ id: "a", type: "text", label: "x" }, { id: "a", type: "date", label: "y" }] }).note,
    a.normalizeForm(many).note,
    a.normalizeForm(big).note,
    a.normalizeForm({ fields: [{ id: "a", type: "rating", label: "x" }] }).note,
    a.normalizeForm({ fields: [{ id: "s", type: "singleselect", label: "S", options: [] }] }).note,
  ], [
    "it is not a form", "a field's id is not one the console can send back", "two fields are named a",
    "it has more than 64 fields", "it is larger than 64 KiB", "none of its fields is of a type the console draws", "S has no options",
  ]);
  assert.deepEqual(a.initialState(f), { site: "", since: "", depth: "b", kinds: ["y"], ok: false, more: { m1: true, m2: false }, seed: [] });
  // A fulfilment fills the form; a value of another type is not taken.
  const given = a.initialState(f, { site: { type: "text", value: "https://x.example" }, depth: { type: "date", value: "a" }, more: { type: "checkbox_group", value: { m1: false, m2: true } } });
  assert.deepEqual([given.site, given.depth, given.more], ["https://x.example", "b", { m1: false, m2: true }]);
  assert.deepEqual(a.collectValues(f, a.initialState(f)), { error: "Site is required.", field: "site" });
  const filled = a.initialState(f);
  Object.assign(filled, { site: "https://example.com", since: "2026-10-01", kinds: ["x", "y"], ok: true, seed: [{ uri: "data:text/plain;base64,YQ==", name: "a.txt", mime_type: "text/plain", size: 1 }] });
  filled.more.m2 = true;
  const errors = [
    a.collectValues(f, { ...filled, site: "  " }),
    a.collectValues(f, { ...filled, since: "1 Oct" }),
    a.collectValues(f, { ...filled, depth: "" }),
    a.collectValues(f, { ...filled, ok: false }),
    a.collectValues(f, { ...filled, seed: [{ uri: "data:," + "x".repeat(300 * 1024), name: "big" }] }),
  ];
  assert.deepEqual(errors.map((e) => e.field), ["site", "since", "depth", "ok", "seed"]);
  assert.equal(errors[1].error, "Since: a date (YYYY-MM-DD).");
  assert.equal(errors[4].error, "The form is larger than 60 KiB in all: send smaller files.");
  const got = a.collectValues(f, filled);
  assert.deepEqual(got, { values: {
    site: { type: "text", value: "https://example.com" },
    since: { type: "date", value: "2026-10-01" },
    depth: { type: "singleselect", value: "b" },
    kinds: { type: "multiselect", value: ["x", "y"] },
    ok: { type: "checkbox", value: true },
    more: { type: "checkbox_group", value: { m1: true, m2: true } },
    seed: { type: "file", value: [{ uri: "data:text/plain;base64,YQ==", name: "a.txt", mime_type: "text/plain" }] },
  } });
  const rows = a.valueRows(f, got.values);
  assert.deepEqual(rows, [["Site", "https://example.com"], ["Since", "2026-10-01"], ["Depth", "B"], ["Kinds", "X, Y"], ["OK", "yes"], ["More", "M1, M2"], ["Seed", "a.txt"]]);
  // In the form's order whatever order the values come in, ids it does not name after.
  const reordered = Object.fromEntries(Object.entries(got.values).sort(([x], [y]) => x.length - y.length || (x < y ? -1 : 1)).concat([["zz", { type: "text", value: "?" }]]));
  assert.deepEqual(a.valueRows(f, reordered), [...rows, ["zz", "?"]]);
  assert.deepEqual(a.valueRows(null, { depth: { type: "singleselect", value: "b" } }), [["depth", "b"]]);
  assert.deepEqual([
    a.fulfilledValues({ [a.FORM_SERVICE]: { form_fulfillments: { initial_form: { values: { x: 1 } } } } }),
    a.fulfilledValues({ other: 1 }),
    a.answeredValues({ [a.FORM_REQUEST]: { values: { y: 2 } } }),
    a.requestedForm({ [a.FORM_REQUEST]: { title: "T", fields: [] } }),
    a.requestedForm({ [a.FORM_REQUEST]: { values: {} } }),
  ], [{ x: 1 }, null, { y: 2 }, { title: "T", fields: [] }, null]);
});
