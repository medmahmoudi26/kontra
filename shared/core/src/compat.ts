/**
 * THE CROSS-VERSION CHECK — what registering `probe@0.2.0` costs everyone still calling `0.1.0`.
 *
 * WHAT WENT WRONG WITHOUT IT. `catalog.ts` refuses a schema change under an UNCHANGED version
 * (ADR 0004). Nothing looked at the version BEFORE, so `probe@0.2.0` could drop a field that
 * `probe@0.1.0` emitted, register with a 200, and every caller reading that field found out later,
 * inside a run, as data that did not fit — with no event anywhere recording that the shape had
 * moved. That was the BACKWARD/FORWARD half of the schema registry ADR 0008 named and never ran;
 * ADR 0027 removed the registry and wrote the gap down (§4), and this closes it in-process, where
 * the schemas already are.
 *
 * IT REPORTS. IT NEVER REFUSES — the opposite of its sibling in `catalog.ts`, deliberately. The
 * immutability gate refuses because a schema change under a fixed version is never legitimate. A
 * breaking change in a NEW version is exactly what versions are FOR, and a gate that refused it
 * would make a deliberate major break unshippable, which is how a gate acquires an escape hatch
 * and then gets ignored. The requirement is only that nobody finds out by having it break: the
 * finding is named at registration, stored with the descriptor and drawn on the Actors page beside
 * the version that introduced it. Nothing on this path returns a non-2xx.
 *
 * THE DIRECTION RULE IS ADR 0008'S, and it is not symmetric:
 *
 *  - INPUT is BACKWARD. A new input schema must still read data shaped for the old one, so existing
 *    producers and saved pipelines keep working. A new REQUIRED field breaks that; an added
 *    OPTIONAL one does not.
 *  - OUTPUT is FORWARD. An old consumer must still read a new producer's output. A REMOVED field
 *    breaks that; an added one does not.
 *
 * AND IT IS HONEST ABOUT WHAT IT IS. This compares two JSON Schema documents STRUCTURALLY — the
 * top-level `properties`, `required`, and each property's declared `type` — which is not a proof of
 * substitutability and cannot be turned into one by reading further: a narrowed `maximum`, an enum
 * with a member deleted, a field whose meaning changed while its name and type did not, are all
 * clean here. So a document that says nothing structural (`{"type":"object"}`) compares as UNKNOWN,
 * never as compatible, and the same goes for a Method that is new in this version and for the first
 * version of an actor. "Checked, and fine" is the one answer this must never give by accident.
 */

import type { JsonSchemaDoc } from './contract/types';
import { compareVersionsDesc } from './versions';

/**
 * The two schemas the direction rule is about.
 *
 * `params` is NOT one of them, and that is a limit rather than an oversight: ADR 0008's rule named
 * inputs and outputs, the two things that flow per unit, and a third rule invented here would be
 * this file deciding a compatibility policy instead of implementing one. A new required `param` is
 * therefore not reported — written down in ADR 0027's consequences with the rest of what this does
 * not prove, so it is a known hole and not a silent one.
 */
const SIGNATURES = ['input', 'output'] as const;
export type Signature = (typeof SIGNATURES)[number];

/** ADR 0008's words for the two directions, kept verbatim because the report is read by people who
 *  will go looking for them. */
export type Rule = 'BACKWARD' | 'FORWARD';

/** Which rule each signature is held to. One map, so the server's sentence and the card's badge
 *  cannot disagree about which direction an `input` finding failed. */
export const RULE: Record<Signature, Rule> = { input: 'BACKWARD', output: 'FORWARD' };

/**
 * One way a version's schema moved in a direction that can break somebody.
 *
 * SELF-CONTAINED ON PURPOSE: this is stored on the actor row and drawn on a card months later, so
 * it names the Method, the schema, the rule and the version it was compared against rather than
 * leaving any of them to be recomputed by a reader who no longer has the catalog of that day.
 * `rule` is carried even though it is derivable from `field` today — a report is a frozen
 * statement, and the day `params` joins the check under BACKWARD the mapping stops being one-to-one
 * while the already-stored findings must still read correctly.
 */
export interface Incompatibility {
  method: string;
  field: Signature;
  rule: Rule;
  /** The version compared against — the one immediately preceding this one AT REGISTRATION. */
  previous: string;
  /** What moved, in one sentence: which field, and why that breaks somebody. */
  detail: string;
}

/** One (Method, schema) pair considered. `unknown` is a real answer and is not `compatible`. */
export interface CompatCheck {
  method: string;
  field: Signature;
  rule: Rule;
  verdict: 'compatible' | 'incompatible' | 'unknown';
  detail: string;
}

