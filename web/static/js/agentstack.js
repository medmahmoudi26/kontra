// AgentStack's forms (agentstack-sdk 0.7.1, a2a/extensions/common/form.py), as data: pure
// functions, no DOM and no request (js/agentform.js draws them).
//
// * **The initial form**: a card declares `services/form/v1` with
//   `params.form_demands.initial_form` (a FormRender); the console API normalises it into the
//   start form's `initial_form` and the console fills it before the first message, whose metadata
//   carries the person's values (`form_fulfillments.initial_form.values`, the start's `form`).
// * **A form the agent asks for**: an `input-required` status whose message's metadata
//   holds `ui/form_request/v1`: a FormRender; the answer is the reply's `form_response`.
//
// A FormRender is {title?, description?, columns? (1-4), submit_label?, fields}; a field is {id,
// label, required, col_span? (1-4), type, ...}: "text" {placeholder, default_value, auto_resize},
// "date" {placeholder, default_value}, "file" {accept}, "singleselect" {options, default_value},
// "multiselect" {options, default_value: [ids]}, "checkbox" {content, default_value},
// "checkbox_group" {fields: [checkbox fields]}. A value is {type, value}: a string (text, date, a
// single choice's id), a boolean (checkbox), ids (multiselect), {id: boolean} (checkbox_group) or
// files [{uri, name?, mime_type?}]; null when empty.

export const FORM_SERVICE = "https://a2a-extensions.agentstack.beeai.dev/services/form/v1";
export const FORM_REQUEST = "https://a2a-extensions.agentstack.beeai.dev/ui/form_request/v1";
/** A form the console draws: at most 64 KiB of JSON and 64 fields (a group's included). */
export const FORM_BYTES_MAX = 64 * 1024;
export const FORM_FIELDS_MAX = 64;
/** A field's (and an option's) id. */
export const ID_RE = /^[A-Za-z0-9_.:-]{1,128}$/;
/** One file, read in the page as a data: URI, and every value of a form together: what a start's
 * and a reply's 64 KiB request body holds, a file's base64 a third larger. */
export const FILE_MAX = 44 * 1024;
export const VALUES_MAX = 60 * 1024;
export const TYPES = Object.freeze(["text", "date", "file", "singleselect", "multiselect", "checkbox", "checkbox_group"]);

const obj = (v) => v !== null && typeof v === "object" && !Array.isArray(v);
const str = (v, max) => (typeof v === "string" && v.trim() ? v.slice(0, max) : null);
const span = (v) => (Number.isInteger(v) && v >= 1 && v <= 4 ? v : null);
// eslint-disable-next-line no-control-regex
const OPTION_ID = /^[^\x00-\x1f\x7f]{1,128}$/;

function byteLength(text) {
  return new TextEncoder().encode(text).length;
}

function options(list) {
  if (!Array.isArray(list)) return null;
  const out = [];
  const seen = new Set();
  for (const o of list.slice(0, 200)) {
    if (!obj(o) || typeof o.id !== "string" || !OPTION_ID.test(o.id) || seen.has(o.id)) continue;
    seen.add(o.id);
    out.push({ id: o.id, label: str(o.label, 200) || o.id });
  }
  return out.length ? out : null;
}

/** One field as the console API normalises it (absent keys null), or a string saying why it is
 * dropped, or null for a field whose type the console does not know (dropped with a note). */
function field(f, nested = false) {
  if (!obj(f)) return "a field is not an object";
  if (typeof f.id !== "string" || !ID_RE.test(f.id)) return "a field's id is not one the console can send back";
  if (typeof f.type !== "string" || !TYPES.includes(f.type) || (nested && f.type !== "checkbox")) return null;
  const out = {
    id: f.id,
    label: str(f.label, 200) || f.id,
    type: f.type,
    required: f.required === true,
    col_span: span(f.col_span),
    placeholder: null,
    default_value: null,
    auto_resize: null,
    options: null,
    content: null,
    accept: null,
    fields: null,
  };
  if (f.type === "text" || f.type === "date") {
    out.placeholder = str(f.placeholder, 200);
    out.default_value = typeof f.default_value === "string" ? f.default_value.slice(0, 65536) : null;
    if (f.type === "text") out.auto_resize = f.auto_resize === false ? false : f.auto_resize === true ? true : null;
  } else if (f.type === "singleselect" || f.type === "multiselect") {
    out.options = options(f.options);
    if (!out.options) return `${out.label} has no options`;
    const ids = new Set(out.options.map((o) => o.id));
    if (f.type === "singleselect") out.default_value = typeof f.default_value === "string" && ids.has(f.default_value) ? f.default_value : null;
    else out.default_value = Array.isArray(f.default_value) ? f.default_value.filter((x) => typeof x === "string" && ids.has(x)) : null;
  } else if (f.type === "checkbox") {
    out.content = str(f.content, 500) || out.label;
    out.default_value = f.default_value === true;
  } else if (f.type === "file") {
    out.accept = Array.isArray(f.accept) ? f.accept.filter((x) => typeof x === "string" && x.length <= 100).slice(0, 20) : [];
  } else if (f.type === "checkbox_group") {
    const inner = [];
    for (const c of Array.isArray(f.fields) ? f.fields : []) {
      const got = field(c, true);
      if (typeof got === "string") return got;
      if (got) inner.push(got);
    }
    if (!inner.length) return `${out.label} has no checkbox`;
    out.fields = inner;
  }
  return out;
}

