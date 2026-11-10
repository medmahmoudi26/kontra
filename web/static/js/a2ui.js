// The thread's A2UI renderer: an agent's surfaces drawn from the console's own catalog,
// on the official renderer vendored as one module (vendor/a2ui/README.md: @a2ui/web_core's
// MessageProcessor and the Lit element base, @a2ui/lit's <a2ui-surface>). thread.js loads this
// module only once a thread shows A2UI.
//
// **The catalog** is A2UI's basic catalog -- its id, its components' and functions' APIs, so any
// agent written for it is understood -- drawn by the console's elements from console.css as
// shipped: text as the thread's text, buttons `.btn`, fields `.field`, cards `.card`, rows and
// columns `.row` and `.stack`, a switch `.sw`, a choice `.sel`, a range `.rng`. Each renders into
// its own children (no shadow root, no stylesheet, no style attribute: a weight or an alignment
// goes through the CSSOM), and every string an agent sends reaches the page as text
// (Lit's bindings, never HTML: the bundle holds no unsafeHTML, and Trusted Types allows only Lit's
// own template policy). **No remote media**: an Image, Video or AudioPlayer is a line
// saying it is not shown (`img-src 'self'`, and no proxy). **openUrl** opens only an http(s) URL,
// only from a person's click on one of the surface's buttons, in a new tab without opener or
// referrer. A Button's action goes to the thread (`onAction`), which sends it as the person's reply.
// **A field's validation message** (its checks, as web_core evaluates them) shows once the person
// has been through the field -- left it, or changed it -- or has pressed a button of its surface
// that its own checks hold back: such a button stays pressable, sends nothing, and shows every
// field's message (its own, when no field says it) with the first such field focused.
// **A surface is drawn only up to NODES_MAX components**: web_core's resolver builds a
// surface's whole tree, templates repeated per item of their data, so a few kilobytes of nested
// lists could hold the page for minutes; the size is counted first (`surfaceSize`), and a surface
// past it is shown as the agent's JSON.

import {
  A2uiLitElement,
  A2uiSurface,
  AudioPlayerApi,
  ButtonApi,
  CardApi,
  Catalog,
  CheckBoxApi,
  ChoicePickerApi,
  ColumnApi,
  ComponentContext,
  DateTimeInputApi,
  DividerApi,
  GenericBinder,
  IconApi,
  ImageApi,
  ListApi,
  MessageProcessor,
  ModalApi,
  RowApi,
  SliderApi,
  TabsApi,
  TextApi,
  TextFieldApi,
  V09_SPEC_FUNCTIONS,
  VideoApi,
  createFunctionImplementation,
  html,
  live,
  nothing,
} from "../vendor/a2ui/a2ui-0.15.0.js";
import { CATALOG_ID, VERSIONS, surfaceOf, surfacesOf } from "./a2uiwire.js";
import { icon } from "./dom.js";

// The surface element is A2UI's own (@a2ui/lit), defined by the bundle as <a2ui-surface>.
export { A2uiSurface };

// ------------------------------------------------------------------ a person's click

let pressing = false;

/** Run `fn` as the direct result of a person's click on a surface's button: the only time
 * openUrl may open anything. */
function pressed(fn) {
  pressing = true;
  try {
    const r = fn();
    if (r && typeof r.catch === "function") r.catch(() => {});
  } finally {
    pressing = false;
  }
}

/** Whether a URL may be opened now: an http(s) URL, during a person's click (and, where the
 * browser says, with its transient activation). */
export function mayOpen(url, { clicking = pressing, activation = globalThis.navigator && navigator.userActivation } = {}) {
  if (!clicking) return false;
  if (activation && activation.isActive === false) return false;
  if (typeof url !== "string") return false;
  try {
    const u = new URL(url);
    return u.protocol === "https:" || u.protocol === "http:";
  } catch {
    return false;
  }
}

const OPEN_URL = "openUrl";
const openUrl = createFunctionImplementation(
  V09_SPEC_FUNCTIONS.find((f) => f.name === OPEN_URL),
  (args) => {
    const url = args && args.url;
    if (!mayOpen(url)) return;
    window.open(new URL(url).href, "_blank", "noopener,noreferrer");
  },
);

