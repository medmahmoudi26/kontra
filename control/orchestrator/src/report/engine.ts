/**
 * THE REPORT ENGINE: one Liquid instance, configured for a template nobody trusts and data nobody
 * trusts either.
 *
 * ── THE THREAT MODEL, STATED ONCE ──────────────────────────────────────────────────────────────
 *
 * Two different inputs with two different trust levels meet here.
 *
 *   • The TEMPLATE comes from a workflow folder in a workspace. It is written by somebody on the
 *     team, so it is not hostile — but it is not reviewed either, and it runs on the orchestrator,
 *     the process holding the object-store credentials and the Temporal client. A template must
 *     therefore not be able to read a file, reach the network, call into the process, or spend
 *     unbounded CPU. That is what Liquid buys over Jinja, Handlebars or MDX, and the configuration
 *     below is what makes the purchase real.
 *   • The DATA is a workflow's return value, which in this product came off a scanned target. It is
 *     hostile by default. `escape.ts` handles it.
 *
 * ── EVERY OPTION HERE WAS MEASURED ON THE PINNED VERSION ───────────────────────────────────────
 *
 * The findings are in `.scratch/phase7/liquid-measurements.md` and ADR 0055; the ones that changed
 * the code:
 *
 *   • `fs: undefined`, which the spec prescribes verbatim, THROWS in the constructor on 10.30.0 —
 *     `normalize()` reads `options.fs.dirname` before it consults `relativeReference`. The
 *     filesystem is closed with a stub that refuses instead.
 *   • `renderLimit` is checked periodically, not continuously: a 10^8-iteration loop under a
 *     2,000 ms limit ran for 5,022 ms. It bounds a render; it does not bound it tightly, which is
 *     why {@link renderReport} is called from a worker thread and never from a request handler.
 *   • `memoryLimit` counts cumulative characters allocated, not bytes retained, so a string-building
 *     loop is quadratic in its iteration count while a 20,000-row table is flat.
 *   • `jsTruthy: false` is Liquid truthiness: `''` and `0` are TRUTHY. `{% if result.sample_request %}`
 *     takes the true branch on an empty string, which the §2.4 contract does not mention and authors
 *     will meet.
 *
 * ── THE FILTER SET IS AN ALLOWLIST, AND ITS COMPLETENESS IS A TEST ─────────────────────────────
 *
 * 10.30.0 ships 88 filters. Every one is either allowed or named in {@link DENIED_FILTERS} with a
 * reason, and `engine.test.ts` asserts that the two sets together cover exactly what the library
 * registers. Upgrading liquidjs therefore FAILS A TEST rather than silently granting a template a
 * new capability — which is the only way an allowlist stays one.
 */

import { Liquid } from 'liquidjs';

import { CodeBlockSink, registerCodeTag, type CodeBlockRecord, type CodeTagDeps } from './codeTag';
import { markdownEscape } from './escape';

/**
 * Filters a report template may use: string, number, date and array basics, per spec §4.1.
 *
 * Everything here is deterministic, local, and incapable of printing a composite — the three
 * properties that let a re-render reproduce a snapshot byte for byte.
 */
export const ALLOWED_FILTERS: ReadonlySet<string> = new Set([
  // strings
  'append', 'capitalize', 'default', 'downcase', 'lstrip', 'normalize_whitespace', 'number_of_words',
  'prepend', 'remove', 'remove_first', 'remove_last', 'replace', 'replace_first', 'replace_last',
  'rstrip', 'size', 'slice', 'split', 'squish', 'strip', 'strip_newlines', 'truncate',
  'truncatewords', 'upcase',
  // numbers
  'abs', 'at_least', 'at_most', 'ceil', 'divided_by', 'floor', 'minus', 'modulo', 'plus', 'round',
  'times', 'to_integer',
  // dates
  'date', 'date_to_long_string', 'date_to_rfc822', 'date_to_string', 'date_to_xmlschema',
  // arrays
  'array_to_sentence_string', 'compact', 'concat', 'find', 'find_exp', 'find_index',
  'find_index_exp', 'first', 'group_by', 'group_by_exp', 'has', 'has_exp', 'join', 'last', 'map',
  'reject', 'reject_exp', 'reverse', 'sort', 'sort_natural', 'sum', 'uniq', 'where', 'where_exp',
]);

/**
 * Filters a report template may NOT use, each with the reason it is refused.
 *
 * The reason is not decoration: it is what a reviewer reads instead of re-deriving the judgement,
 * and what a future engineer needs before moving one of these to the allowlist.
 */
