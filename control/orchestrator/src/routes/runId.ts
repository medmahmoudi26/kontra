/**
 * THE ONE PLACE A `:runId` PATH SEGMENT BECOMES A RUN ID.
 *
 * It lives here rather than in `server.ts` for `errors.ts`'s reason, stated there in full: every
 * module in this directory imports it, `server.ts` imports every module, and a CommonJS cycle
 * between them resolves to `undefined` at exactly the wrong moment. So it is a leaf below both.
 *
 * WHAT IT DOES IS NOTHING, AND THAT IS THE FIX. Seventeen routes take a `:runId` — the count is
 * `grep -a "runIdOf(req)" routes/*.ts`, and it is worth stating because it was miscounted as
 * sixteen while the decode was scattered. Four of them used to call `decodeURIComponent` on it and
 * thirteen did not, so the two halves of a single operator action — stop a run, then read the run
 * you stopped — could land on different executions. The disagreement was real; the resolution is
 * the opposite of the obvious one, because the four were the wrong thirteen:
 *
 * FASTIFY HAS ALREADY DECODED IT. find-my-way splits the path on `/` BEFORE decoding and then
 * resolves the escapes inside each segment (`lib/url-sanitizer.js`: `safeDecodeURI` sets
 * `shouldDecodeParam` for any `%XX` naming a reserved character, and `safeDecodeURIComponent`
 * expands it). Measured over real HTTP against fastify 5.8.5 / find-my-way 9.6.0, not `inject`:
 *
 *     GET /api/runs/kontra-fleet%2Fdns   ->   req.params.runId === 'kontra-fleet/dns'
 *
 * A SECOND DECODE IS THEREFORE NOT A NO-OP, it is a corruption, and it has two failure modes:
 *
 *   - an id carrying a literal `%` — `sweep-50%-done`, sent as `sweep-50%25-done` — arrives as
 *     `sweep-50%-done`, and `decodeURIComponent` on THAT throws `URIError: URI malformed`. The
 *     route answers 500 and an operator reads "kontra is broken" about their own run's name.
 *   - an id whose text contains an escape sequence — `kontra-fleet/dns%2Fteardown` — arrives
 *     intact and is decoded down to `kontra-fleet/dns/teardown`, a different run, silently. Nothing
 *     in this repo is known to mint such an id today; it is here because it is what the first
 *     caller that composes an id out of an already-encoded string would produce, and because it is
 *     the shape that fails SILENTLY rather than loudly.
 *
 * AND NO CLIENT WANTS THE SECOND DECODE, because no client encodes twice: the browser uses
 * `encodeURIComponent` (`frontend/src/run/api.ts`) and the CLI uses Go's `url.PathEscape`, which
 * escapes `/` exactly as `encodeURIComponent` does. Both encode once; Fastify decodes once.
 *
 * SO THE NEXT ROUTE CANNOT GET THIS WRONG BY OMISSION — a handler that simply reads
 * `req.params.runId` is now correct, which is the whole point of resolving the disagreement in this
 * direction rather than the other. What this function is for is the case that IS still possible:
 * somebody reintroducing a decode. It is the named place that says why not, and
 * `runIdRoutes.test.ts` is what fails if they do it anyway.
 */

import type { FastifyRequest } from 'fastify';

/** The Run this request is about — the workflow id (ADR 0023 §12), as the caller addressed it. */
export function runIdOf(req: FastifyRequest): string {
  return (req.params as { runId: string }).runId;
}