/** The basic catalog's functions, openUrl the console's (above). */
export const FUNCTIONS = Object.freeze(V09_SPEC_FUNCTIONS.map((f) => (f.name === OPEN_URL ? openUrl : f)));

// ------------------------------------------------------------------ the elements

let ids = 0;
const JUSTIFY = { start: "flex-start", center: "center", end: "flex-end", spaceBetween: "space-between", spaceAround: "space-around", spaceEvenly: "space-evenly", stretch: "stretch" };
const ALIGN = { start: "flex-start", center: "center", end: "flex-end", stretch: "stretch" };
const str = (v) => (v === undefined || v === null ? "" : String(v));
const kids = (p) => (Array.isArray(p.children) ? p.children : []);

/** What every element of the catalog shares: light DOM (console.css reaches it), a weight and an
 * accessible label on the element itself. */
class KontraElement extends A2uiLitElement {
  createRenderRoot() {
    return this;
  }

  willUpdate(changed) {
    super.willUpdate(changed);
    const p = this.controller && this.controller.props;
    if (!p) return;
    if (typeof p.weight === "number" && Number.isFinite(p.weight)) this.style.flex = String(p.weight);
    else this.style.removeProperty("flex");
    const label = p.accessibility && p.accessibility.label;
    if (typeof label === "string" && label) this.setAttribute("aria-label", label);
    else this.removeAttribute("aria-label");
  }

  get props() {
    return this.controller && this.controller.props;
  }
}

// ------------------------------------------------------------------ when a field says what is wrong

/** The surface element an element is drawn in (A2UI's <a2ui-surface>, light DOM), or null. */
const surfaceElement = (el) => (el && typeof el.closest === "function" ? el.closest("a2ui-surface") : null);
/** Whether a button of the element's surface was pressed while its checks held it back. */
export const triedOn = (el) => {
  const s = surfaceElement(el);
  return Boolean(s && s.dataset.kontraTried === "1");
};
/** A component's own messages, while its checks fail. */
const messagesOf = (p) => (p && p.isValid === false && Array.isArray(p.validationErrors) ? p.validationErrors.map(str).filter(Boolean) : []);
/** Whether a field shows its messages: after the person has been through it, or a submit was tried. */
export const showsErrors = ({ touched, tried }) => Boolean(touched || tried);

/** A field of the catalog: it remembers whether the person has been through it (focus left it, or
 * its value changed), and shows its validation messages only from then on, or once a button of its
 * surface was pressed while its checks held it back. */
class KontraField extends KontraElement {
  touched = false;

  constructor() {
    super();
    const touch = (e) => {
      if (this.touched) return;
      if (e.type === "focusout" && e.relatedTarget && this.contains(e.relatedTarget)) return;
      this.touch();
    };
    this.addEventListener("focusout", touch);
    this.addEventListener("change", touch);
  }

  touch() {
    if (this.touched) return;
    this.touched = true;
    this.requestUpdate();
  }

  /** Whether its messages show now. */
  shown(p) {
    return messagesOf(p).length > 0 && showsErrors({ touched: this.touched, tried: triedOn(this) });
  }

  /** aria-invalid for its control while a message shows. */
  invalid(p) {
    return this.shown(p) ? "true" : nothing;
  }

  errors(p) {
    return this.shown(p) ? messagesOf(p).map((m) => html`<p class="ferr">${m}</p>`) : nothing;
  }
}
const FIELD_TAGS = "kontra-a2ui-textfield, kontra-a2ui-checkbox, kontra-a2ui-choicepicker, kontra-a2ui-slider, kontra-a2ui-datetime";

class KText extends KontraElement {
  static tagName = "kontra-a2ui-text";
  render() {
    const p = this.props;
    if (!p) return nothing;
    const text = str(p.text);
    switch (p.variant) {
      case "h1":
      case "h2":
        return html`<h3>${text}</h3>`;
      case "h3":
      case "h4":
      case "h5":
        return html`<h4>${text}</h4>`;
      case "caption":
        return html`<span class="small">${text}</span>`;
      default:
        return html`<span class="plain">${text}</span>`;
    }
  }
}

