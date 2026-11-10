// An AP2 part on the thread (js/ap2.js): a labelled card -- a checkout with its items and total, a mandate or a receipt
// with what its token says -- and the part's whole JSON one click away. Nothing on it is verified: the card says so.

import { h } from "../dom.js";
import { jsonBox, rawToggle } from "./cards.js";

const row = (k, v) => (v === null || v === undefined || v === "" ? null : h("div", { class: "tr" }, [h("b", { text: k }), h("span", { text: String(v) })]));

function checkoutNodes(s) {
  const nodes = [];
  if (s.items.length) {
    nodes.push(h("div", { class: "tablewrap" }, [h("table", { dataset: { ap2: "items" } }, [
      h("thead", {}, [h("tr", {}, ["Item", "Qty", "Price", "Total"].map((x) => h("th", { text: x })))]),
      h("tbody", {}, s.items.map((i) => h("tr", {}, [
        h("td", { text: i.title }), h("td", { class: "m", text: String(i.quantity) }),
        h("td", { class: "m", text: i.price || "—" }), h("td", { class: "m", text: i.total || "—" }),
      ]))),
    ])]));
  }
  nodes.push(h("div", { class: "terms2 kv" }, [
    row("Total", s.total), row("Merchant", s.merchant), row("Status", s.status), row("Checkout", s.id), row("Expires", s.expires),
  ].filter(Boolean)));
  return nodes;
}

function tokenNodes(s) {
  return [h("div", { class: "terms2 kv" }, [
    row("Status", s.status), row("Issuer", s.issuer), row("Type", s.vct), row("Checkout", s.checkout), row("Issued", s.issued),
    row("Expires", s.expires), row("Disclosures", s.disclosures || null), row("Size", `${s.size} characters`),
  ].filter(Boolean))];
}

/** The card of an AP2 part: `ap2` from ap2Of, `data` the part's JSON text. */
export function ap2Card(ap2, data) {
  const s = ap2.summary;
  const body = [];
  if (ap2.kind === "checkout" && s) body.push(...checkoutNodes(s));
  else if (s) body.push(...tokenNodes(s));
  else body.push(jsonBox(data));
  const { button, box } = rawToggle(`The ${ap2.key} part as sent`, () => {
    try {
      return JSON.parse(data);
    } catch {
      return data;
    }
  });
  button.textContent = "Raw JSON";
  const signedNote = s && s.signed ? "Signed by the agent; the console does not check signatures." : null;
  return h("div", { class: "card ap2", dataset: { ap2: ap2.kind } }, [
    h("div", { class: "hd" }, [h("span", { class: "tag", text: "AP2" }), h("b", { text: ap2.title }), h("span", { class: "muted mono small", text: ` ${ap2.key}` })]),
    h("div", { class: "bd stack" }, [...body, signedNote ? h("p", { class: "small muted", text: signedNote }) : null, button, box]),
  ]);
}
