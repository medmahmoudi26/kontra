// An AgentStack form drawn (js/agentstack.js's normalised FormRender): the card's initial form on
// a new conversation, or a form an agent asks with on the thread. Built from
// console.css as shipped (`.form`, `.field`, `.sel`, `.check`, `.sw`/`.swrow`, `.artin`); its
// columns and spans, and what the stylesheet lacks, through the CSSOM. A file is read in the page as
// a data: URI, at most FILE_MAX each. Every control keeps `state` current; `collect()` gives the
// values as AgentStack sends them, or the first field's error, shown under that field.

import { FILE_MAX, collectValues, initialState } from "./agentstack.js";
import { h, icon } from "./dom.js";
import { fieldId } from "./formfields.js";
import { sizeText } from "./files.js";

const phone = () => typeof matchMedia === "function" && matchMedia("(max-width:720px)").matches;

/** A textarea one line high that grows with what is typed (CSSOM: console.css gives a field's
 * textarea 82px). */
function growing(el) {
  el.rows = 1;
  el.style.minHeight = "38px";
  el.style.resize = "none";
  el.style.overflowY = "hidden";
  const fit = () => {
    el.style.height = "auto";
    el.style.height = `${Math.min(el.scrollHeight + 2, 320)}px`;
    el.style.overflowY = el.scrollHeight > 320 ? "auto" : "hidden";
  };
  el.addEventListener("input", fit);
  requestAnimationFrame(fit);
  return el;
}

function readFile(file) {
  return new Promise((resolve, reject) => {
    const r = new FileReader();
    r.onload = () => resolve(String(r.result));
    r.onerror = () => reject(r.error || new Error("the browser could not read the file"));
    r.readAsDataURL(file);
  });
}

/** Draw `form` (normalised). `prefix` keeps its ids apart; `given`: values to start from (a
 * fulfilment's, Run again). Returns { node, state, collect, showError, clearErrors, focus }. */