/** A remote picture, film or sound: never fetched (img-src 'self'; no proxy). */
class KMedia extends KontraElement {
  static tagName = "kontra-a2ui-media";
  render() {
    const p = this.props;
    if (!p) return nothing;
    const what = { [ImageApi.name]: "An image", [VideoApi.name]: "A video", [AudioPlayerApi.name]: "A sound" }[this.context && this.context.componentModel ? this.context.componentModel.type : ""] || "Media";
    const said = str(p.description);
    return html`<span class="small" data-media="hidden">${what} is not shown here: the console loads nothing from other sites.${said ? ` (${said})` : ""}</span>`;
  }
}

const ICONS = { check: "check", add: "plus", close: "x", download: "download", upload: "upload", lock: "lock", search: "search", event: "clock", calendarToday: "clock" };

class KIcon extends KontraElement {
  static tagName = "kontra-a2ui-icon";
  render() {
    const p = this.props;
    if (!p) return nothing;
    const name = typeof p.name === "string" ? ICONS[p.name] : null;
    return name ? html`${icon(name, 15)}` : nothing;
  }
}

class KRow extends KontraElement {
  static tagName = "kontra-a2ui-row";
  layout() {
    return "row";
  }
  willUpdate(changed) {
    super.willUpdate(changed);
    const p = this.props;
    if (!p) return;
    this.className = this.layout(p);
    if (p.justify && JUSTIFY[p.justify]) this.style.justifyContent = JUSTIFY[p.justify];
    else this.style.removeProperty("justify-content");
    if (p.align && ALIGN[p.align]) this.style.alignItems = ALIGN[p.align];
    else this.style.removeProperty("align-items");
  }
  render() {
    const p = this.props;
    if (!p) return nothing;
    return html`${kids(p).map((c) => this.renderNode(c))}`;
  }
}

class KColumn extends KRow {
  static tagName = "kontra-a2ui-column";
  layout() {
    return "stack";
  }
}

class KList extends KRow {
  static tagName = "kontra-a2ui-list";
  layout(p) {
    return p && p.direction === "horizontal" ? "row" : "stack";
  }
}

class KCard extends KontraElement {
  static tagName = "kontra-a2ui-card";
  render() {
    const p = this.props;
    if (!p) return nothing;
    return html`<div class="card"><div class="bd stack">${p.child ? this.renderNode(p.child) : nothing}</div></div>`;
  }
}

class KTabs extends KontraElement {
  static tagName = "kontra-a2ui-tabs";
  active = 0;
  render() {
    const p = this.props;
    if (!p || !Array.isArray(p.tabs) || !p.tabs.length) return nothing;
    if (this.active >= p.tabs.length) this.active = 0;
    const pick = (i) => {
      this.active = i;
      this.requestUpdate();
    };
    return html`<div class="stack">
      <div class="seg" role="tablist">${p.tabs.map((t, i) => html`<button type="button" role="tab" aria-pressed=${String(i === this.active)} @click=${() => pick(i)}>${str(t && t.title)}</button>`)}</div>
      <div class="stack">${p.tabs[this.active] && p.tabs[this.active].child ? this.renderNode(p.tabs[this.active].child) : nothing}</div>
    </div>`;
  }
}

/** A modal, in the thread: its trigger, and its content opened below it. */
class KModal extends KontraElement {
  static tagName = "kontra-a2ui-modal";
  open = false;
  render() {
    const p = this.props;
    if (!p) return nothing;
    const toggle = (on) => {
      this.open = on;
      this.requestUpdate();
    };
    return html`<div class="stack">
      <div @click=${() => toggle(true)}>${p.trigger ? this.renderNode(p.trigger) : nothing}</div>
      ${this.open ? html`<div class="card"><div class="bd stack">${p.content ? this.renderNode(p.content) : nothing}<div class="row"><button type="button" class="btn sm" @click=${() => toggle(false)}>Close</button></div></div></div>` : nothing}
    </div>`;
  }
}

class KDivider extends KontraElement {
  static tagName = "kontra-a2ui-divider";
  render() {
    return html`<div class="sec" role="separator"></div>`;
  }
}

/** A Button. Pressed while its checks fail, it sends nothing: its surface shows every field's
 * message, the first such field takes the focus, and the button says what no field says. */
