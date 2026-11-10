// A2UI on the thread, as data: which parts of an agent's message carry A2UI
// (https://a2ui.org, v0.9 and v0.9.1: declarative UI an agent sends as JSON, drawn by the console
// from its own catalog), the messages such a part holds, and what a person's click on one of its
// buttons sends back (an A2UI `action`, through the thread's reply). Pure functions: no DOM, no
// request, no renderer (js/a2ui.js draws, and is loaded only once a thread shows A2UI); the forms
// an agent sent, kept as data, read a person's answer in words (`FormBook`, `answerOf`).

/** A2UI's media type (v0.9.1 on), and the one v0.8 and v0.9 used. */
export const A2UI_MIME = "application/a2ui+json";
export const A2UI_MIME_LEGACY = "application/json+a2ui";
/** The catalog the console draws: the basic catalog's components and functions, with console.css
 * (js/a2ui.js). The agent names it in each surface it creates. */
export const CATALOG_ID = "https://a2ui.org/specification/v0_9/catalogs/basic/catalog.json";
/** What the console tells the console API it renders (a start's and a reply's `a2ui_catalogs`:
 * the console API makes them the message's `a2uiClientCapabilities`). */
export const CATALOGS = Object.freeze([CATALOG_ID]);
/** The protocol versions the renderer reads, and an action's `version` names. */
export const VERSIONS = Object.freeze(["v0.9", "v0.9.1"]);
/** At most this many messages in one part: a surface is a few dozen; more is not drawn. */
export const MESSAGES_MAX = 500;
/** An action's JSON is at most this long: the reply route reads 64 KiB at most. */
export const ACTION_MAX = 60 * 1024;

const obj = (v) => v !== null && typeof v === "object" && !Array.isArray(v);
const isA2uiType = (t) => {
  if (typeof t !== "string") return false;
  const bare = t.split(";")[0].trim().toLowerCase();
  return bare === A2UI_MIME || bare === A2UI_MIME_LEGACY;
};

/** Whether an A2A part carries A2UI: a data part whose type says so -- the part's metadata
 * `mimeType` (what A2UI's A2A extension specifies), its A2A 1.0 `mediaType`, or a `mimeType`
 * beside its data (the A2UI sample client's v0.3 shape). */
export function isA2uiPart(p) {
  if (!obj(p) || p.data === undefined) return false;
  return [obj(p.metadata) ? p.metadata.mimeType : undefined, p.mediaType, p.mimeType].some(isA2uiType);
}

/** The A2UI messages a part's data holds: an array of them (the specification's), one message
 * (the reference agents' one message per part), A2UI's list wrapper `{"messages": [...]}`, or an
 * array an A2A v0.3 hop wrapped as `{"value": [...]}`; null for anything else (the thread shows
 * it as JSON, with a note). */
export function a2uiMessages(data) {
  let list = data;
  if (obj(data) && Object.keys(data).length === 1) {
    if (Array.isArray(data.messages)) list = data.messages;
    else if (Array.isArray(data.value)) list = data.value;
  }
  if (obj(list)) list = [list];
  if (!Array.isArray(list) || !list.length || list.length > MESSAGES_MAX || !list.every(obj)) return null;
  return list;
}

/** The surfaces the messages create (`createSurface.surfaceId`, in order). */
export function createdSurfaces(messages) {
  const ids = [];
  for (const m of messages) {
    const c = obj(m) && obj(m.createSurface) ? m.createSurface : null;
    if (c && typeof c.surfaceId === "string") ids.push(c.surfaceId);
  }
  return ids;
}

/** The surface a message is about: the `surfaceId` of its one operation (v0.9's
 * `createSurface`, `updateComponents`, `updateDataModel`, `deleteSurface`); null when it names
 * none (the processor refuses such a message). */
export function surfaceOf(m) {
  if (!obj(m)) return null;
  for (const v of Object.values(m)) {
    if (obj(v) && typeof v.surfaceId === "string") return v.surfaceId;
  }
  return null;
}

