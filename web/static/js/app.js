// State shared by the views: the console API client, what the backend offers (GET /v1/info), and
// a tiny event bus that tells views what changed. There is no session: the local development
// profile has no login, and the console holds no credential.

import { Gate } from "./api.js";

/** The gate that served this page: same origin, no credential. */
export const gate = new Gate({ base: "" });

/** The host's names for the two parties of a call (spec: A2A Labels On Every Item). The local gate shows each as it
 * is stored, the principal (`anonymous`, `key:kk_…`, `node:…`) and the service's name; a host embedding this console
 * (Kontra Cloud) may replace `displayName` to show its own handle or key label: still the party's name, never a role. */
export const host = {
  /** kind: "principal" | "service"; id: the gate's string for it. */
  displayName: (kind, id) => id,
};

/** { principal, service } in the host's display form, for taskevents.js's wireOf and wireOfExchange. */
export function partiesOf(principal, service) {
  const name = (kind, id) => (typeof id === "string" && id ? String(host.displayName(kind, id) || id) : null);
  return { principal: name("principal", principal), service: name("service", service) };
}

export const state = {
  /** GET /v1/info once read: { profile, features, retention, ... }; null before, or when it failed. */
  info: null,
};

const listeners = new Map();

export function on(event, fn) {
  if (!listeners.has(event)) listeners.set(event, new Set());
  listeners.get(event).add(fn);
  return () => listeners.get(event).delete(fn);
}

export function emit(event, data) {
  for (const fn of listeners.get(event) || []) {
    try {
      fn(data);
    } catch (err) {
      console.error(err);
    }
  }
}

/** Read what the backend offers, once; a failure leaves `state.info` null (every view shows). */
export async function loadInfo() {
  try {
    state.info = await gate.info();
  } catch {
    state.info = null;
  }
  emit("info", state.info);
  return state.info;
}

/** Whether the backend offers `feature`: true unless /v1/info lists its features without it. */
export function offers(feature) {
  const f = state.info && Array.isArray(state.info.features) ? state.info.features : null;
  return f === null || f.includes(feature);
}