class KButton extends KontraElement {
  static tagName = "kontra-a2ui-button";

  /** Its surface's fields, as they are on the page now. */
  fields() {
    const root = surfaceElement(this) || this.parentElement;
    return root ? [...root.querySelectorAll(FIELD_TAGS)] : [];
  }

  /** What its checks say that no field of its surface shows. */
  own(p) {
    const said = new Set(this.fields().filter((f) => f.shown(f.props)).flatMap((f) => messagesOf(f.props)));
    return messagesOf(p).filter((m) => !said.has(m));
  }

  /** A submit tried while the checks fail: the surface shows what is wrong, the first field that says
   * so takes the focus. */
  async reveal() {
    const s = surfaceElement(this);
    if (s) s.dataset.kontraTried = "1";
    const fields = this.fields();
    for (const f of fields) f.requestUpdate();
    await Promise.all(fields.map((f) => f.updateComplete));
    this.requestUpdate();
    const first = fields.find((f) => f.shown(f.props));
    const control = first && first.querySelector("input, textarea, select, button");
    if (!control) return;
    control.focus({ preventScroll: true });
    // Into view when it is not (the thread's banner sticks over the top of the page).
    const r = first.getBoundingClientRect();
    if (r.top < 120 || r.bottom > window.innerHeight) first.scrollIntoView({ block: "center" });
  }

  render() {
    const p = this.props;
    if (!p) return nothing;
    const cls = p.variant === "primary" ? "btn primary" : p.variant === "borderless" ? "btn sm" : "btn";
    const held = p.isValid === false;
    const click = () => {
      if (typeof p.action !== "function") return;
      if (held) {
        this.reveal();
        return;
      }
      pressed(() => p.action());
    };
    const button = html`<button type="button" class=${cls} data-held=${held ? "1" : nothing} @click=${click}>${p.child ? this.renderNode(p.child) : nothing}</button>`;
    const own = held && triedOn(this) ? this.own(p) : [];
    return own.length ? html`<div class="stack">${button}${own.map((m) => html`<p class="ferr">${m}</p>`)}</div>` : button;
  }
}

class KTextField extends KontraField {
  static tagName = "kontra-a2ui-textfield";
  fid = `a2ui-f${++ids}`;
  render() {
    const p = this.props;
    if (!p) return nothing;
    const set = (e) => p.setValue && p.setValue(e.target.value);
    const value = live(str(p.value));
    let control;
    const bad = this.invalid(p);
    if (p.variant === "longText") control = html`<textarea id=${this.fid} rows="3" aria-invalid=${bad} .value=${value} @input=${set}></textarea>`;
    else if (p.variant === "obscured") control = html`<input id=${this.fid} type="password" class="sel" autocomplete="off" aria-invalid=${bad} .value=${value} @input=${set}>`;
    else control = html`<input id=${this.fid} type="text" autocomplete="off" spellcheck="false" inputmode=${p.variant === "number" ? "decimal" : "text"} aria-invalid=${bad} .value=${value} @input=${set}>`;
    return html`<div class="field">
      ${p.label ? html`<label for=${this.fid}>${str(p.label)}</label>` : nothing}
      ${control}
      ${this.errors(p)}
    </div>`;
  }
}

class KCheckBox extends KontraField {
  static tagName = "kontra-a2ui-checkbox";
  render() {
    const p = this.props;
    if (!p) return nothing;
    const on = p.value === true;
    return html`<div class="stack"><div class="swrow">
      <button type="button" class="sw" aria-pressed=${String(on)} aria-label=${str(p.label)} aria-invalid=${this.invalid(p)} @click=${() => { this.touch(); if (p.setValue) p.setValue(!on); }}><i></i></button>
      <span class="plain">${str(p.label)}</span>
    </div>${this.errors(p)}</div>`;
  }
}