/** A FormRender as the console draws it: { form, note } -- `form` normalised as the console API
 * normalises a card's initial form (the start form's `initial_form`), or null with `note` saying why the
 * console cannot draw it; `note` also says when fields of a type it does not know were dropped. */
export function normalizeForm(raw) {
  if (!obj(raw) || !Array.isArray(raw.fields)) return { form: null, note: "it is not a form" };
  let size;
  try {
    size = byteLength(JSON.stringify(raw));
  } catch {
    return { form: null, note: "it is not a form" };
  }
  if (size > FORM_BYTES_MAX) return { form: null, note: "it is larger than 64 KiB" };
  const count = raw.fields.reduce((n, f) => n + 1 + (obj(f) && Array.isArray(f.fields) ? f.fields.length : 0), 0);
  if (count > FORM_FIELDS_MAX) return { form: null, note: `it has more than ${FORM_FIELDS_MAX} fields` };
  const fields = [];
  const ids = new Set();
  let dropped = 0;
  for (const f of raw.fields) {
    const got = field(f);
    if (typeof got === "string") return { form: null, note: got };
    if (got === null) {
      dropped += 1;
      continue;
    }
    for (const id of [got.id, ...(got.fields || []).map((c) => c.id)]) {
      if (ids.has(id)) return { form: null, note: `two fields are named ${id}` };
      ids.add(id);
    }
    fields.push(got);
  }
  if (!fields.length) return { form: null, note: dropped ? "none of its fields is of a type the console draws" : "it has no field" };
  const form = {
    title: str(raw.title, 200),
    description: str(raw.description, 2000),
    columns: span(raw.columns),
    submit_label: str(raw.submit_label, 40),
    fields,
  };
  return { form, note: dropped ? `${dropped} field${dropped === 1 ? "" : "s"} of a type the console does not draw left out` : null };
}

/** The form's state as drawn: each field's default (or `given`, a fulfilment's values, as Run
 * again fills it). text/date: a string; singleselect: an id or ""; multiselect: ids; checkbox: a
 * boolean; checkbox_group: {id: boolean}; file: [{uri, name, mime_type, size}]. */
export function initialState(form, given = null) {
  const g = obj(given) ? given : {};
  const state = {};
  for (const f of form.fields) {
    const v = obj(g[f.id]) && g[f.id].type === f.type ? g[f.id].value : undefined;
    if (f.type === "text" || f.type === "date") state[f.id] = typeof v === "string" ? v : f.default_value || "";
    else if (f.type === "singleselect") state[f.id] = typeof v === "string" && f.options.some((o) => o.id === v) ? v : f.default_value || "";
    else if (f.type === "multiselect") state[f.id] = Array.isArray(v) ? v.filter((x) => f.options.some((o) => o.id === x)) : [...(f.default_value || [])];
    else if (f.type === "checkbox") state[f.id] = typeof v === "boolean" ? v : f.default_value === true;
    else if (f.type === "checkbox_group") {
      state[f.id] = Object.fromEntries(f.fields.map((c) => [c.id, obj(v) && typeof v[c.id] === "boolean" ? v[c.id] : c.default_value === true]));
    } else if (f.type === "file") {
      state[f.id] = Array.isArray(v) ? v.filter((x) => obj(x) && typeof x.uri === "string").map((x) => ({ uri: x.uri, name: x.name || null, mime_type: x.mime_type || null, size: null })) : [];
    }
  }
  return state;
}

const DATE = /^\d{4}-\d{2}-\d{2}$/;

/** The values the form's state holds, as AgentStack sends them ({id: {type, value}}, every field),
 * or { error, field } naming the first field that is wrong. */
