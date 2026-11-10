// The DOM of a form's typed fields (js/agentschema.js's spec): an agent's form on the thread
// (js/views/thread.js: the A2A samples' form part, a Structured Input request). Built from
// console.css as shipped: `.field` with its label, a text input,
// a textarea, a `.sel` choice, a `.sw` switch, a date input; no style attribute. `values` is the
// form's state (name -> string, or boolean for a switch), which every control keeps current, and
// `prefix` keeps one form's ids apart from another's.

import { h } from "./dom.js";

/** The id of field `name` in the form `prefix`. */
export const fieldId = (prefix, name) => `${prefix}-${name.replace(/[^A-Za-z0-9_-]/g, "_")}`;

const TY = { json: "JSON", long: "text", url: "url", email: "email", date: "date", enum: "choice", boolean: "yes / no" };

/** One field's control, its value kept in `values[f.name]`. */
export function control(f, values, prefix = "form") {
  const id = fieldId(prefix, f.name);
  if (f.kind === "boolean") {
    const on = f.initial === true;
    if (on) values[f.name] = true;
    const b = h("button", { class: "sw", type: "button", "aria-pressed": String(on), "aria-label": f.label, id, disabled: f.readOnly }, [h("i")]);
    b.addEventListener("click", () => {
      const next = b.getAttribute("aria-pressed") !== "true";
      b.setAttribute("aria-pressed", String(next));
      values[f.name] = next;
    });
    return b;
  }
  if (f.kind === "enum") {
    const labels = f.labels || f.options.map(String);
    const sel = h("select", { class: "sel", id, "aria-label": f.label, disabled: f.readOnly }, [
      h("option", { value: "", text: f.required ? "Choose…" : "—" }),
      ...labels.map((o, i) => h("option", { value: String(i), text: String(o) })),
    ]);
    sel.value = f.initial;
    values[f.name] = sel.value;
    sel.addEventListener("change", () => { values[f.name] = sel.value; });
    return sel;
  }
  const long = f.kind === "long" || f.kind === "json";
  const inputmode = { integer: "numeric", number: "decimal", url: "url", email: "email" }[f.kind] || null;
  const el = long
    ? h("textarea", { rows: f.kind === "json" ? "4" : "3", id, "aria-label": f.label, spellcheck: f.kind === "json" ? "false" : "true" })
    : h("input", {
      // A date is the browser's calendar; every other field a text input, which console.css styles
      // (a URL or an email says so to a phone's keyboard through inputmode).
      type: f.kind === "date" ? "date" : "text",
      id,
      "aria-label": f.label,
      spellcheck: "false",
      autocomplete: "off",
      inputmode,
    });
  if (f.max && f.kind !== "json") el.setAttribute("maxlength", String(f.max));
  if (f.placeholder) el.setAttribute("placeholder", f.placeholder);
  if (f.required) el.setAttribute("aria-required", "true");
  if (f.readOnly) el.disabled = true;
  el.value = typeof f.initial === "string" ? f.initial : "";
  values[f.name] = el.value;
  el.addEventListener("input", () => { values[f.name] = el.value; });
  return el;
}

/** One field: its label (with its kind and whether it is required), its control, its hint, and
 * the choices a description lists as suggestions (a datalist: the field stays free text). `wide`:
 * the whole row (a form puts its short fields side by side). */
export function fieldNode(f, values, prefix = "form", { wide = true } = {}) {
  const id = fieldId(prefix, f.name);
  const ty = [TY[f.kind] || f.kind, f.required ? "required" : null, f.readOnly ? "fixed" : null].filter(Boolean).join(" · ");
  const ctl = control(f, values, prefix);
  let list = null;
  if (f.suggest && f.suggest.length && ctl.tagName === "INPUT") {
    list = h("datalist", { id: `${id}-choices` }, f.suggest.map((w) => h("option", { value: w })));
    ctl.setAttribute("list", `${id}-choices`);
  }
  const full = wide || f.kind === "long" || f.kind === "json" || f.kind === "url";
  return h("div", { class: `field${full ? " wide" : ""}`, dataset: { field: f.name, kind: f.kind } }, [
    h("label", { for: id }, [f.label, h("span", { class: "ty", text: ` ${ty}` })]),
    ctl,
    list,
    f.hint ? h("span", { class: "hint", text: f.hint }) : null,
  ]);
}
