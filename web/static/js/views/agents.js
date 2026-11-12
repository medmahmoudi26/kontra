// Cards (#/cards, GET /v1/services): one card per service behind this gate, as Kontra Cloud's catalog
// draws them. An A2A service shows its served agent card (the `agent` summary: name, version,
// description, provider, skills); an MCP server its URL. Each says whether an agent is reachable
// now and how long its ended tasks are kept; an A2A service has Send message (the agent decides whether a task exists). The description is
// clamped to two lines: a small down arrow under the card, shown only when there is more than the
// card shows, opens the full description and each skill's details, and turns up to close them.
// Everything a card says comes from the agent: it is set as text, never as markup.

import { gate, offers } from "../app.js";
import { $, fill, h, icon } from "../dom.js";
import { showView } from "../nav.js";
import { jsonBox } from "./cards.js";
import { copyButton, failure, serviceAvatar, span } from "./common.js";
import { validationMark } from "./wire.js";

let generation = 0;

export function leaveCards() {
  generation += 1;
  const root = $("#cards");
  if (root) fill(root, []);
}

/** Whether an agent of service `s` is reachable now. */
export const reachable = (s) => (typeof s.reachable === "boolean" ? s.reachable : Array.isArray(s.sessions) && s.sessions.length > 0);

/** A service's task retention in words: "7d", "kept forever", or "—". */
export function keptText(s) {
  const r = s.retention;
  if (r === undefined || r === null) return "—";
  return Number(r) === 0 ? "kept forever" : span(r);
}

const nonEmpty = (xs) => (Array.isArray(xs) ? xs.filter((x) => typeof x === "string" && x.trim()) : []);

/** A skill's details, for the open card: its description, examples and input/output modes. */
function skillDetail(sk) {
  const ins = nonEmpty(sk.input_modes);
  const outs = nonEmpty(sk.output_modes);
  const ex = nonEmpty(sk.examples);
  const modes = [ins.length ? `in: ${ins.join(", ")}` : "", outs.length ? `out: ${outs.join(", ")}` : ""].filter(Boolean).join(" · ");
  return h("div", { class: "skill", dataset: { skill: sk.name } }, [
    h("div", { class: "row" }, [h("b", { text: sk.name }), ...nonEmpty(sk.tags).map((t) => h("span", { class: "tag", text: t }))]),
    sk.description ? h("p", { class: "plain", text: sk.description }) : null,
    ex.length ? h("div", { class: "ex" }, ex.map((x) => h("code", { text: x }))) : null,
    modes ? h("p", { class: "small mono", text: modes }) : null,
  ]);
}

/** Whether a skill has anything the closed card does not show. */
const hasDetail = (sk) => Boolean(sk.description) || nonEmpty(sk.examples).length > 0 || nonEmpty(sk.input_modes).length > 0
  || nonEmpty(sk.output_modes).length > 0 || nonEmpty(sk.tags).length > 0;

function card(s) {
  const up = reachable(s);
  const a = s.kind === "a2a" ? s.agent : null;
  const skills = a && Array.isArray(a.skills) ? a.skills : [];
  const note = h("span", { class: "small" });
  const title = (a && a.name) || s.name;
  const said = s.kind === "mcp"
    ? "An MCP server: its tools are in tools/list."
    : a ? (a.description || "An A2A agent.") : "No card yet: is its node connected (kontra connect)?";
  const desc = h("p", { class: "desc plain", text: said });
  const details = h("div", { class: "details stack", hidden: true }, skills.filter(hasDetail).map(skillDetail));
  const arrow = h("button", { class: "expand", type: "button", "aria-expanded": "false", "aria-label": `More about ${title}`, title: "More", hidden: true }, [icon("chev", 14)]);
  const art = h("article", { class: `scard${up ? "" : " off"}`, dataset: { service: s.name, kind: s.kind } }, [
    h("div", { class: "row1" }, [
      serviceAvatar(s.name, s.kind),
      h("div", { class: "id" }, [
        h("div", { class: "nm" }, [h("b", { text: title }), a && a.version ? h("span", { class: "v", text: `v${a.version}` }) : null]),
        h("div", { class: "t" }, [
          h("span", { class: "mono", text: s.name }),
          a && a.provider ? h("span", { text: a.provider }) : null,
          h("span", { class: "st" }, [h("span", { class: `dot ${up ? "online" : "offline"}` }), up ? "reachable" : "not reachable"]),
        ]),
      ]),
    ]),
    desc,
    skills.length ? h("div", { class: "chips skills" }, skills.map((sk) => h("span", { class: "tag", text: sk.name }))) : null,
    details,
    s.url ? h("div", { class: "url" }, [h("code", { text: s.url }), copyButton(s.url, "Copy", note)]) : null,
    h("div", { class: "foot" }, [
      s.kind === "a2a" && !s.disabled ? h("a", { class: "btn sm", href: `#/run/${encodeURIComponent(s.name)}`, text: "Send message" }) : null,
      s.kind === "a2a" && offers("cards_compare") ? h("a", { class: "btn sm ghost", href: `#/cards/${encodeURIComponent(s.name)}`, dataset: { act: "compare" }, text: "Compare cards" }) : null,
      s.kind === "a2a" && !s.disabled && offers("playground") ? h("a", { class: "btn sm ghost", href: `#/play/${encodeURIComponent(s.name)}`, dataset: { act: "play" }, text: "Playground" }) : null,
      h("span", { class: "tag", text: s.kind === "mcp" ? "mcp server" : "a2a agent" }),
      s.sealed ? h("span", { class: "tag", text: "sealed" }) : null,
      s.disabled ? h("span", { class: "tag", text: "disabled" }) : null,
      note,
      h("span", { class: "kept", title: "How long its ended tasks are kept", text: keptText(s) }),
    ]),
    h("div", { class: "expandrow" }, [arrow]),
  ]);
  arrow.addEventListener("click", () => {
    const open = arrow.getAttribute("aria-expanded") !== "true";
    arrow.setAttribute("aria-expanded", String(open));
    arrow.title = open ? "Less" : "More";
    art.classList.toggle("open", open);
    details.hidden = !open;
  });
  // The arrow only when there is more: a clamped description (measured once in the page), or a skill's details.
  const measure = () => {
    if (arrow.getAttribute("aria-expanded") === "true") return;
    arrow.hidden = !(desc.scrollHeight > desc.clientHeight + 2 || details.childElementCount > 0);
  };
  return { node: art, measure };
}