class KChoicePicker extends KontraField {
  static tagName = "kontra-a2ui-choicepicker";
  fid = `a2ui-f${++ids}`;
  render() {
    const p = this.props;
    if (!p) return nothing;
    const options = Array.isArray(p.options) ? p.options : [];
    const chosen = Array.isArray(p.value) ? p.value.map(String) : [];
    const set = (v) => p.setValue && p.setValue(v);
    let control;
    if (p.variant === "multipleSelection") {
      const flip = (v, on) => set(on ? [...chosen.filter((x) => x !== v), v] : chosen.filter((x) => x !== v));
      control = html`<div class="stack">${options.map((o) => html`<label class="check"><input type="checkbox" .checked=${live(chosen.includes(str(o.value)))} @change=${(e) => flip(str(o.value), e.target.checked)}>${str(o.label)}</label>`)}</div>`;
    } else {
      control = html`<select id=${this.fid} class="sel" aria-invalid=${this.invalid(p)} .value=${live(chosen[0] || "")} @change=${(e) => set(e.target.value ? [e.target.value] : [])}>
        <option value="">Choose…</option>
        ${options.map((o) => html`<option value=${str(o.value)} ?selected=${chosen[0] === str(o.value)}>${str(o.label)}</option>`)}
      </select>`;
    }
    return html`<div class="field">
      ${p.label ? html`<label for=${this.fid}>${str(p.label)}</label>` : nothing}
      ${control}
      ${this.errors(p)}
    </div>`;
  }
}

class KSlider extends KontraField {
  static tagName = "kontra-a2ui-slider";
  fid = `a2ui-f${++ids}`;
  render() {
    const p = this.props;
    if (!p) return nothing;
    const min = typeof p.min === "number" ? p.min : 0;
    const max = typeof p.max === "number" ? p.max : 100;
    const value = typeof p.value === "number" ? p.value : min;
    return html`<div class="field">
      ${p.label ? html`<label for=${this.fid}>${str(p.label)}</label>` : nothing}
      <div class="rng"><input id=${this.fid} type="range" min=${String(min)} max=${String(max)} .value=${live(String(value))} @input=${(e) => p.setValue && p.setValue(Number(e.target.value))}><output>${String(value)}</output></div>
      ${this.errors(p)}
    </div>`;
  }
}

class KDateTimeInput extends KontraField {
  static tagName = "kontra-a2ui-datetime";
  fid = `a2ui-f${++ids}`;
  render() {
    const p = this.props;
    if (!p) return nothing;
    const both = p.enableDate && p.enableTime;
    const type = both ? "datetime-local" : p.enableTime ? "time" : "date";
    const cut = (v) => {
      const s = str(v);
      return type === "date" ? s.slice(0, 10) : type === "time" ? s.slice(0, 5) : s.slice(0, 16);
    };
    return html`<div class="field">
      ${p.label ? html`<label for=${this.fid}>${str(p.label)}</label>` : nothing}
      <input id=${this.fid} type=${type} class=${type === "time" ? "sel" : ""} min=${cut(p.min) || nothing} max=${cut(p.max) || nothing} aria-invalid=${this.invalid(p)} .value=${live(cut(p.value))} @input=${(e) => p.setValue && p.setValue(e.target.value)}>
      ${this.errors(p)}
    </div>`;
  }
}

const component = (api, element) => ({ ...api, tagName: element.tagName, element });

/** The console's catalog: A2UI's basic catalog, each component drawn by the console. */
export const catalog = new Catalog(CATALOG_ID, "0.9", [
  component(TextApi, KText),
  component(ImageApi, KMedia),
  component(IconApi, KIcon),
  component(VideoApi, KMedia),
  component(AudioPlayerApi, KMedia),
  component(RowApi, KRow),
  component(ColumnApi, KColumn),
  component(ListApi, KList),
  component(CardApi, KCard),
  component(TabsApi, KTabs),
  component(ModalApi, KModal),
  component(DividerApi, KDivider),
  component(ButtonApi, KButton),
  component(TextFieldApi, KTextField),
  component(CheckBoxApi, KCheckBox),
  component(ChoicePickerApi, KChoicePicker),
  component(SliderApi, KSlider),
  component(DateTimeInputApi, KDateTimeInput),
], FUNCTIONS);

// ------------------------------------------------------------------ how large a surface is

/** At most this many components drawn in one surface. web_core's resolver builds a surface's whole
 * tree when it is drawn, and a List, Row or Column whose children are a template repeats its child
 * once per item of the data at its path (up to 10,000), and templates nest: a part of a few
 * kilobytes can ask for 10^8 components and hold the page for minutes, on every visit of the
 * thread. A surface past this is not drawn (the thread shows the agent's JSON with a note). */
