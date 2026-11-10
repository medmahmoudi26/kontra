// js/ap2.js: AP2 v0.2.0 data parts classified by the keys examples/sellers/ap2-merchant emits, a checkout's items and
// total read from its JWT (decoded, never verified), mandates and receipts in brief, anything else under ap2. unknown,
// and a part with no AP2 key left to the thread's other views.

import assert from "node:assert/strict";
import test from "node:test";
import { load } from "./helpers.mjs";

const b64url = (v) => Buffer.from(typeof v === "string" ? v : JSON.stringify(v)).toString("base64url");
const jwt = (claims) => `${b64url({ alg: "ES256", typ: "JWT" })}.${b64url(claims)}.${b64url("sig")}`;

// The merchant's checkout (examples/sellers/ap2-merchant main.go checkout()).
const totals = [{ type: "subtotal", amount: 1000 }, { type: "total", amount: 1000 }];
const claims = {
  id: "c-1", merchant: { id: "example-merchant", name: "Example Merchant", website: "https://merchant.example" },
  line_items: [{ id: "li_1", item: { id: "example-item", title: "Example item", price: 1000 }, quantity: 1, totals }],
  status: "incomplete", currency: "USD", totals, iat: 1767225600, exp: 1767229200,
};
const checkout = { "ap2.checkout": { checkout_id: "c-1", checkout_jwt: jwt(claims), checkout_hash: "h", item_label: "Example item", amount_cents: 1000, currency: "USD" } };

test("a checkout shows its items and total", async () => {
  const { ap2Of } = await load("ap2.js");
  const got = ap2Of(checkout);
  assert.equal(got.kind, "checkout");
  assert.equal(got.title, "Checkout");
  assert.deepEqual(got.summary.items, [{ title: "Example item", quantity: 1, price: "10.00 USD", total: "10.00 USD" }]);
  assert.equal(got.summary.total, "10.00 USD");
  assert.equal(got.summary.merchant, "Example Merchant");
  assert.equal(got.summary.status, "incomplete");
  assert.equal(got.summary.signed, true);
  // Without a readable JWT, the part's own summary.
  const bare = ap2Of({ "ap2.checkout": { checkout_jwt: "not.a-jwt", item_label: "Thing", amount_cents: 250, currency: "EUR" } });
  assert.deepEqual(bare.summary.items, [{ title: "Thing", quantity: 1, price: "2.50 EUR", total: "2.50 EUR" }]);
  assert.equal(bare.summary.signed, false);
});

test("mandates and receipts in brief, the rest unknown", async () => {
  const { ap2Of, jwtClaims, disclosures } = await load("ap2.js");
  const disclosure = b64url(["salt", "checkout_hash", "h"]);
  const mandate = `${jwt({ iss: "shopper", vct: "mandate.checkout.1", iat: 1767225600 })}~${disclosure}~`;
  const m = ap2Of({ "ap2.mandates.CheckoutMandateSdJwt": mandate });
  assert.equal(m.kind, "checkout_mandate");
  assert.equal(m.summary.vct, "mandate.checkout.1");
  assert.equal(m.summary.checkout, "h");
  assert.equal(m.summary.disclosures, 1);
  assert.deepEqual(disclosures(mandate), [["salt", "checkout_hash", "h"]]);
  const r = ap2Of({ "ap2.CheckoutReceipt": jwt({ iss: "example-merchant", status: "Success", checkout_id: "c-1" }) });
  assert.deepEqual([r.kind, r.title, r.summary.status, r.summary.issuer], ["checkout_receipt", "Checkout receipt", "Success", "example-merchant"]);
  assert.equal(ap2Of({ "ap2.somethingNew": { x: 1 } }).kind, "unknown");
  assert.equal(ap2Of({ "ap2.somethingNew": { x: 1 } }).summary, null);
  assert.equal(ap2Of({ name: "a booking" }), null);
  assert.equal(ap2Of("ap2.checkout"), null);
  assert.equal(jwtClaims("a.%%%.c"), null);
  assert.equal(jwtClaims("x".repeat(70000)), null);
});

test("the thread reads an AP2 part as one", async () => {
  const { partsOf } = await load("taskevents.js");
  const [p] = partsOf([{ data: checkout, mediaType: "application/json" }]);
  assert.equal(p.ap2.kind, "checkout");
  assert.equal(typeof p.data, "string");
  const [plain] = partsOf([{ data: { name: "x" } }]);
  assert.equal(plain.ap2, undefined);
});