/** Everything one registration concluded about the version before it. */
export interface CompatReport {
  /** The version compared against. UNDEFINED means there was none — a first version has nothing to
   *  compare against, which is not a claim that it is compatible with anything. */
  previous?: string;
  /** Every pair considered, including the ones that could not be compared. */
  checks: CompatCheck[];
  /** The subset that breaks somebody — what the route stores on the row. Empty is "nothing
   *  reported", still not "compatible": read `checks` for that. */
  findings: Incompatibility[];
}

/** What comparing needs off a Method. Structural rather than `ActorOperation`, so `db/repo.ts` can
 *  import the finding type from here without the two files importing each other. */
export interface ComparableOperation {
  name: string;
  input?: JsonSchemaDoc;
  output?: JsonSchemaDoc;
}

/** What comparing needs off a catalogued row: which actor and version it is, and what it promises. */
export interface ComparableVersion {
  name: string;
  version: string;
  operations: readonly ComparableOperation[];
}

/**
 * Compare a registration against the version immediately preceding it.
 *
 * `catalogued` is the whole catalog — the same-name rows are picked out here, because "which
 * version comes before this one" is a question about the catalog and not about the caller's memory
 * of it. The comparison is against ONE version: `0.3.0` is checked against `0.2.0` and never
 * against `0.1.0`, so a field dropped in 0.2.0 is reported once, on 0.2.0, rather than again on
 * every release after it.
 */
export function compareWithPreceding(
  next: ComparableVersion,
  catalogued: readonly ComparableVersion[]
): CompatReport {
  const previous = precedingVersion(next, catalogued);
  // NOTHING TO COMPARE, and the report says so by having nothing in it. A first version cannot be
  // incompatible with anything; emitting a `compatible` check here would be the system claiming to
  // have checked a promise no earlier version ever made.
  if (!previous) return { checks: [], findings: [] };

  const checks: CompatCheck[] = [];
  for (const after of next.operations) {
    const before = previous.operations.find((o) => o.name === after.name);
    for (const field of SIGNATURES) {
      const judged = before
        ? judge(field, before[field], after[field])
        : {
            // A Method that did not exist in the preceding version breaks no existing caller of it,
            // and there is no earlier schema to hold this one to. Adding Methods is the ordinary
            // way an actor grows (the same reason `refuseSchemaChange` allows a new Method).
            verdict: 'unknown' as const,
            detail: `Method "${after.name}" is new in this version — ${previous.version} does not declare it`,
          };
      checks.push({ method: after.name, field, rule: RULE[field], ...judged });
    }
  }

  // A Method the preceding version had and this one DROPS is not reported here. It is a real break
  // — a dispatch to a name that no longer resolves — and it does not fit this per-signature shape;
  // it is named as an open limit in ADR 0027's consequences rather than half-answered here.
  const findings = checks
    .filter((c) => c.verdict === 'incompatible')
    .map(({ method, field, rule, detail }) => ({
      method,
      field,
      rule,
      previous: previous.version,
      detail,
    }));
  return { previous: previous.version, checks, findings };
}

/**
 * The newest catalogued version of the same actor that is strictly OLDER than this one.
 *
 * Strictly older, by the palette's ordering (`./versions`), which is why that ordering had to be
 * shared rather than reimplemented: `0.10.0` follows `0.9.0`, so a lexical comparison here would
 * compare a new build against a version four releases back and report a diff nobody made in it.
 * A re-registration of an already-catalogued version compares 0 and is therefore not its own
 * predecessor.
 */
export function precedingVersion<T extends { name: string; version: string }>(
  next: { name: string; version: string },
  catalogued: readonly T[]
): T | undefined {
  return catalogued
    .filter((a) => a.name === next.name && compareVersionsDesc(a.version, next.version) > 0)
    .sort((a, b) => compareVersionsDesc(a.version, b.version))[0];
}

/** One schema's top level, as much of it as a structural comparison can honestly read. */
interface Shape {
  properties: Record<string, unknown>;
  required: Set<string>;
}

/** Compare one Method's `input` or `output` across two versions. */
function judge(
  field: Signature,
  before: JsonSchemaDoc | undefined,
  after: JsonSchemaDoc | undefined
): { verdict: CompatCheck['verdict']; detail: string } {
  if (before === undefined || after === undefined) {
    // "declares nothing" and "declares the empty schema" are different promises (`canonical` in
    // catalog.ts turns on the same distinction), and a promise nobody made cannot be broken.
    const which =
      before === undefined && after === undefined
        ? 'neither version declares'
        : before === undefined
          ? 'the preceding version declares no'
          : 'this version declares no';
    return { verdict: 'unknown', detail: `${which} ${field} schema — nothing to compare` };
  }

  const was = shapeOf(before);
  const now = shapeOf(after);
  if (!was || !now) {
    // `{"type":"object"}` is a legal schema that says nothing about fields. Comparing it to one
    // that lists ten of them would read every field as added or removed, which is a diff of the
    // documents and not a statement about compatibility.
    const which = !was && !now ? 'neither' : !was ? 'the preceding' : 'this';
    return {
      verdict: 'unknown',
      detail: `${which} ${field} schema declares no properties — a document that says nothing structural cannot be compared to one that does`,
    };
  }

  const said = field === 'input' ? backward(was, now) : forward(was, now);
  if (said.length > 0) return { verdict: 'incompatible', detail: said.join('; ') };
  return {
    verdict: 'compatible',
    detail:
      field === 'input'
        ? 'every field the preceding input schema accepted is still accepted'
        : 'every field the preceding output schema promised is still promised',
  };
}