export const NODES_MAX = 1000;
/** web_core's own cap on one template's children (`MAX_DYNAMIC_CHILD_LIST_SIZE`). */
const TEMPLATE_MAX = 10000;

const COMPONENT_ID = "REF:#/$defs/ComponentId";
const CHILD_LIST = "REF:#/$defs/ChildList";
const isObj = (v) => v !== null && typeof v === "object" && !Array.isArray(v);

/** A zod schema without its optional/nullable/default/effects wrappers, and the descriptions met. */
function unwrap(schema) {
  const said = [];
  let s = schema;
  for (;;) {
    if (s.description) said.push(s.description);
    const t = s._def.typeName;
    if (t === "ZodOptional" || t === "ZodNullable" || t === "ZodDefault") s = s._def.innerType;
    else if (t === "ZodEffects") s = s._def.schema;
    else return { s, said };
  }
}
const marked = (said, ref) => said.some((d) => d.startsWith(ref));
const childListUnion = (s) => s._def.typeName === "ZodUnion"
  && s._def.options.some((o) => o._def.typeName === "ZodObject" && o.shape.componentId && o.shape.path);

const refsOf = new WeakMap();
/** The properties of a component that name its children, read from its API's schema as web_core's
 * resolver reads them (`extractRefFields`): a child list (`children`: ids, or a template), one
 * child (`child`, a Modal's `trigger` and `content`), or an array of objects naming one (a Tabs'
 * `tabs[].child`). */
export function childRefs(api) {
  let refs = refsOf.get(api);
  if (refs) return refs;
  refs = [];
  const { s } = unwrap(api.schema);
  const shape = s._def.typeName === "ZodObject" ? s.shape : {};
  for (const [key, value] of Object.entries(shape)) {
    const f = unwrap(value);
    if (marked(f.said, CHILD_LIST) || childListUnion(f.s)) refs.push({ key, kind: "list" });
    else if (marked(f.said, COMPONENT_ID)) refs.push({ key, kind: "single" });
    else if (f.s._def.typeName === "ZodArray") {
      const e = unwrap(f.s._def.type);
      if (marked(e.said, COMPONENT_ID)) refs.push({ key, kind: "list" });
      else if (e.s._def.typeName === "ZodObject") {
        const sub = Object.entries(e.s.shape).filter(([, v]) => marked(unwrap(v).said, COMPONENT_ID)).map(([k]) => k);
        if (sub.length) refs.push({ key, kind: "nested", sub });
      }
    }
  }
  refsOf.set(api, refs);
  return refs;
}

/** How many components drawing the surface makes, counted as web_core's resolver builds them --
 * each component bound by web_core's own binder at its data path, a template's children one per
 * item, a missing or cyclic one a placeholder -- and counted only as far as `limit` + 1. A surface
 * the binder cannot read counts as more than any limit. */
export function surfaceSize(surface, limit = NODES_MAX) {
  let n = 0;
  const chain = [];
  const visit = (id, path, parent) => {
    if (n > limit) return;
    n += 1;
    const model = surface.componentsModel.get(id);
    if (!model) return;
    if (chain.some(([c, p]) => c === id && p === path)) return;
    const api = model.catalog && model.catalog.components.get(model.type);
    if (!api) return;
    const refs = childRefs(api).filter((r) => r.key in (model.properties || {}));
    if (!refs.length) return;
    const context = new ComponentContext(surface, id, path, parent);
    const props = new GenericBinder(context, api.schema).snapshot;
    const child = (v, at = path) => {
      if (typeof v === "string" && v) visit(v, at, context.dataContext);
    };
    chain.push([id, path]);
    for (const { key, kind, sub } of refs) {
      const v = props[key];
      if (!Array.isArray(v)) {
        if (kind === "single") child(v);
        continue;
      }
      if (kind === "list") {
        for (const item of v.slice(0, TEMPLATE_MAX)) {
          if (n > limit) break;
          if (typeof item === "string") child(item);
          else if (isObj(item) && typeof item.id === "string" && typeof item.basePath === "string") child(item.id, item.basePath);
          else if (isObj(item) && typeof item.componentId === "string") child(item.componentId);
        }
      } else if (kind === "nested") {
        for (const item of v) if (isObj(item)) for (const k of sub) child(item[k]);
      }
    }
    chain.pop();
  };
  try {
    visit(surface.rootId || "root", "/", undefined);
  } catch {
    return Infinity;
  }
  return n;
}

