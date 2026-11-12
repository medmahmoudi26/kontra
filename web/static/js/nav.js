// The top bar's sections and breadcrumb (#crumb), and the views' sections in the page: one place
// that hides every view before the router shows one, and says where the person is. A task's thread
// (#/t/<task>), a conversation (#/c/...) and a new conversation (#/run/<service>) show under their section
// as a trail. Below 720px console.css hides the crumb; the section links stay.

import { offers } from "./app.js";
import { $, fill, h } from "./dom.js";

/** Every view of the page (index.html), each a <section> the router shows one at a time. */
export const VIEWS = Object.freeze([
  "#view-tasks", "#view-contexts", "#view-thread", "#view-traffic", "#view-exchange", "#view-calls", "#view-cards",
  "#view-playground",
]);

/** The sections: [key, href, label, feature] (a section whose feature /v1/info leaves out is hidden). In A2A every
 * message belongs to a context, and a task is what an agent may create in it: Contexts comes first and is the landing
 * page (D360); a backend without contexts lands on Tasks. */
export const SECTIONS = Object.freeze([
  ["contexts", "#/", "Contexts", "contexts"],
  ["tasks", "#/tasks", "Tasks", null],
  ["traffic", "#/traffic", "Traffic", "traffic"],
  ["calls", "#/calls", "MCP calls", "calls"],
  ["cards", "#/cards", "Cards", null],
  ["playground", "#/play", "Playground", "playground"],
]);

/** The sections the backend offers. */
export const sections = () => SECTIONS.filter(([, , , feature]) => feature === null || offers(feature));

/** Hide every view (the router, before it shows one). */
export function hideViews() {
  for (const id of VIEWS) {
    const v = $(id);
    if (v) v.hidden = true;
  }
}

/** Show the view `id` alone, under `section` (one of SECTIONS' keys) with the trail `trail` (a
 * string, or [label, href] pairs then a last string). */
export function showView(id, section, trail = null) {
  hideViews();
  $(id).hidden = false;
  crumb(section, trail);
  navLinks(section);
}

/** The top bar's section links, the current one marked. */
export function navLinks(section) {
  const root = $("#sections");
  if (!root) return;
  fill(root, sections().map(([key, href, label]) => h("a", { href, class: key === section ? "cur" : null, "aria-current": key === section ? "page" : null, text: label })));
}

/** The crumb: the current section (a link back when a trail follows), then the trail. */
export function crumb(section, trail = null) {
  const root = $("#crumb");
  if (!root) return;
  const steps = trail === null ? [] : [].concat(trail);
  const sec = SECTIONS.find(([key]) => key === section);
  const parts = [];
  if (sec && steps.length) {
    parts.push(h("button", { type: "button", text: sec[2], onclick: () => { location.hash = sec[1]; } }));
  }
  steps.forEach((step, i) => {
    if (parts.length) parts.push(h("span", { class: "sep", text: "/" }));
    if (Array.isArray(step) && i < steps.length - 1) {
      parts.push(h("button", { type: "button", text: step[0], onclick: () => { location.hash = step[1]; } }));
    } else {
      parts.push(h("span", { class: "cur", "aria-current": i === steps.length - 1 ? "page" : null, text: Array.isArray(step) ? step[0] : step }));
    }
  });
  fill(root, parts);
  root.hidden = parts.length === 0;
}