export const DENIED_FILTERS: Readonly<Record<string, string>> = {
  raw: 'bypasses the Markdown escape, which is the one defence between a target\'s bytes and the document',
  sample:
    'is random: measured at 10 distinct outputs over 40 renders of the same input, so a report could not be re-rendered to the same bytes and no version would be reproducible',
  json: 'prints a whole composite; pick a field or loop over it',
  jsonify: 'prints a whole composite; pick a field or loop over it',
  inspect: 'prints a whole composite, including shapes the context contract does not promise',
  base64_decode: 'decodes bytes an author has not looked at into a document somebody reads',
  base64_encode: 'is an encoding, not a presentation; {% code %} carries bytes',
  sha256: 'is a cryptographic primitive, not a presentation concern',
  hmac_sha256: 'is a cryptographic primitive, and takes a key',
  escape: 'HTML-escapes, which then gets Markdown-escaped again: two encodings, one value',
  escape_once: 'same, and its idempotence is about HTML entities, not Markdown',
  xml_escape: 'encodes for XML, a syntax this pipeline never emits, and then gets Markdown-escaped on top',
  newline_to_br: 'emits HTML, which the pipeline drops after parsing',
  strip_html: 'implies HTML passes through here; it does not',
  url_encode: 'changes what the bytes say; a URL in a report is evidence',
  url_decode: 'same, in the direction that can surface a value the author did not know was there',
  cgi_escape: 'percent-encodes, changing what the bytes say; a URL in a report is evidence and not a link',
  uri_escape: 'percent-encodes, with the same objection: a value is shown as it was, not as it would be sent',
  slugify: 'rewrites a value to be a URL fragment, which no report field is',
  push: 'mutates the array it is given, so two renders of one input can differ and no version is reproducible',
  unshift: 'mutates the array it is given, with the same consequence for reproducibility as push',
  pop: 'mutates the array it is given, and removing a row while presenting it is never what a report means',
  shift: 'mutates the array it is given, and a presentation layer has no business consuming its input',
};

/** The filesystem a report template gets: none, with a refusal rather than a crash. */
function closedFilesystem(): never[] | Record<string, unknown> {
  const refuse = () => {
    throw new Error('a report template is one file: it cannot read from the filesystem');
  };
  return {
    readFileSync: refuse,
    readFile: refuse,
    existsSync: () => false,
    exists: async () => false,
    resolve: refuse,
    dirname: refuse,
    sep: '/',
    contains: () => false,
    fallback: () => undefined,
  };
}

/** Characters of template. §4.1's value; `parseLimit` counts characters, measured. */
export const DEFAULT_PARSE_LIMIT = 100_000;
/**
 * Milliseconds per render, enforced loosely by the library and tightly by the worker's deadline.
 *
 * 10,000 AND NOT THE SPEC'S 2,000, because 2,000 fails legitimate reports. Measured on this box with
 * the real `markdownEscape` in the loop, a GFM table of realistic rows:
 *
 *     1,000 rows   452 ms      20,000 rows  1,053 ms
 *     5,000 rows   473 ms      50,000 rows  2,199 ms
 *
 * A 20,000-row table therefore has barely 2x headroom under 2,000 ms, and it FAILED outright when
 * run alongside other tests on a loaded box — which is the normal condition of a materializer, not
 * an unusual one. A limit that a real report trips is not a safety control, it is an outage.
 *
 * The limit's job is to stop a pathological template, and 10,000 ms does that while leaving room for
 * a large honest one. Report SIZE is policed where it belongs, by the 5 MiB snapshot cap in
 * render.ts, which is a better-aimed control than a clock. The worker thread's own deadline
 * (`renderLimitMs` x 1.5) is the backstop for the library's periodic checking, measured overshooting
 * its budget by 2.5x on a tight loop.
 */
export const DEFAULT_RENDER_LIMIT_MS = 10_000;
/** Cumulative characters allocated. Generous for tables, tight for `append` loops. Measured. */
export const DEFAULT_MEMORY_LIMIT = 10_000_000;

export interface EngineLimits {
  parseLimit?: number;
  renderLimitMs?: number;
  memoryLimit?: number;
  blockLimitBytes?: number;
}

export interface EngineDeps {
  resolveRef: CodeTagDeps['resolveRef'];
  /** Byte-preserving redaction for `{% code %}`; see codeTag.ts on why latin-1. */
  redactLatin1: CodeTagDeps['redactLatin1'];
  /** The `| redact` filter's rules, over ordinary text. */
  redactText(text: string): string;
  limits?: EngineLimits;
}

export interface RenderedTemplate {
  /** Liquid's output: Markdown, with escaped values and fenced blocks. Not yet parsed. */
  markdown: string;
  /** Every `{% code %}` block this render produced, in render order. */
  blocks: readonly CodeBlockRecord[];
}