// ------------------------------------------------------------------ one thread's surfaces

/** What the processor checks of each message (web_core's integrity checks, with its depth and
 * path limits): a component or function the catalog does not have is refused; a surface may still
 * arrive over several messages (a reference or the root that comes later). */
const VALIDATION = Object.freeze({
  allowUnknownElements: false,
  allowOrphanComponents: true,
  allowDanglingReferences: true,
  allowMissingRoot: true,
});

const normalVersion = (v) => (v === "v0.9.1" || v === "0.9.1" ? "v0.9.1" : "v0.9");
const sentence = (s) => String(s).slice(0, 300).replace(/[.\s]+$/, "");
const why = (r) => sentence((r && r.error && r.error.message) || "an A2UI message the console cannot read");
const thrown = (e) => sentence((e && e.message) || e);

/** One thread's A2UI: a MessageProcessor fed each A2UI part in the order the events came.
 *
 * **A part is not a unit** (A2UI's extension specification, "Processing Rules"): each message is
 * applied on its own, in order; one the processor refuses is reported with its reason and the rest
 * still apply. A surface sent again (a `createSurface` for an id still shown: an agent's next turn)
 * replaces the shown one only when its messages in the part apply on their own and it fits
 * NODES_MAX -- tried first on a processor of its own -- otherwise the shown form stays and those
 * messages are refused. A surface is drawn only while it fits NODES_MAX: every shown surface a part
 * is about is taken off the page while the part is applied (so web_core's resolver never builds
 * what the part asks for) and put back only if it still fits. */
export class Surfaces {
  /** `onAction(event, version)`: a Button's action (A2UI's `action`, its surface's protocol
   * version). `onLarge(id)`: a shown surface grew past NODES_MAX and was taken off the page. */
  constructor({ onAction, onLarge = () => {} }) {
    this.onAction = onAction;
    this.onLarge = onLarge;
    this.versions = new Map();
    this.elements = new Map();
    this.processor = new MessageProcessor([catalog], (event) => {
      this.onAction(event, this.versions.get(event.surfaceId) || "v0.9");
    }, { validationConfig: VALIDATION });
    this.processor.onSurfaceDeleted((id) => {
      this.elements.delete(id);
    });
  }

  /** Apply one part's messages: `{ created, refused, large, shown }` -- the surfaces it created
   * (ids, in order); the messages refused (`{ index, why }`, `why` a sentence); the surfaces it is
   * about that are past NODES_MAX (not drawn); and those to draw now, where the part is (it is
   * about them, they exist, fit and have their root, and are not on the page yet). */
  feed(messages) {
    const about = surfacesOf(messages);
    const trials = this.replacements(messages);
    // Off the page while the part applies: no resolver builds what the part asks for.
    const off = [];
    for (const id of about) {
      const el = this.elements.get(id);
      if (el && el.parentNode) {
        off.push([id, el, el.parentNode, el.nextSibling]);
        el.remove();
      }
    }
    const created = [];
    const refused = [];
    const kept = new Map();
    const sub = this.processor.onSurfaceCreated((s) => created.push(s.id));
    try {
      messages.forEach((m, index) => {
        const id = surfaceOf(m);
        const trial = trials.get(index);
        if (trial && !trial.ok) kept.set(id, trial.why);
        else if (trial) this.processor.processMessagesWithErrors([{ version: this.version(id), deleteSurface: { surfaceId: id } }]);
        if (kept.has(id)) {
          refused.push({ index, why: `the console kept the form it shows: the agent's new one was refused (${kept.get(id)})` });
          return;
        }
        try {
          const errors = this.processor.processMessagesWithErrors([m]);
          if (errors.length) refused.push({ index, why: why(errors[0]) });
        } catch (e) {
          refused.push({ index, why: thrown(e) });
        }
      });
    } finally {
      sub.unsubscribe();
    }
    for (const m of messages) {
      if (isObj(m) && isObj(m.createSurface) && created.includes(m.createSurface.surfaceId)) {
        this.versions.set(m.createSurface.surfaceId, normalVersion(m.version));
      }
    }
    const large = about.filter((id) => this.has(id) && !this.fits(id));
    for (const [id, el, parent, next] of off) {
      if (this.elements.get(id) !== el) continue; // deleted or replaced meanwhile
      if (large.includes(id)) {
        this.elements.delete(id);
        this.onLarge(id);
      } else {
        parent.insertBefore(el, next && next.parentNode === parent ? next : null);
      }
    }
    const shown = about.filter((id) => !large.includes(id) && !this.elements.has(id) && this.drawable(id));
    return { created, refused, large, shown };
  }