/** #/cards. */
export async function showCards() {
  showView("#view-cards", "cards");
  const root = $("#cards");
  const mine = ++generation;
  fill(root, [h("p", { class: "empty", text: "Loading…" })]);
  let got;
  try {
    got = await gate.services();
  } catch (err) {
    if (mine === generation) fill(root, [failure(err, "The gate's services")]);
    return () => {};
  }
  if (mine !== generation) return () => {};
  const items = Array.isArray(got) ? got : Array.isArray(got && got.items) ? got.items : [];
  if (!items.length) {
    fill(root, [h("p", { class: "empty", text: "No service yet: configure one, or connect a node with kontra connect." })]);
    return () => {};
  }
  const cards = items.map(card);
  fill(root, [h("div", { class: "grid", id: "cardsgrid" }, cards.map((c) => c.node))]);
  const measure = () => { for (const c of cards) c.measure(); };
  requestAnimationFrame(() => requestAnimationFrame(measure));
  window.addEventListener("resize", measure);
  return () => window.removeEventListener("resize", measure);
}

// ------------------------------------------------------------------ #/cards/<name>: the two cards (D360)

/** One of the two cards, its validation, and, on the served one, the members the gate set marked. */
function cardColumn(title, side, gateSet, id) {
  if (!side) return h("div", { class: "card", id }, [h("div", { class: "hd" }, [h("h3", { text: title })]),
    h("div", { class: "bd" }, [h("p", { class: "empty", text: "No card." })])]);
  let text;
  try {
    text = typeof side.card === "string" ? side.card : JSON.stringify(side.card, null, 2);
  } catch {
    text = String(side.card);
  }
  return h("div", { class: "card", id }, [
    h("div", { class: "hd" }, [h("h3", { text: title }), h("span", { class: "tag", text: `A2A ${side.version}` }), validationMark(side.validation)]),
    h("div", { class: "bd stack" }, [
      gateSet ? h("div", { class: "gateset", dataset: { gateset: gateSet.join(" ") } }, gateSet.length
        ? [h("p", { class: "small", text: "Set or rewritten by the gate:" }), ...gateSet.map((p) => h("code", { class: "mark", text: p }))]
        : [h("p", { class: "small", text: "The gate changed nothing." })]) : null,
      jsonBox(text),
    ]),
  ]);
}

/** #/cards/<name>. */
export async function showCardCompare(name) {
  showView("#view-cards", "cards", [name || "?"]);
  const root = $("#cards");
  const mine = ++generation;
  fill(root, [h("p", { class: "empty", text: "Loading…" })]);
  let got;
  try {
    got = await gate.serviceCards(name);
  } catch (err) {
    if (mine === generation) fill(root, [failure(err, "This service's cards")]);
    return () => {};
  }
  if (mine !== generation) return () => {};
  fill(root, [h("div", { class: "compare", id: "cardcompare" }, [
    cardColumn("The callee's card, as fetched", got.callee, null, "calleecard"),
    cardColumn("The gate's served card", got.served, Array.isArray(got.gate_set) ? got.gate_set : [], "servedcard"),
  ])]);
  return () => {};
}
