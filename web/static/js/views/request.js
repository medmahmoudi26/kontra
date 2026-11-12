// The request card of a new conversation: the message the person sends an agent (the agent decides whether it makes a task), in the thread
// itself. A first message is text: the card's skill examples are chips that fill it, and a JSON
// object of parameters can go with it when the card takes application/json. When the card offers an
// AgentStack initial form (js/agentstack.js, drawn by js/agentform.js), that form is the request and
// the text an optional message. One primary button, Run (the initial form's submit_label), which
// Enter in a one-line field presses too. Sent, the card folds to one row (requestRow).

import { CATALOGS } from "../a2uiwire.js";
import { valueRows } from "../agentstack.js";
import { agentForm } from "../agentform.js";
import { h, icon } from "../dom.js";
import { flatRows } from "../taskevents.js";
import { chipRow, clamped, enterSends, phone, rawToggle } from "./cards.js";

const obj = (v) => v !== null && typeof v === "object" && !Array.isArray(v);

/** A JSON object an example holds, or null (an example is a starter either way). */
export function jsonObject(text) {
  const s = typeof text === "string" ? text.trim() : "";
  if (!s.startsWith("{")) return null;
  try {
    const v = JSON.parse(s);
    return obj(v) ? v : null;
  } catch {
    return null;
  }
}

/** What the folded request shows: [label, value] rows of its initial form's values (the form's
 * labels), or of its data (a flat object's names). */
export function requestRows(form, { data = null, values = null } = {}) {
  if (values) return valueRows(form && form.initial_form ? form.initial_form : null, values);
  const rows = flatRows(data);
  if (rows) return rows;
  if (obj(data)) return [["data", JSON.stringify(data).slice(0, 200)]];
  return [];
}

/** The folded request: "Request", its chips, its text in two lines, its "{}". `raw()`: what was
 * sent (the start's body, then the message as the gate recorded it). */
export function requestRow({ label = "Request", rows = [], text = "", raw = null, note = null }) {
  const lead = h("span", { class: "lbl", text: label });
  const parts = [lead];
  if (rows.length) {
    // One line where it fits: the chips take the room between the label and the "{}", and wrap
    // there, not under the label.
    const chips = chipRow(rows);
    chips.style.flex = "1 1 0";
    chips.style.minWidth = "0";
    if (phone()) {
      // A phone has no room beside the label: the chips take the next line, the whole width.
      chips.style.flexBasis = "100%";
      chips.style.order = "1";
    }
    parts.push(chips);
  }
  if (note) parts.push(h("span", { class: "small", text: note }));
  let toggle = null;
  if (raw) {
    toggle = rawToggle("What was sent", raw);
    toggle.button.style.marginLeft = "auto";
    parts.push(toggle.button);
  }
  const head = h("div", { class: "reqsum", dataset: { request: "sent" } }, parts);
  head.style.flexWrap = "wrap";
  const extra = [];
  if (text) {
    const t = clamped([text], 2);
    t.style.flexBasis = "100%";
    extra.push(t);
  }
  if (toggle) {
    toggle.box.style.flexBasis = "100%";
    extra.push(toggle.box);
  }
  for (const n of extra) {
    n.style.order = "2";
    head.appendChild(n);
  }
  return head;
}

/**
 * The editable request card for `form` (the start form) of service `service`. `prefill`: { text,
 * data, values } (Run again). `send(body, summary)`: called with the start's body and what the
 * folded card shows ({ rows, text }); a rejection (a GateError) puts the card back with its error.
 * `discard`: a close button (a second request on a thread). Returns { node, focus, fail }.
 */