  /** For each `createSurface` of the part naming a surface still shown, whether its messages (that
   * one and those after it about the same surface) apply on their own and fit: tried on a scratch
   * processor, so the shown form is replaced only by one that is drawn. */
  replacements(messages) {
    const trials = new Map();
    const seen = new Set();
    messages.forEach((m, index) => {
      const id = isObj(m) && isObj(m.createSurface) ? m.createSurface.surfaceId : null;
      if (typeof id !== "string" || seen.has(id)) return;
      seen.add(id);
      if (!this.has(id)) return;
      const own = messages.filter((x, i) => i >= index && surfaceOf(x) === id);
      const scratch = new MessageProcessor([catalog], () => {}, { validationConfig: VALIDATION });
      try {
        let trial = { ok: true };
        for (const x of own) {
          const errors = scratch.processMessagesWithErrors([x]);
          if (errors.length) {
            trial = { ok: false, why: why(errors[0]) };
            break;
          }
        }
        const s = scratch.getSurface(id);
        if (trial.ok && s && surfaceSize(s) > NODES_MAX) trial = { ok: false, why: `more than ${NODES_MAX} components` };
        trials.set(index, trial);
      } catch (e) {
        trials.set(index, { ok: false, why: thrown(e) });
      } finally {
        scratch.dispose();
      }
    });
    return trials;
  }

  /** Whether the surface is (still) shown. */
  has(id) {
    return Boolean(this.processor.getSurface(id));
  }

  /** Whether the surface has its root component: something to draw. */
  drawable(id) {
    const s = this.processor.getSurface(id);
    return Boolean(s && s.componentsModel.get(s.rootId || "root"));
  }

  /** Whether drawing the surface makes at most NODES_MAX components. */
  fits(id) {
    const s = this.processor.getSurface(id);
    return Boolean(s) && surfaceSize(s) <= NODES_MAX;
  }

  /** The surface's element: A2UI's <a2ui-surface>, drawing with the console's catalog (one per
   * surface; feed takes it off the page and puts it back). */
  element(id) {
    let el = this.elements.get(id);
    if (!el) {
      el = document.createElement("a2ui-surface");
      el.surface = this.processor.getSurface(id);
      this.elements.set(id, el);
    }
    return el;
  }

  /** The surface's protocol version ("v0.9" or "v0.9.1"). */
  version(id) {
    return this.versions.get(id) || VERSIONS[0];
  }

  /** A2UI's `a2uiClientDataModel` for the next message: the data model of every surface that
   * asked to receive it (`sendDataModel`), which the specification sends with every message, in
   * the newest version among them; null when none asked. */
  dataModels() {
    const surfaces = {};
    let version = "v0.9";
    for (const [id, s] of this.processor.getSurfaces()) {
      if (!s.sendDataModel) continue;
      surfaces[id] = s.dataModel.get("/");
      if (this.version(id) === "v0.9.1") version = "v0.9.1";
    }
    return Object.keys(surfaces).length ? { version, surfaces } : null;
  }

  /** Subscribe to a surface's deletion (the agent's deleteSurface, or a replacement). */
  onDeleted(fn) {
    return this.processor.onSurfaceDeleted((id) => fn(id));
  }

  dispose() {
    for (const id of [...this.processor.getSurfaces().keys()]) {
      this.processor.processMessagesWithErrors([{ version: this.version(id), deleteSurface: { surfaceId: id } }]);
    }
    this.processor.dispose();
  }
}