export function collectValues(form, state) {
  const values = {};
  for (const f of form.fields) {
    const v = state[f.id];
    let value = null;
    if (f.type === "text" || f.type === "date") {
      const s = typeof v === "string" ? v : "";
      if (f.required && !s.trim()) return { error: `${f.label} is required.`, field: f.id };
      if (f.type === "date" && s && !DATE.test(s)) return { error: `${f.label}: a date (YYYY-MM-DD).`, field: f.id };
      value = s.trim() ? s : null;
    } else if (f.type === "singleselect") {
      value = typeof v === "string" && f.options.some((o) => o.id === v) ? v : null;
      if (f.required && value === null) return { error: `${f.label}: pick one.`, field: f.id };
    } else if (f.type === "multiselect") {
      const ids = Array.isArray(v) ? f.options.map((o) => o.id).filter((id) => v.includes(id)) : [];
      if (f.required && !ids.length) return { error: `${f.label}: pick at least one.`, field: f.id };
      value = ids.length ? ids : null;
    } else if (f.type === "checkbox") {
      value = v === true;
      if (f.required && !value) return { error: `${f.label} is required.`, field: f.id };
    } else if (f.type === "checkbox_group") {
      value = Object.fromEntries(f.fields.map((c) => [c.id, Boolean(obj(v) && v[c.id] === true)]));
      for (const c of f.fields) if (c.required && !value[c.id]) return { error: `${c.label} is required.`, field: f.id };
      if (f.required && !Object.values(value).some(Boolean)) return { error: `${f.label}: pick at least one.`, field: f.id };
    } else if (f.type === "file") {
      const files = Array.isArray(v) ? v : [];
      if (f.required && !files.length) return { error: `${f.label}: add a file.`, field: f.id };
      value = files.length ? files.map((x) => ({ uri: x.uri, ...(x.name ? { name: x.name } : {}), ...(x.mime_type ? { mime_type: x.mime_type } : {}) })) : null;
    }
    values[f.id] = { type: f.type, value };
  }
  if (byteLength(JSON.stringify({ values })) > VALUES_MAX) {
    const big = form.fields.find((f) => f.type === "file");
    return { error: "The form is larger than 60 KiB in all: send smaller files.", field: big ? big.id : form.fields[0].id };
  }
  return { values };
}

/** The values in words: [label, text] for each field that holds something, labelled as `form`
 * labels them (by id when the form is not known), in the form's order (the gate's log keeps a
 * message's metadata as jsonb, whose keys come back shortest first), then the ids it does not name. */
export function valueRows(form, values) {
  const rows = [];
  const fields = form ? form.fields : [];
  const given = obj(values) ? values : {};
  const ids = [...fields.map((f) => f.id).filter((id) => Object.hasOwn(given, id)), ...Object.keys(given).filter((id) => !fields.some((f) => f.id === id))];
  for (const id of ids) {
    const entry = given[id];
    if (!obj(entry)) continue;
    const f = fields.find((x) => x.id === id) || null;
    const label = f ? f.label : id;
    const v = entry.value;
    let text = null;
    if (typeof v === "string") {
      const o = f && f.options ? f.options.find((x) => x.id === v) : null;
      text = o ? o.label : v;
    } else if (typeof v === "boolean") {
      text = v ? "yes" : "no";
    } else if (Array.isArray(v) && v.every((x) => typeof x === "string")) {
      text = v.map((x) => (f && f.options ? (f.options.find((o) => o.id === x) || { label: x }).label : x)).join(", ");
    } else if (Array.isArray(v)) {
      text = v.filter(obj).map((x) => x.name || "file").join(", ");
    } else if (obj(v)) {
      const on = Object.entries(v).filter(([, b]) => b === true).map(([k]) => (f && f.fields ? (f.fields.find((c) => c.id === k) || { label: k }).label : k));
      text = on.length ? on.join(", ") : "none";
    }
    if (text !== null && text !== "") rows.push([label, text]);
  }
  return rows;
}

/** The initial form's values a first message's metadata carries (the console's or another AgentStack
 * client's fulfilment), or null. */
export function fulfilledValues(metadata) {
  const m = obj(metadata) && obj(metadata[FORM_SERVICE]) ? metadata[FORM_SERVICE] : null;
  const f = m && obj(m.form_fulfillments) && obj(m.form_fulfillments.initial_form) ? m.form_fulfillments.initial_form : null;
  return f && obj(f.values) ? f.values : null;
}

/** The answer to a form request a reply's metadata carries, or null. */
export function answeredValues(metadata) {
  const m = obj(metadata) && obj(metadata[FORM_REQUEST]) ? metadata[FORM_REQUEST] : null;
  return m && obj(m.values) ? m.values : null;
}

/** The FormRender a status message's metadata asks with, as it came, or null. */
export function requestedForm(metadata) {
  return obj(metadata) && obj(metadata[FORM_REQUEST]) && Array.isArray(metadata[FORM_REQUEST].fields) ? metadata[FORM_REQUEST] : null;
}
