// AP2 v0.2.0 data parts (the Agent Payments Protocol, https://github.com/google-agentic-commerce/ap2): what a part
// carries, read for display only. The gate relays AP2 untouched (D351) and so does the console: nothing here checks a
// signature, a hash or an expiry -- the agents do. A JWT's claims are decoded to show what it says, and the card says
// they are not verified. The keys are the AP2 samples' (code/samples/python/src/common/constants.py), as
// examples/sellers/ap2-merchant emits them.

/** AP2's A2A extension. */
export const AP2_EXTENSION = "https://github.com/google-agentic-commerce/ap2/v1";

/** The data-part keys the console names, and what each holds. */
export const AP2_KEYS = Object.freeze({
  "ap2.checkout": "checkout",
  "ap2.mandates.CheckoutMandateSdJwt": "checkout_mandate",
  "ap2.mandates.PaymentMandateSdJwt": "payment_mandate",
  "ap2.CheckoutReceipt": "checkout_receipt",
  "ap2.PaymentReceipt": "payment_receipt",
  "ap2.mandates.IntentMandate": "intent_mandate",
  "ap2.mandates.CartMandate": "cart_mandate",
  "ap2.mandates.PaymentMandate": "payment_mandate",
});

/** What each kind is called on its card. */
export const AP2_TITLES = Object.freeze({
  checkout: "Checkout",
  checkout_mandate: "Checkout mandate",
  payment_mandate: "Payment mandate",
  intent_mandate: "Intent mandate",
  cart_mandate: "Cart mandate",
  checkout_receipt: "Checkout receipt",
  payment_receipt: "Payment receipt",
  unknown: "AP2 data",
});

const isObj = (v) => v !== null && typeof v === "object" && !Array.isArray(v);
const JWT_MAX = 64 * 1024;

/** The claims of a JWT's payload segment (or an SD-JWT's issuer JWT, before its first "~"), or null: decoded, never
 * verified. */
export function jwtClaims(token) {
  if (typeof token !== "string" || token.length > JWT_MAX) return null;
  const jwt = token.split("~")[0];
  const seg = jwt.split(".");
  if (seg.length !== 3 || !/^[A-Za-z0-9_-]+$/.test(seg[1])) return null;
  try {
    const b64 = seg[1].replace(/-/g, "+").replace(/_/g, "/");
    const bin = atob(b64 + "=".repeat((4 - (b64.length % 4)) % 4));
    const bytes = Uint8Array.from(bin, (c) => c.charCodeAt(0));
    const claims = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(bytes));
    return isObj(claims) ? claims : null;
  } catch {
    return null;
  }
}

/** The disclosures of an SD-JWT (the segments between "~"), each [salt, name, value] or [salt, value], decoded. */
export function disclosures(token) {
  if (typeof token !== "string" || token.length > JWT_MAX || !token.includes("~")) return [];
  const out = [];
  for (const d of token.split("~").slice(1, 33)) {
    if (!d || !/^[A-Za-z0-9_-]+$/.test(d)) continue;
    try {
      const b64 = d.replace(/-/g, "+").replace(/_/g, "/");
      const v = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(Uint8Array.from(atob(b64 + "=".repeat((4 - (b64.length % 4)) % 4)), (c) => c.charCodeAt(0))));
      if (Array.isArray(v)) out.push(v);
    } catch {
      // not a disclosure
    }
  }
  return out;
}

/** An amount in minor units as money ("10.00 USD"), or null. */
export function money(minor, currency) {
  if (typeof minor !== "number" || !Number.isFinite(minor)) return null;
  const cur = typeof currency === "string" && /^[A-Z]{3}$/.test(currency) ? currency : "";
  return `${(minor / 100).toFixed(2)}${cur ? ` ${cur}` : ""}`;
}

const str = (v, n = 200) => (typeof v === "string" && v ? v.slice(0, n) : null);

/** A checkout's lines and totals from its claims (the AP2 checkout object) or, failing that, the part's own summary
 * (item_label, amount_cents, currency). */
function checkoutOf(value) {
  const claims = isObj(value) ? jwtClaims(value.checkout_jwt) : null;
  const currency = str((claims && claims.currency) || (isObj(value) && value.currency), 8);
  const items = [];
  if (claims && Array.isArray(claims.line_items)) {
    for (const li of claims.line_items.slice(0, 50)) {
      if (!isObj(li)) continue;
      const item = isObj(li.item) ? li.item : {};
      const qty = typeof li.quantity === "number" ? li.quantity : 1;
      const lineTotal = Array.isArray(li.totals) ? li.totals.find((t) => isObj(t) && t.type === "total") : null;
      items.push({
        title: str(item.title) || str(item.id) || str(li.id) || "item",
        quantity: qty,
        price: money(item.price, currency),
        total: money(lineTotal ? lineTotal.amount : typeof item.price === "number" ? item.price * qty : null, currency),
      });
    }
  }
  let total = null;
  if (claims && Array.isArray(claims.totals)) {
    const t = claims.totals.find((x) => isObj(x) && x.type === "total");
    if (t) total = money(t.amount, currency);
  }
  if (!items.length && isObj(value) && str(value.item_label)) {
    items.push({ title: str(value.item_label), quantity: 1, price: money(value.amount_cents, currency), total: money(value.amount_cents, currency) });
  }
  if (total === null && isObj(value)) total = money(value.amount_cents, currency);
  const merchant = claims && isObj(claims.merchant) ? str(claims.merchant.name) || str(claims.merchant.id) : null;
  return {
    id: str((claims && claims.id) || (isObj(value) && value.checkout_id)),
    merchant,
    status: str(claims && claims.status, 40),
    currency,
    items,
    total,
    expires: claims && typeof claims.exp === "number" ? new Date(claims.exp * 1000).toISOString() : null,
    signed: Boolean(claims),
  };
}

/** A receipt's or a mandate's claims in brief. */
function tokenOf(token) {
  const claims = jwtClaims(token);
  const disclosed = disclosures(token);
  const named = {};
  for (const d of disclosed) if (d.length === 3 && typeof d[1] === "string") named[d[1]] = d[2];
  const pick = (k) => (claims && claims[k] !== undefined ? claims[k] : named[k]);
  return {
    issuer: str(pick("iss")),
    vct: str(pick("vct"), 100),
    status: str(pick("status"), 40) || str(pick("result"), 40),
    checkout: str(pick("checkout_id") || pick("checkout_hash"), 120),
    issued: typeof pick("iat") === "number" ? new Date(pick("iat") * 1000).toISOString() : null,
    expires: typeof pick("exp") === "number" ? new Date(pick("exp") * 1000).toISOString() : null,
    disclosures: disclosed.length,
    signed: Boolean(claims),
    size: typeof token === "string" ? token.length : 0,
  };
}

/** The AP2 content of a data part's `data` (an object), or null when it has no AP2 key: `{kind, key, title, summary}`
 * for the first AP2 key it holds (`kind` "unknown" for a key under `ap2.` the console does not name). */
export function ap2Of(data) {
  if (!isObj(data)) return null;
  const key = Object.keys(data).find((k) => Object.hasOwn(AP2_KEYS, k)) || Object.keys(data).find((k) => k.startsWith("ap2."));
  if (!key) return null;
  const kind = AP2_KEYS[key] || "unknown";
  const value = data[key];
  let summary = null;
  if (kind === "checkout") summary = checkoutOf(value);
  else if (kind !== "unknown" && typeof value === "string") summary = tokenOf(value);
  return { kind, key, title: AP2_TITLES[kind], summary };
}