export function requestCard(form, { service, prefill = null, send, discard = null }) {
  const parts = [];
  const err = h("p", { class: "ferr", id: "starterr", role: "alert" });
  const initial = form.initial_form ? agentForm(form.initial_form, { prefix: "start", given: prefill ? prefill.values : null }) : null;
  const verb = (initial && form.initial_form.submit_label) || "Run";
  const sendBtn = h("button", { class: "btn primary", type: "button", id: "startsend", text: verb });

  if (form.unsupported_extensions && form.unsupported_extensions.length) {
    parts.push(h("p", { class: "small", id: "startext", text: `Needs ${form.unsupported_extensions.join(", ")}: the gate may refuse a message sent from here.` }));
  }

  const text = h("textarea", { rows: "3", id: "startmessage", "aria-label": "Message", maxlength: "65536", placeholder: `What should ${form.name || service} do?` });
  text.style.minHeight = "64px";
  if (prefill && typeof prefill.text === "string") text.value = prefill.text;
  const messageField = h("div", { class: "field wide" }, [text]);

  // A JSON object of parameters: the card takes JSON and offers no form.
  const params = !initial && form.accepts_json
    ? h("textarea", { rows: "5", id: "startparams", spellcheck: "false", "aria-label": "Parameters (JSON object)", placeholder: "{\n  \"name\": \"value\"\n}" })
    : null;
  const paramsBox = params ? h("div", { class: "field wide", id: "startparamsbox", hidden: true }, [params]) : null;
  const paramsBtn = params ? h("button", { class: "linkbtn", type: "button", id: "startparamsbtn", text: "{ } JSON" }) : null;
  if (paramsBtn) {
    paramsBtn.style.paddingLeft = "0";
    paramsBtn.addEventListener("click", () => {
      paramsBox.hidden = !paramsBox.hidden;
      if (!paramsBox.hidden) params.focus();
    });
  }
  if (params && prefill && obj(prefill.data)) {
    params.value = JSON.stringify(prefill.data, null, 2);
    paramsBox.hidden = false;
  }

  if (initial) {
    parts.push(h("div", { class: "stack", id: "startinitial" }, [
      form.initial_form.description ? h("p", { class: "small", text: form.initial_form.description }) : null,
      initial.node,
    ]));
  } else if (form.initial_form_note) {
    parts.push(h("p", { class: "small", id: "startnote", text: `Its form is not drawn (${form.initial_form_note}).` }));
  }

  // With a form, the message is optional and folded; without one it is the request.
  if (initial) {
    const add = h("button", { class: "linkbtn", type: "button", id: "startaddmsg", text: "+ Add a message" });
    add.style.paddingLeft = "0";
    add.style.alignSelf = "flex-start";
    messageField.hidden = !(prefill && prefill.text);
    add.hidden = !messageField.hidden;
    add.addEventListener("click", () => {
      messageField.hidden = false;
      add.hidden = true;
      text.focus();
    });
    parts.push(add, messageField);
  } else {
    parts.push(messageField);
  }

  // The skills' examples: one click fills the message (a JSON one, the parameters).
  const examples = (form.skills || []).flatMap((s) => (s.examples || []).filter((x) => typeof x === "string" && x.trim()).map((ex) => [s, ex])).slice(0, 8);
  if (examples.length && !initial) {
    const row = h("div", { class: "chips", id: "startskills" }, examples.map(([s, ex]) => {
      const isJson = jsonObject(ex) !== null;
      const chip = h("button", { class: "chip", type: "button", title: `${s.name}: ${ex}`, "aria-label": `Use the example: ${ex.slice(0, 120)}`, dataset: { example: isJson ? "json" : "text" } }, [h("b", { text: isJson ? "json" : "try" }), h("span", { text: ex })]);
      chip.style.maxWidth = "100%";
      chip.addEventListener("click", () => {
        const o = jsonObject(ex);
        if (o && params) {
          params.value = JSON.stringify(o, null, 2);
          paramsBox.hidden = false;
          params.focus();
          return;
        }
        text.value = ex;
        text.focus();
      });
      return chip;
    }));
    parts.push(row);
  }
  if (paramsBox) parts.push(paramsBox);

  const foot = h("div", { class: "reqfoot" }, [sendBtn, paramsBtn, err]);
  err.style.flexBasis = "100%";
  parts.push(foot);

  const close = discard ? h("button", { class: "iconbtn", type: "button", "aria-label": "Discard this request", title: "Discard" }, [icon("x", 13)]) : null;
  if (close) {
    close.style.width = "26px";
    close.style.height = "26px";
    close.style.marginLeft = "auto";
    close.addEventListener("click", discard);
  }
  const node = h("div", { class: "card reqform", id: "startform" }, [
    h("div", { class: "hd" }, [h("h3", { text: "Send message" }), close]),
    h("div", { class: "bd" }, parts),
  ]);
  // Enter in a one-line field runs it, Ctrl+Enter in a text box.
  enterSends(node, sendBtn);

  const fail = (e, field = null) => {
    sendBtn.disabled = false;
    sendBtn.textContent = verb;
    const detail = e && e.detail && typeof e.detail.field === "string" ? e.detail.field : field;
    if (initial && detail && form.initial_form.fields.some((f) => f.id === detail)) {
      initial.showError(detail, e.message || String(e));
      return;
    }
    err.textContent = e && e.message ? e.message : String(e);
  };

  sendBtn.addEventListener("click", () => {
    err.textContent = "";
    if (initial) initial.clearErrors();
    const message = text.value.trim() ? text.value : null;
    const body = { service };
    let rows = [];
    if (initial) {
      const got = initial.collect();
      if (got.error) {
        initial.showError(got.field, got.error);
        return;
      }
      body.form = { values: got.values };
      rows = requestRows(form, { values: got.values });
    } else if (params && params.value.trim()) {
      const o = jsonObject(params.value);
      if (!o) {
        err.textContent = "Parameters: a JSON object ({…}), or leave them empty.";
        return;
      }
      body.data = o;
      rows = requestRows(form, { data: o });
    }
    if (message === null && !body.data && !body.form) {
      err.textContent = "Write what it should do.";
      text.focus();
      return;
    }
    if (message !== null) body.text = message;
    if (form.a2ui) body.a2ui_catalogs = [...CATALOGS];
    sendBtn.disabled = true;
    send(body, { rows, text: message || "" }).catch((e) => fail(e));
  });

  return {
    node,
    focus: () => {
      if (initial) initial.focus();
      else if (!text.closest("[hidden]")) text.focus();
    },
    fail,
  };
}