/**
 * BACKWARD (input): can this schema still read data shaped for the old one?
 *
 * A property the new schema DROPS is not a finding: JSON Schema accepts unlisted properties unless
 * a document forbids them, so an old caller that keeps sending it is still accepted. What breaks is
 * a demand the old data cannot meet.
 */
function backward(was: Shape, now: Shape): string[] {
  const said: string[] = [];
  for (const name of now.required) {
    if (!was.required.has(name)) {
      said.push(`required field "${name}" added — data shaped for the older schema does not carry it`);
    }
  }
  for (const [name, prop] of Object.entries(now.properties)) {
    const older = was.properties[name];
    if (older === undefined) continue; // an ADDED optional field — the case this must not report
    const lost = missing(typesOf(older), typesOf(prop));
    if (lost.length > 0) {
      said.push(`field "${name}" no longer accepts ${lost.join(', ')} — the older schema allowed it`);
    }
  }
  return said;
}

/**
 * FORWARD (output): can a consumer written against the old schema still read what this one emits?
 *
 * A property this schema ADDS is not a finding: an old consumer reads the fields it knows and
 * ignores the rest.
 */
function forward(was: Shape, now: Shape): string[] {
  const said: string[] = [];
  for (const [name, older] of Object.entries(was.properties)) {
    const prop = now.properties[name];
    if (prop === undefined) {
      said.push(`field "${name}" removed — a caller reading it gets nothing`);
      continue;
    }
    if (was.required.has(name) && !now.required.has(name)) {
      said.push(
        `field "${name}" is no longer guaranteed — a caller that reads it unconditionally breaks on the unit that omits it`
      );
    }
    const added = missing(typesOf(prop), typesOf(older));
    if (added.length > 0) {
      said.push(`field "${name}" can now be ${added.join(', ')} — the older schema did not promise that`);
    }
  }
  return said;
}

/**
 * A schema's top level, or `null` when it says nothing structural.
 *
 * `properties` is the only thing read, which is the same shallow reading the Actors page's field
 * table does (`schemaFields`) — deliberately: what is reported here is what a reader can see on the
 * card that carries the report. No `properties` is not an empty object; it is "this document does
 * not describe fields", and that has to stay distinguishable.
 */
function shapeOf(doc: JsonSchemaDoc | undefined): Shape | null {
  if (!doc || typeof doc !== 'object') return null;
  const props = (doc as { properties?: unknown }).properties;
  if (!props || typeof props !== 'object' || Array.isArray(props)) return null;
  const req = (doc as { required?: unknown }).required;
  const required = new Set(
    Array.isArray(req) ? (req.filter((r) => typeof r === 'string') as string[]) : []
  );
  return { properties: props as Record<string, unknown>, required };
}

/**
 * The type names one property allows, or `null` for a property that does not say.
 *
 * `anyOf` is flattened because that is how a derived optional ARRIVES: pydantic emits
 * `Optional[str]` as `{"anyOf":[{"type":"string"},{"type":"null"}]}`, so a check that only read
 * `type` would see every optional field as unconstrained and never notice one being made
 * non-nullable. Anything else — `$ref`, `allOf`, a bare `{}` — is unknown, and unknown is not a
 * finding: a schema this cannot read must not be reported as a break.
 */
function typesOf(prop: unknown): Set<string> | null {
  if (!prop || typeof prop !== 'object') return null;
  const t = (prop as { type?: unknown }).type;
  if (typeof t === 'string') return new Set([t]);
  if (Array.isArray(t) && t.every((x) => typeof x === 'string')) return new Set(t as string[]);
  const any = (prop as { anyOf?: unknown }).anyOf;
  if (Array.isArray(any)) {
    const out = new Set<string>();
    for (const member of any) {
      const members = typesOf(member);
      if (!members) return null; // one unreadable branch makes the whole union unreadable
      for (const name of members) out.add(name);
    }
    return out.size > 0 ? out : null;
  }
  return null;
}

/** Type names `had` lists that `holds` does not. Empty when either side declares none — an
 *  unconstrained property is unknown, and unknown is never reported as a break. */
function missing(had: Set<string> | null, holds: Set<string> | null): string[] {
  if (!had || !holds) return [];
  return [...had].filter((t) => !holds.has(t)).map((t) => `"${t}"`);
}