/** Every surface the messages are about, in order, once each. */
export function surfacesOf(messages) {
  return [...new Set(messages.map(surfaceOf).filter((id) => id !== null))];
}

// ------------------------------------------------------------------ the person's answer, in words

/** At most this many components kept of one form, and this many forms, by `FormBook`. */
export const BOOK_COMPONENTS_MAX = 2000;
export const BOOK_SURFACES_MAX = 100;
/** An answer shown as names and values has at most this many; more is shown as JSON. */
export const ANSWER_ROWS_MAX = 40;

/** The forms an agent sent on one thread, as data: each surface's components by id, as the agent
 * last sent them (`createSurface` starts the surface anew; `updateComponents` adds or replaces by
 * id), kept once the surface is deleted, so a person's answer reads with the labels of the form it
 * answered, whether or not the renderer drew it. **Each `createSurface` is a generation of its
 * own** (a number, unique on the thread): A2UI lets an agent use a deleted surface's id again, and
 * what was drawn or answered under the first keeps its own components and title. Bounded
 * (BOOK_COMPONENTS_MAX a form, BOOK_SURFACES_MAX forms and surface ids: the oldest goes first). */
export class FormBook {
  constructor() {
    /** generation -> components (id -> the component as the agent sent it). */
    this.forms = new Map();
    /** surface id -> its current generation. */
    this.gens = new Map();
    this.next = 0;
  }

  /** The surface's current generation: that of its last `createSurface` the book noted (or of the
   * first message about it), 0 for an id the book never saw. */
  generation(id) {
    return this.gens.get(id) || 0;
  }

  surface(id, fresh = false) {
    let gen = this.generation(id);
    if (fresh || !gen) {
      gen = ++this.next;
      this.gens.delete(id);
      this.gens.set(id, gen);
      while (this.gens.size > BOOK_SURFACES_MAX) this.gens.delete(this.gens.keys().next().value);
    }
    let s = this.forms.get(gen);
    if (!s) {
      s = new Map();
      this.forms.set(gen, s);
      while (this.forms.size > BOOK_SURFACES_MAX) this.forms.delete(this.forms.keys().next().value);
    }
    return s;
  }

  /** Note one A2UI part's messages, in order. */
  note(messages) {
    for (const m of Array.isArray(messages) ? messages : []) {
      if (!obj(m)) continue;
      const c = obj(m.createSurface) ? m.createSurface : null;
      if (c && typeof c.surfaceId === "string") this.surface(c.surfaceId, true);
      const u = obj(m.updateComponents) ? m.updateComponents : null;
      if (u && typeof u.surfaceId === "string" && Array.isArray(u.components)) {
        const s = this.surface(u.surfaceId);
        for (const x of u.components) {
          if (obj(x) && typeof x.id === "string" && (s.has(x.id) || s.size < BOOK_COMPONENTS_MAX)) s.set(x.id, x);
        }
      }
    }
  }

  /** A surface's components (id -> the component as the agent sent it) in generation `gen` (by
   * default its current one), or null. */
  components(id, gen = this.generation(id)) {
    return (gen && this.forms.get(gen)) || null;
  }

  /** A surface's title in generation `gen` (by default its current one): its first h1 or h2 Text
   * written as a string, or null. */
  title(id, gen = this.generation(id)) {
    const s = this.components(id, gen);
    if (!s) return null;
    for (const c of s.values()) {
      if (c.component === "Text" && (c.variant === "h1" || c.variant === "h2") && typeof c.text === "string" && c.text.trim()) {
        return c.text.trim().slice(0, 120);
      }
    }
    return null;
  }

  /** Whether the surface in generation `gen` (by default its current one) has something a person
   * fills or presses (an input of the basic catalog, a Button); null when the book does not know
   * its components. */
  takesInput(id, gen = this.generation(id)) {
    const s = this.components(id, gen);
    if (!s) return null;
    for (const c of s.values()) if (INPUTS.has(c.component) || c.component === "Button") return true;
    return false;
  }
}