/**
 * Render one template against one context.
 *
 * A FRESH LIQUID INSTANCE PER RENDER, deliberately. The block sink is per-render state, and sharing
 * an instance across renders would mean one run's blocks could be numbered after another run's — but
 * the stronger reason is that this process renders data from different scanned targets minutes apart,
 * and a renderer with no state between calls has no way to leak one into the next. Construction cost
 * is a few hundred microseconds; parsing dominates either way.
 *
 * ASYNC ONLY. `{% code %}` yields a promise to resolve a ref, and under `parseAndRenderSync` a
 * yielded promise reaches the tag unresolved — producing either `[object Promise]` in the document
 * or a thrown RenderError, both measured. There is no sync entry point here and `engine.test.ts`
 * asserts that adding one breaks.
 */
export async function renderReport(
  template: string,
  context: Record<string, unknown>,
  deps: EngineDeps
): Promise<RenderedTemplate> {
  const limits = deps.limits ?? {};
  const sink = new CodeBlockSink();
  const liquid = new Liquid({
    strictVariables: true,
    strictFilters: true,
    // `{% if result %}` must work when `result` is null, which is every run that did not complete.
    lenientIf: true,
    // The default, set explicitly: it is what turns `{{ result.constructor }}` into an error.
    ownPropertyOnly: true,
    outputEscape: markdownEscape as never,
    parseLimit: limits.parseLimit ?? DEFAULT_PARSE_LIMIT,
    renderLimit: limits.renderLimitMs ?? DEFAULT_RENDER_LIMIT_MS,
    memoryLimit: limits.memoryLimit ?? DEFAULT_MEMORY_LIMIT,
    // UTC unless a template's own `date` filter argument says otherwise. Honoured behaviourally on
    // 10.30.0 even though it is absent from the normalized options map.
    timezoneOffset: 0,
    fs: closedFilesystem() as never,
    root: [],
    relativeReference: false,
    jsTruthy: false,
  });

  applyFilterPolicy(liquid, deps.redactText);
  closeTemplateInclusion(liquid);
  reserveImageTag(liquid);
  registerCodeTag(liquid, {
    resolveRef: deps.resolveRef,
    redactLatin1: deps.redactLatin1,
    sink,
    ...(limits.blockLimitBytes !== undefined ? { limitBytes: limits.blockLimitBytes } : {}),
  });

  const markdown = await liquid.parseAndRender(template, context);
  return { markdown, blocks: sink.all() };
}

/**
 * Delete every filter off the allowlist, then add `| redact`.
 *
 * DELETION, NOT OVERRIDE. `liquid.filters` is a bare registry keyed by name, and a deleted name
 * becomes `ParseError: undefined filter: <name>` under `strictFilters` — at PARSE time, which means
 * `serve` lint and `report preview` both catch it before anything renders. An override that throws
 * would only fire at render, after the author had already waited for a run.
 */
function applyFilterPolicy(liquid: Liquid, redactText: (text: string) => string): void {
  const registry = liquid.filters as unknown as Record<string, unknown>;
  for (const name of Object.keys(registry)) {
    if (!ALLOWED_FILTERS.has(name)) delete registry[name];
  }
  /**
   * `| redact` applies the shared redaction rules to a plain value, for an author who knows a field
   * may carry a credential. §5. Export always uses the redacted form, so this is for the case where
   * a value is sensitive and the block form does not apply.
   */
  liquid.registerFilter('redact', (value: unknown) =>
    typeof value === 'string' ? redactText(value) : value
  );
}

/**
 * `{% include %}`, `{% render %}` and `{% layout %}` are overridden to refuse in words.
 *
 * The closed filesystem already stops them — they fail with `ENOENT: Failed to lookup "/etc/passwd"
 * in ""` — but that message describes a filesystem that was consulted, which is both alarming and
 * untrue. Acceptance test 4 asks for a clean error, so these say what is actually the case.
 */
function closeTemplateInclusion(liquid: Liquid): void {
  for (const name of ['include', 'render', 'layout'] as const) {
    liquid.registerTag(name, {
      parse() {},
      *render() {
        throw new Error(
          `{% ${name} %} is not available in a report template: a template is one file, and the renderer has no filesystem`
        );
      },
    });
  }
}

/** `{% image %}` is reserved by spec §11: the name is taken now so a future release can define it. */
function reserveImageTag(liquid: Liquid): void {
  liquid.registerTag('image', {
    parse() {},
    *render() {
      throw new Error(
        '{% image %} is reserved for a future release and is not supported yet: use {% code %} for bytes'
      );
    },
  });
}
