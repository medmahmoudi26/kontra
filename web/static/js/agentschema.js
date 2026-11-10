// An agent's form on the thread, from its JSON Schema (pure functions: no DOM, no request): the
// A2A samples' form part (`{type: "form", form, form_data, instructions}`, which the A2A Inspector
// draws) and the Structured Input extension's `input_schema` part are drawn from their schema --
// the required fields first (the gate re-marshals a data part's keys sorted), the agent's
// `form_data` as initial values, plain strings on one line -- and answered with an answer that
// always sends, its values typed as the A2A Inspector types them.
//
// The console checks what it draws (numbers, bounds, lengths, an absolute URL, an email's @, a
// date); the agent checks its own form's answer. `pattern`, `$ref` and the other formats are never
// checked here: an agent's regular expression never runs in the page.

const LONG_TEXT = 200;
const HINT_MAX = 300;
const LABEL_MAX = 80;
/** An agent's form is drawn up to this many fields; more is shown as it came. */
export const FIELDS_MAX = 60;
/** A description that is only a list of choices: "a | b | c", 2 to 12 short words. */
const CHOICES = /^\s*([A-Za-z0-9_.-]{1,40})(?:\s*\|\s*([A-Za-z0-9_.-]{1,40})){1,11}\s*$/;
const URL_FORMATS = new Set(["uri", "url", "iri"]);

const obj = (v) => v !== null && typeof v === "object" && !Array.isArray(v);
const scalar = (x) => typeof x === "string" || typeof x === "number" || typeof x === "boolean";

/** The type a property names: its `type`, or the one type besides "null" a type array holds. */
function typeOf(prop) {
  const t = prop.type;
  if (Array.isArray(t)) {
    const rest = t.filter((x) => x !== "null");
    return rest.length === 1 ? rest[0] : undefined;
  }
  return t;
}

/** A choice's options: `enum` (scalars), or `oneOf`/`anyOf` of `{const}`, each `title` its label. */
function choicesOf(prop) {
  if (Array.isArray(prop.enum) && prop.enum.length && prop.enum.every(scalar)) {
    return { values: prop.enum.slice(), labels: prop.enum.map(String) };
  }
  for (const key of ["oneOf", "anyOf"]) {
    const list = prop[key];
    if (Array.isArray(list) && list.length && list.every((x) => obj(x) && scalar(x.const))) {
      return { values: list.map((x) => x.const), labels: list.map((x) => (typeof x.title === "string" && x.title ? x.title.slice(0, LABEL_MAX) : String(x.const))) };
    }
  }
  return null;
}

function kindOf(prop) {
  if (!obj(prop)) return "json";
  if (choicesOf(prop)) return "enum";
  switch (typeOf(prop)) {
    case "boolean": return "boolean";
    case "integer": return "integer";
    case "number": return "number";
    case "string": {
      const f = typeof prop.format === "string" ? prop.format.toLowerCase() : "";
      if (URL_FORMATS.has(f)) return "url";
      if (f === "email") return "email";
      if (f === "date") return "date";
      if (typeof prop.maxLength === "number") return prop.maxLength <= LONG_TEXT ? "text" : "long";
      return "text"; // an agent's form field is a line, as the samples draw it
    }
    default: return "json";
  }
}

const short = (s) => (typeof s === "string" && s ? s.slice(0, HINT_MAX) : null);
const num = (v) => (typeof v === "number" && Number.isFinite(v) ? v : null);

/** The words of a description that is nothing but a list of choices ("quick | standard | deep"). */
export function suggestionsOf(description) {
  if (typeof description !== "string" || !CHOICES.test(description)) return null;
  return description.split("|").map((w) => w.trim());
}

function initialOf(kind, value, choices) {
  if (value === undefined || value === null) return "";
  if (kind === "json") return JSON.stringify(value, null, 2);
  if (kind === "boolean") return value === true || value === "true";
  if (kind === "enum") {
    const i = choices.values.findIndex((v) => v === value || String(v) === String(value));
    return i < 0 ? "" : String(i);
  }
  return typeof value === "object" ? JSON.stringify(value) : String(value);
}