/** Each component's place in the form as it is laid out: depth first from its root through every
 * child a component names (`child`, `children` -- ids, or a template's `componentId` --, a Tabs'
 * `tabs[].child`, a Modal's `trigger` and `content`), then those no other names, in the order the
 * agent sent them. The gate sorts a data part's keys, so an action's context does not keep the
 * form's order; this does. */
function layoutOrder(comps) {
  const rank = new Map();
  const visit = (id) => {
    if (typeof id !== "string" || rank.has(id) || !comps.has(id)) return;
    rank.set(id, rank.size);
    const c = comps.get(id);
    if (!obj(c)) return;
    const named = [c.child, c.trigger, c.content];
    if (Array.isArray(c.children)) named.push(...c.children);
    else if (obj(c.children)) named.push(c.children.componentId);
    if (Array.isArray(c.tabs)) for (const t of c.tabs) if (obj(t)) named.push(t.child);
    for (const n of named) visit(n);
  };
  visit("root");
  for (const id of comps.keys()) if (!rank.has(id)) rank.set(id, rank.size);
  return rank;
}

/** The input components of the basic catalog whose `value` a person sets. */
const INPUTS = new Set(["TextField", "DateTimeInput", "ChoicePicker", "CheckBox", "Slider"]);
const label = (v) => (typeof v === "string" && v.trim() ? v.trim().slice(0, 200) : null);
const pathOf = (v) => (obj(v) && typeof v.path === "string" ? v.path : null);

/** A date and time as an A2UI DateTimeInput writes it (`2026-11-09T19:30`, seconds or a zone
 * allowed) as `2026-11-09 19:30` (a zone after it, `Z` as UTC): the value as the person chose it,
 * never moved to another zone. */
function dateText(s) {
  const m = /^(\d{4}-\d{2}-\d{2})T(\d{2}:\d{2})(:\d{2}(\.\d+)?)?(Z|[+-]\d{2}:\d{2})?$/.exec(s);
  return m ? `${m[1]} ${m[2]}${m[5] ? ` ${m[5] === "Z" ? "UTC" : m[5]}` : ""}` : s;
}

/** One value of an answer as text, or undefined when it is no flat value (an object, a nested
 * list). `field` is the component bound to it, when known: a ChoicePicker's values read as its
 * options' labels. */
function valueText(v, field) {
  if (v === null || v === undefined || v === "") return "—";
  if (typeof v === "boolean") return v ? "Yes" : "No";
  if (typeof v === "number") return Number.isFinite(v) ? String(v) : undefined;
  if (typeof v === "string") return field && field.component === "DateTimeInput" ? dateText(v) : v;
  if (Array.isArray(v) && v.every((x) => typeof x === "string" || typeof x === "number" || typeof x === "boolean")) {
    if (!v.length) return "—";
    const options = field && field.component === "ChoicePicker" && Array.isArray(field.options) ? field.options : [];
    return v.map((x) => {
      const o = options.find((opt) => obj(opt) && String(opt.value) === String(x));
      return (o && label(o.label)) || String(x);
    }).join(", ");
  }
  return undefined;
}

/** A person's answer to a surface, in words: the button they pressed (its label as the
 * form wrote it, else the action's name) and the action's context as [label, value] rows, each
 * value labelled as the form labelled the field bound to the same data path (a TextField's,
 * DateTimeInput's, ChoicePicker's or CheckBox's `label`; a choice by its option's label, a switch
 * Yes or No), else by its key; in the form's order (`layoutOrder`), the rest in the context's. `components` is the form
 * (`FormBook.components`), or null when the thread never saw it. `rows` is null when the context
 * holds what is no flat value, or more than ANSWER_ROWS_MAX: the thread shows its JSON. */