export function agentForm(form, { prefix = "as", given = null } = {}) {
  const state = initialState(form, given);
  const errors = new Map();
  const firsts = new Map();
  const columns = phone() ? 1 : form.columns || 1;
  const grid = h("div", { class: "form", dataset: { agentstack: "form" } });
  grid.style.gridTemplateColumns = `repeat(${columns}, minmax(0, 1fr))`;

  const control = (f) => {
    const id = fieldId(prefix, f.id);
    if (f.type === "text") {
      const long = f.auto_resize === true || (f.default_value || "").length > 80 || (f.placeholder || "").length > 80;
      const el = long
        ? growing(h("textarea", { id, "aria-label": f.label, spellcheck: "true" }))
        : h("input", { type: "text", id, "aria-label": f.label, autocomplete: "off", spellcheck: "false" });
      if (f.placeholder) el.setAttribute("placeholder", f.placeholder);
      if (f.required) el.setAttribute("aria-required", "true");
      el.value = state[f.id];
      el.addEventListener("input", () => { state[f.id] = el.value; });
      firsts.set(f.id, el);
      return el;
    }
    if (f.type === "date") {
      const el = h("input", { type: "date", id, "aria-label": f.label });
      if (f.required) el.setAttribute("aria-required", "true");
      el.value = state[f.id];
      el.addEventListener("input", () => { state[f.id] = el.value; });
      el.addEventListener("change", () => { state[f.id] = el.value; });
      firsts.set(f.id, el);
      return el;
    }
    if (f.type === "singleselect") {
      const sel = h("select", { class: "sel", id, "aria-label": f.label }, [
        h("option", { value: "", text: f.required ? "Choose…" : "—" }),
        ...f.options.map((o) => h("option", { value: o.id, text: o.label })),
      ]);
      sel.value = state[f.id];
      sel.addEventListener("change", () => { state[f.id] = sel.value; });
      firsts.set(f.id, sel);
      return sel;
    }
    if (f.type === "multiselect") {
      const box = h("div", { class: "list", id, role: "group", "aria-label": f.label });
      box.style.gap = "6px";
      f.options.forEach((o, i) => {
        const cb = h("input", { type: "checkbox", id: `${id}-${i}`, value: o.id, checked: state[f.id].includes(o.id) });
        cb.addEventListener("change", () => {
          const on = new Set(state[f.id]);
          if (cb.checked) on.add(o.id);
          else on.delete(o.id);
          state[f.id] = f.options.map((x) => x.id).filter((x) => on.has(x));
        });
        if (i === 0) firsts.set(f.id, cb);
        box.appendChild(h("label", { class: "check", for: `${id}-${i}` }, [cb, o.label]));
      });
      return box;
    }
    const toggle = (sid, label, on, set) => {
      const sw = h("button", { class: "sw", type: "button", id: sid, "aria-pressed": String(on), "aria-label": label }, [h("i")]);
      sw.addEventListener("click", () => {
        const next = sw.getAttribute("aria-pressed") !== "true";
        sw.setAttribute("aria-pressed", String(next));
        set(next);
      });
      const text = h("span", { class: "minw0", text: label });
      text.style.fontSize = "12.5px";
      text.style.color = "var(--ink-2)";
      text.style.cursor = "pointer";
      text.addEventListener("click", () => sw.click());
      return [h("div", { class: "swrow" }, [sw, text]), sw];
    };
    if (f.type === "checkbox") {
      const [row, sw] = toggle(id, f.content || f.label, state[f.id], (v) => { state[f.id] = v; });
      firsts.set(f.id, sw);
      return row;
    }
    if (f.type === "checkbox_group") {
      const box = h("div", { class: "stack", id, role: "group", "aria-label": f.label });
      box.style.gap = "6px";
      f.fields.forEach((c, i) => {
        const [row, sw] = toggle(fieldId(prefix, `${f.id}-${c.id}`), c.content || c.label, state[f.id][c.id], (v) => { state[f.id][c.id] = v; });
        if (i === 0) firsts.set(f.id, sw);
        box.appendChild(row);
      });
      return box;
    }
    // A file: chosen in the page, read as a data: URI (FILE_MAX each), listed with a remove.
    const input = h("input", { type: "file", id, multiple: true, "aria-label": f.label, hidden: true });
    if (f.accept && f.accept.length) input.setAttribute("accept", f.accept.join(","));
    const list = h("div", { class: "stack" });
    list.style.gap = "4px";
    const pick = h("button", { class: "btn sm", type: "button", dataset: { act: "pick" } }, [icon("upload", 13), " Choose a file"]);
    pick.addEventListener("click", () => input.click());
    const draw = () => {
      list.replaceChildren(...state[f.id].map((x, i) => {
        const rm = h("button", { class: "linkbtn", type: "button", "aria-label": `Remove ${x.name || "the file"}`, text: "remove" });
        rm.addEventListener("click", () => {
          state[f.id] = state[f.id].filter((_, j) => j !== i);
          draw();
        });
        return h("div", { class: "artfile", dataset: { picked: x.name || "file" } }, [x.name || "file", x.size !== null ? ` · ${sizeText(x.size)}` : "", " ", rm]);
      }));
    };
    input.addEventListener("change", async () => {
      say(f.id, "");
      const chosen = [...(input.files || [])];
      input.value = "";
      for (const file of chosen) {
        if (file.size > FILE_MAX) {
          say(f.id, `${file.name} is ${sizeText(file.size)}: the console sends files of at most ${FILE_MAX / 1024} KiB.`);
          continue;
        }
        try {
          const uri = await readFile(file);
          state[f.id] = [...state[f.id], { uri, name: file.name, mime_type: file.type || null, size: file.size }];
        } catch (e) {
          say(f.id, `${file.name}: ${e.message || e}`);
        }
      }
      draw();
    });
    draw();
    firsts.set(f.id, pick);
    const zone = h("div", { class: "artin" }, [h("div", { class: "top" }, [pick, f.accept && f.accept.length ? h("span", { class: "artstat", text: f.accept.join(", ") }) : null]), list, input]);
    return zone;
  };

  const say = (fid, text) => {
    const p = errors.get(fid);
    if (p) p.textContent = text;
  };

  for (const f of form.fields) {
    const err = h("p", { class: "ferr", role: "alert" });
    errors.set(f.id, err);
    const lead = f.type === "checkbox" ? null : h("label", { for: fieldId(prefix, f.id) }, [f.label, f.required ? h("span", { class: "ty", text: " required" }) : null]);
    const cell = h("div", { class: "field", dataset: { field: f.id, kind: f.type } }, [lead, control(f), err]);
    if (f.col_span && columns > 1) cell.style.gridColumn = `span ${Math.min(f.col_span, columns)}`;
    grid.appendChild(cell);
  }

  return {
    node: grid,
    state,
    collect: () => collectValues(form, state),
    showError: (fid, text) => {
      say(fid, text);
      const el = firsts.get(fid);
      if (el && text) el.focus();
    },
    clearErrors: () => { for (const p of errors.values()) p.textContent = ""; },
    focus: () => {
      const first = firsts.get(form.fields[0].id);
      if (first) first.focus();
    },
  };
}
