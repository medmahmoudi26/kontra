/**
 * THE BODIES THE REPORT ROUTES ACCEPT, validated where validation belongs.
 *
 * ── WHY THIS IS NOT IN THE ROUTE ───────────────────────────────────────────────────────────────
 *
 * No route handler in this codebase runs ajv. The house shape is a `parseX(body: unknown): X` in a
 * sibling domain module with its own compiled schema and a typed refusal, and a handler that catches
 * that one type and answers 400 — `routes/catalog.ts` over `catalog.ts`. Two reasons it is better
 * here: the schema is testable without a server, and the route stays a list of decisions rather than
 * a wall of guards.
 *
 * ── THE SCHEMAS ARE SMALL BECAUSE THE SURFACE IS ───────────────────────────────────────────────
 *
 * A report takes almost nothing from a client. The template for a preview, the body of a note, and
 * one boolean. Everything else a report says comes from the Run, which is the whole premise: the
 * workflow computes, the report presents, and a client cannot inject either.
 *
 * `additionalProperties: false` throughout, so a misspelled field is a 400 rather than silence. A
 * client that sends `{templates: "..."}` and gets 200 with an empty preview would have no way to find
 * out why.
 */

import Ajv2020 from 'ajv/dist/2020';
import type { ErrorObject } from 'ajv';

/** A body this module refuses. Typed so the route maps it to 400 without parsing a message. */
export class BodyRefused extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'BodyRefused';
  }
}

const ajv = new Ajv2020({ strict: false, allErrors: true });

/** Longest template a preview may carry. Matches the engine's `parseLimit`, measured in characters. */
export const PREVIEW_TEMPLATE_MAX = 100_000;

const PREVIEW_SCHEMA = {
  type: 'object',
  additionalProperties: false,
  required: ['template'],
  properties: {
    template: { type: 'string', minLength: 1, maxLength: PREVIEW_TEMPLATE_MAX },
  },
} as const;

const RENDER_SCHEMA = {
  type: 'object',
  additionalProperties: false,
  properties: {
    /**
     * Render from today's `report.md` instead of the pinned template — §4.6's second action.
     *
     * NOT IMPLEMENTED BY THE RENDER ROUTE YET, and it answers 501 rather than silently ignoring the
     * flag: reading today's template needs a run id to folder mapping, which does not exist (ADR 0055
     * §9). A client that asks for it and gets a pinned re-render would have no way to tell.
     */
    current_template: { type: 'boolean' },
  },
} as const;

const FEEDBACK_SCHEMA = {
  type: 'object',
  additionalProperties: false,
  required: ['body'],
  properties: {
    body: { type: 'string', minLength: 1, maxLength: 16_384 },
  },
  // THE AUTHOR IS NOT A FIELD. §7.2: "the author comes from the session or token, never the body", and
  // acceptance test 19 asserts it. `additionalProperties: false` is what makes sending one a 400
  // rather than something the route has to remember to ignore.
} as const;

const validatePreview = ajv.compile(PREVIEW_SCHEMA);
const validateRender = ajv.compile(RENDER_SCHEMA);
const validateFeedback = ajv.compile(FEEDBACK_SCHEMA);

/** ajv's errors as one sentence naming the field. */
function explain(errors: ErrorObject[] | null | undefined): string {
  if (!errors || errors.length === 0) return 'the body is not valid';
  return errors
    .map((e) => {
      const where = e.instancePath ? e.instancePath.replace(/^\//, '') : 'the body';
      return `${where} ${e.message ?? 'is invalid'}`;
    })
    .join('; ');
}

export function parsePreview(body: unknown): { template: string } {
  if (!validatePreview(body)) throw new BodyRefused(explain(validatePreview.errors));
  return body as { template: string };
}

export function parseRender(body: unknown): { current_template?: boolean } {
  const given = body ?? {};
  if (!validateRender(given)) throw new BodyRefused(explain(validateRender.errors));
  return given as { current_template?: boolean };
}

export function parseFeedback(body: unknown): { body: string } {
  if (!validateFeedback(body)) throw new BodyRefused(explain(validateFeedback.errors));
  return body as { body: string };
}

/**
 * A request-rate limiter for one route.
 *
 * ── THERE IS NO PRECEDENT FOR THIS IN THE TREE, SO THE SHAPE IS BORROWED ───────────────────────
 *
 * The only limiter here is `routes/rowStream.ts`, which caps CONCURRENT streams per client with a
 * `Map<string, number>` in the registrar's closure and an injectable cap. A preview needs a cap over
 * TIME instead, because each request ends — so this is a sliding window of recent timestamps, with
 * rowStream's conventions kept: the state lives in the registrar, the cap is injectable so a test can
 * reach it in two requests, and the refusal quotes the effective cap.
 *
 * `req.ip` IS THE SOCKET ADDRESS, because `trustProxy` is off. Behind a proxy every client shares one
 * address and this degrades into a global cap. `rowStream.ts:122-126` says that out loud about itself
 * and so does this: it is a brake on accidental load, not a per-user quota, and a report preview is
 * CPU somebody else's request has to wait for.
 */
export class RateWindow {
  private readonly seen = new Map<string, number[]>();

  constructor(
    private readonly max: number,
    private readonly windowMs: number
  ) {}

  /** `null` when admitted, or the sentence to refuse with. */
  check(client: string, now: number): string | null {
    const cutoff = now - this.windowMs;
    const recent = (this.seen.get(client) ?? []).filter((t) => t > cutoff);
    if (recent.length >= this.max) {
      return `too many previews: at most ${this.max} every ${Math.round(this.windowMs / 1000)}s from one address. A preview renders a template, which is CPU every other request waits for.`;
    }
    recent.push(now);
    this.seen.set(client, recent);
    // The map is swept here rather than on a timer: a timer would need an `onClose` hook to avoid
    // holding the process open, and the only entries that can accumulate are addresses that stopped
    // asking — which this loop drops the next time anybody asks.
    if (this.seen.size > 1_000) {
      for (const [key, times] of this.seen) {
        if (times.every((t) => t <= cutoff)) this.seen.delete(key);
      }
    }
    return null;
  }
}