export function answerOf(action, components = null) {
  const a = obj(action) ? action : {};
  const name = typeof a.name === "string" && a.name ? a.name.slice(0, 120) : "an action";
  const context = obj(a.context) ? a.context : {};
  const comps = components instanceof Map ? components : null;
  const button = comps && typeof a.sourceComponentId === "string" ? comps.get(a.sourceComponentId) : null;
  let pressed = null;
  if (obj(button) && typeof button.child === "string" && comps) {
    const t = comps.get(button.child);
    if (obj(t) && t.component === "Text") pressed = label(t.text);
  }
  const act = obj(button) && obj(button.action) ? button.action : null;
  const bound = act && obj(act.event) ? act.event.context : act ? act.context : null;
  const fieldAt = (path) => {
    if (!path || !comps) return null;
    for (const c of comps.values()) if (obj(c) && INPUTS.has(c.component) && pathOf(c.value) === path) return c;
    return null;
  };
  const entries = Object.entries(context);
  const order = comps ? layoutOrder(comps) : new Map();
  let ranked = entries.length <= ANSWER_ROWS_MAX ? [] : null;
  for (const [i, [key, v]] of entries.entries()) {
    if (!ranked) break;
    const field = fieldAt(obj(bound) ? pathOf(bound[key]) : null);
    const text = valueText(v, field);
    if (text === undefined) ranked = null;
    else ranked.push({ at: field && order.has(field.id) ? order.get(field.id) : Number.MAX_SAFE_INTEGER, i, row: [(field && label(field.label)) || key, text] });
  }
  // In the form's order (a field's place in its layout), the rest after in the context's.
  const rows = ranked && ranked.sort((x, y) => x.at - y.at || x.i - y.i).map((r) => r.row);
  return { pressed: pressed || name, rows, empty: entries.length === 0 };
}

/** The reply a renderer's action makes (A2UI v0.9's client-to-server `action`): `{version,
 * action: {name, surfaceId, sourceComponentId, timestamp, context}}`, with the surface's
 * protocol version; null when it is not one the console API takes. Nothing else of the
 * renderer's event (v1.0's catalogId, metadata, userMessage) is sent. */
export function actionMessage(event, version) {
  if (!obj(event) || !VERSIONS.includes(version)) return null;
  const { name, surfaceId, sourceComponentId, timestamp } = event;
  const context = obj(event.context) ? event.context : {};
  for (const s of [name, surfaceId, sourceComponentId, timestamp]) {
    if (typeof s !== "string" || !s || s.length > 256) return null;
  }
  const message = { version, action: { name, surfaceId, sourceComponentId, timestamp, context } };
  let size;
  try {
    size = JSON.stringify(message).length;
  } catch {
    return null;
  }
  return size <= ACTION_MAX ? message : null;
}

/** The body of the thread's reply (`task_message`) for an action: the A2UI message, the
 * catalogs the console renders, and the surfaces' data model when one asked for it
 * (`sendDataModel`). */
export function actionReply(message, dataModel = null) {
  const body = { a2ui: message, a2ui_catalogs: [...CATALOGS] };
  if (obj(dataModel)) body.a2ui_data_model = dataModel;
  return body;
}

/** The body of a text reply: the text and, on every reply (A2UI's extension specification: the
 * capabilities go with every message of a client that renders A2UI, whether or not the thread has
 * shown a surface yet), the catalogs the console renders, with the data model of the surfaces that
 * asked for it. The gate activates A2UI only for an agent whose card declares it; any other agent
 * has no use for the metadata. */
export function textReply(text, dataModel = null) {
  const body = { text, a2ui_catalogs: [...CATALOGS] };
  if (obj(dataModel)) body.a2ui_data_model = dataModel;
  return body;
}

/** The body of a reply that answers an agent's form: its values as one data
 * part (`data`); for a form asked through the Structured Input extension (`form.convention`
 * "structured"), `structured_input` -- the part is then tagged `structured_input`, with the
 * request's id when it had one, and the extension activated, which that extension requires of a
 * client that speaks it; and, as on every reply, the catalogs the console renders with the data
 * model of the surfaces that asked for it. */
export function formReply(values, form = null, dataModel = null) {
  const body = { data: values, a2ui_catalogs: [...CATALOGS] };
  if (obj(form) && form.convention === "structured") {
    body.structured_input = typeof form.inputId === "string" && form.inputId ? { id: form.inputId } : {};
  }
  if (obj(dataModel)) body.a2ui_data_model = dataModel;
  return body;
}