/** Why an agent's form cannot be drawn (its properties are not an object, or too many), or null. */
export function refusal(schema) {
  if (!obj(schema)) return "it is not a JSON object";
  if (!obj(schema.properties)) return "its fields (properties) are not a JSON object";
  const n = Object.keys(schema.properties).length;
  if (!n) return "it has no field";
  if (n > FIELDS_MAX) return `it has ${n} fields, more than the ${FIELDS_MAX} the console draws`;
  return null;
}

/** The form a schema asks for: `fields` (one per property of an object schema, the required ones
 * first: { name, label, kind, required, hint, options, labels, initial, max, minimum, maximum,
 * exclusiveMinimum, exclusiveMaximum, minLength, maxLength, placeholder, suggest, readOnly }), or
 * `whole` (one JSON box for the whole answer) when the schema is not an object of properties.
 * `initial`: values by property name (the form part's `form_data`), before a property's `default`. */
export function formSpec(schema, { initial = null } = {}) {
  if (!obj(schema)) return { whole: true, fields: [] };
  const props = obj(schema.properties) ? schema.properties : null;
  const isObject = schema.type === undefined || schema.type === "object" || (Array.isArray(schema.type) && schema.type.includes("object"));
  if (!isObject || !props || !Object.keys(props).length) return { whole: true, fields: [], hint: short(schema.description) };
  const requiredList = Array.isArray(schema.required) ? schema.required.filter((x) => typeof x === "string") : [];
  const required = new Set(requiredList);
  const given = obj(initial) ? initial : {};
  const names = [...requiredList.filter((n) => Object.hasOwn(props, n)), ...Object.keys(props).filter((n) => !required.has(n))];
  const fields = [...new Set(names)].map((name) => {
    const prop = props[name];
    const kind = kindOf(prop);
    const p = obj(prop) ? prop : {};
    const choices = kind === "enum" ? choicesOf(p) : null;
    const value = Object.hasOwn(given, name) ? given[name] : p.default;
    const example = Array.isArray(p.examples) && p.examples.length && scalar(p.examples[0]) ? String(p.examples[0]).slice(0, 120) : null;
    return {
      name,
      label: typeof p.title === "string" && p.title ? p.title.slice(0, LABEL_MAX) : name,
      kind,
      required: required.has(name),
      hint: short(p.description),
      options: choices ? choices.values : null,
      labels: choices ? choices.labels : null,
      initial: initialOf(kind, value, choices),
      max: typeof p.maxLength === "number" ? p.maxLength : null,
      minimum: num(p.minimum),
      maximum: num(p.maximum),
      exclusiveMinimum: num(p.exclusiveMinimum),
      exclusiveMaximum: num(p.exclusiveMaximum),
      minLength: typeof p.minLength === "number" ? p.minLength : null,
      maxLength: typeof p.maxLength === "number" ? p.maxLength : null,
      placeholder: example,
      suggest: kind === "text" ? suggestionsOf(p.description) : null,
      readOnly: p.readOnly === true,
    };
  });
  return { whole: false, fields, hint: short(schema.description), required: [...required] };
}

const isUrl = (s) => {
  try {
    const u = new URL(s);
    return /^[a-z][a-z0-9+.-]*:$/i.test(u.protocol) && (u.host !== "" || u.protocol === "mailto:" || u.protocol === "urn:");
  } catch {
    return false;
  }
};

const set = (x) => x !== null && x !== undefined;

function checkNumber(f, n) {
  if (set(f.minimum) && n < f.minimum) return `${f.label}: at least ${f.minimum}.`;
  if (set(f.maximum) && n > f.maximum) return `${f.label}: at most ${f.maximum}.`;
  if (set(f.exclusiveMinimum) && n <= f.exclusiveMinimum) return `${f.label}: more than ${f.exclusiveMinimum}.`;
  if (set(f.exclusiveMaximum) && n >= f.exclusiveMaximum) return `${f.label}: less than ${f.exclusiveMaximum}.`;
  return null;
}

function checkText(f, text) {
  if (set(f.minLength) && text.length < f.minLength) return `${f.label}: at least ${f.minLength} characters.`;
  if (set(f.maxLength) && text.length > f.maxLength) return `${f.label}: at most ${f.maxLength} characters.`;
  if (f.kind === "url" && !isUrl(text)) return `${f.label}: a full address, with its scheme (https://…).`;
  if (f.kind === "email" && !/^[^\s@]+@[^\s@]+$/.test(text)) return `${f.label}: an email address.`;
  if (f.kind === "date" && !/^\d{4}-\d{2}-\d{2}$/.test(text)) return `${f.label}: a date (YYYY-MM-DD).`;
  return null;
}

/** The answer's `data` from what the person filled in (`values`: name -> the field's string, or a
 * boolean for a yes/no, `undefined` for one never touched): { data } or { error } naming the field.
 *
 * Always sent (accepting the agent's values is an answer), typed as the A2A Inspector types them --
 * a switch true or false, numbers as numbers (left out when empty), a choice's value (left out when
 * none), a text field always ("", unless `blanks` is false: Structured Input's agent validates, so an
 * optional text left empty is left out), a JSON box parsed (left out when empty) -- and the keys of
 * `extra` (a form's `form_data`) that are not fields passed back unchanged. */
export function collect(spec, values, { extra = null, blanks = true } = {}) {
  if (spec.whole) {
    const raw = typeof values.$whole === "string" ? values.$whole.trim() : "";
    if (!raw) return { error: "Fill in the input (a JSON object)." };
    try {
      const v = JSON.parse(raw);
      if (!obj(v)) return { error: "The input must be a JSON object." };
      return { data: v };
    } catch {
      return { error: "The input is not valid JSON." };
    }
  }
  const data = {};
  if (obj(extra)) {
    const names = new Set(spec.fields.map((f) => f.name));
    for (const [k, v] of Object.entries(extra)) if (!names.has(k)) data[k] = v;
  }
  for (const f of spec.fields) {
    const v = values[f.name];
    if (f.kind === "boolean") {
      data[f.name] = v === undefined ? f.initial === true : Boolean(v);
      continue;
    }
    const text = typeof v === "string" ? v : "";
    const textual = f.kind === "text" || f.kind === "long" || f.kind === "url" || f.kind === "email" || f.kind === "date";
    if (!text.trim()) {
      if (textual) {
        if (f.required) return { error: `${f.label} is required.` };
        if (blanks) data[f.name] = "";
      }
      continue;
    }
    if (f.kind === "integer") {
      if (!/^-?\d+$/.test(text.trim())) return { error: `${f.label}: a whole number.` };
      const n = Number(text.trim());
      const bad = checkNumber(f, n);
      if (bad) return { error: bad };
      data[f.name] = n;
    } else if (f.kind === "number") {
      const n = Number(text.trim());
      if (!/^-?(?:\d+\.?\d*|\.\d+)(?:[eE][-+]?\d+)?$/.test(text.trim()) || !Number.isFinite(n)) return { error: `${f.label}: a number.` };
      const bad = checkNumber(f, n);
      if (bad) return { error: bad };
      data[f.name] = n;
    } else if (f.kind === "enum") {
      const i = Number(text);
      if (!Number.isInteger(i) || i < 0 || i >= f.options.length) return { error: `${f.label}: pick one.` };
      data[f.name] = f.options[i];
    } else if (f.kind === "json") {
      try {
        data[f.name] = JSON.parse(text);
      } catch {
        return { error: `${f.label}: not valid JSON.` };
      }
    } else {
      const value = f.kind === "url" || f.kind === "email" ? text.trim() : text;
      const bad = checkText(f, value);
      if (bad) return { error: bad };
      data[f.name] = value;
    }
  }
  for (const f of spec.fields) {
    if (f.required && !(f.name in data)) return { error: `${f.label} is required.` };
  }
  return { data };
}
