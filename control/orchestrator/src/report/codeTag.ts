/**
 * `{% code %}`: the one place a report shows bytes instead of prose.
 *
 * ── WHY IT IS A TAG AND NOT A FILTER ───────────────────────────────────────────────────────────
 *
 * Everything a `{{ }}` hole prints goes through `markdownEscape`, which is correct for prose and
 * fatal for evidence: a request with a `|` in it, a response with CRLF line endings, a payload with
 * a NUL — escaped, those stop being the bytes that were sent. A tag writes to the emitter directly
 * and bypasses `outputEscape` entirely, which was CONFIRMED on liquidjs 10.30.0 rather than assumed
 * (spec §4.3 asks for exactly that confirmation; `codeTag.test.ts` pins it).
 *
 * So the rule is: prose is escaped, evidence is fenced. A fence needs no escaping because its
 * content is not Markdown — provided the fence itself cannot be closed from inside, which is the
 * whole job of {@link fenceFor}.
 *
 * ── THREE VALUE FORMS, ONE OF WHICH CAN HANG ───────────────────────────────────────────────────
 *
 *     {% code "http", result.request %}
 *
 * where the VALUE the template names is one of three runtime shapes — and they are runtime shapes
 * rather than things a template can type, because Liquid has no object-literal syntax: a template
 * can only ever reference data the workflow returned, never construct a structure inline. That is a
 * property worth having, so it is stated rather than discovered.
 *
 *     "GET / HTTP/1.1"                 a string, taken as UTF-8 text
 *     {"b64": "R0VU..."}               exact bytes
 *     {"ref": "<claim check ref>"}     bytes in object storage
 *
 * The third form is the dangerous one, and the danger was measured on a live run rather than
 * imagined: a peer engineer's workflow finished its work, sealed every partition, and then sat in
 * `running` for an hour and forty minutes because dereferencing a claim-checked page dispatches
 * `kontra.fetch_blob` to an ACTOR's task queue, and the Fleet it needed was already gone. No error,
 * no timeout — the activity simply read `stalled`. A report rendered at run end is rendered after
 * every Fleet scope has exited, so that is the default case here, not the edge case.
 *
 * Two defences, and the order matters:
 *
 *   1. {@link CodeTagDeps.resolveRef} is supplied by the caller and reads OBJECT STORAGE DIRECTLY,
 *      with the orchestrator's own credentials. No activity, no task queue, no actor, so there is
 *      nothing to be absent. This makes the hang structurally impossible rather than bounded.
 *   2. It is given a deadline anyway, because (1) protects against a missing actor and not against
 *      a slow store, and because a renderer that can block a materializer is a renderer that will
 *      eventually block a materializer.
 *
 * An unresolved ref is a FIRST-CLASS OUTPUT, not an error: the block renders a marker naming what
 * was not read and why, and the version still stores as `ok`. A report that states what it could
 * not read is worth more than no report — and the inverse mistake, a block that renders empty, is
 * indistinguishable from a ref that legitimately held nothing.
 *
 * ── REDACTION HAPPENS HERE, BEFORE ANYTHING IS STORED ──────────────────────────────────────────
 *
 * `lang == "http"` always redacts; `redact: true` redacts anything. The redacted bytes are what the
 * snapshot holds, what the console shows and what both exports contain. The originals go to
 * `report_secrets`, reachable only through the audited reveal route.
 *
 * Redaction runs over a LATIN-1 VIEW of the bytes, not a UTF-8 decode. Latin-1 maps every one of the
 * 256 byte values to a code point and back, so every byte the rules do not touch survives exactly —
 * which a lossy UTF-8 decode would not do (it replaces each invalid sequence with U+FFFD and the
 * original bytes are gone). The rules themselves are ASCII patterns — header names, base64, hex —
 * so they lose nothing by not seeing multi-byte characters as characters. The VISIBLE text is a
 * separate, UTF-8 decode of the redacted bytes, so a report in another script still reads correctly.
 */

import {
  Tokenizer,
  evalToken,
  type Context,
  type Emitter,
  type Liquid,
  type TagToken,
  type Token,
} from 'liquidjs';

/** A `lang` the console will hand to a highlighter, so it is a token and not free text. */
const LANG_RE = /^[a-z0-9+-]{1,20}$/;

/** What a `lang` that fails {@link LANG_RE} becomes. Never an error: a bad lang is a cosmetic bug. */
const LANG_FALLBACK = 'text';

/** Langs that are redacted whether or not the template asked. */
const ALWAYS_REDACT = new Set(['http']);

/** Default visible bytes per block. 1 MiB, per spec §4.3; the full bytes stay downloadable. */
export const DEFAULT_BLOCK_LIMIT_BYTES = 1024 * 1024;

export function blockLimitBytes(): number {
  const raw = Number(process.env.KONTRA_REPORT_BLOCK_LIMIT_BYTES);
  return Number.isFinite(raw) && raw > 0 ? Math.floor(raw) : DEFAULT_BLOCK_LIMIT_BYTES;
}

/** Where a ref's bytes came from, or why they did not. */
export type RefResolution =
  | { bytes: Buffer; fullBytes: number }
  /** Named rather than thrown: an unreadable ref is a marker in the report, not a failed render. */
  | { unresolved: string };

export interface CodeTagDeps {
  /**
   * Fetch a claim-checked object's bytes, reading the store DIRECTLY — never through an activity
   * dispatched to an actor's queue, for the reason in this file's header.
   *
   * MUST NOT reject and MUST NOT hang: it owns its own deadline and reports a failure as
   * `{ unresolved }`. A rejection here would fail a render that should have degraded.
   */
  resolveRef(ref: string, limitBytes: number): Promise<RefResolution>;
  /** Apply the shared redaction rules to a latin-1 view. Returns the text unchanged when clean. */
  redactLatin1(text: string): string;
  /** Collects the blocks this render produced, assigning ids in render order. */
  sink: CodeBlockSink;
  limitBytes?: number;
}

/** One fenced block, as the snapshot stores it. */
export interface CodeBlockRecord {
  /** `b1`, `b2`, … in render order. Deterministic, so re-rendering the same inputs reuses them. */
  id: string;
  lang: string;
  /** Exact bytes as served and shown: AFTER redaction, AFTER truncation. */
  bytes: Buffer;
  /** The originals, present only when redaction actually changed something. Audited access only. */
  raw?: Buffer;
  redacted: boolean;
  truncated: boolean;
  /** Length before truncation, so the console can say "showing 1.0 MiB of 4.3 MiB". */
  fullBytes: number;
  source: 'text' | 'b64' | 'ref';
  ref?: string;
  /** Set when a ref was not read. The block's visible content is a marker, not bytes. */
  unresolved?: string;
}

/**
 * Collects blocks for one render.
 *
 * Ids are POSITIONAL (`b1`, `b2`, …) rather than content hashes, and that is deliberate: two blocks
 * holding identical bytes are two blocks, and a re-render of the same run must produce the same ids
 * so a stored link to `b2` keeps meaning what it meant.
 */
export class CodeBlockSink {
  private readonly blocks: CodeBlockRecord[] = [];

  next(): string {
    return `b${this.blocks.length + 1}`;
  }

  add(record: CodeBlockRecord): void {
    this.blocks.push(record);
  }

  all(): readonly CodeBlockRecord[] {
    return this.blocks;
  }

  /** The map the snapshot stores, keyed by block id. */
  byId(): Record<string, CodeBlockRecord> {
    return Object.fromEntries(this.blocks.map((b) => [b.id, b]));
  }
}

/**
 * The fence for a block of content.
 *
 * ONE BACKTICK LONGER THAN THE LONGEST RUN INSIDE, minimum three. This is the whole defence against
 * acceptance test 1: a value containing ``` cannot close the block that holds it, so nothing after
 * it is parsed as Markdown. Counting runs rather than occurrences matters — ``` is one run of three,
 * not three runs of one, and a fence of four closes it while a fence of two does not exist.
 */
export function fenceFor(content: string): string {
  let longest = 0;
  let run = 0;
  for (const ch of content) {
    if (ch === '`') {
      run += 1;
      if (run > longest) longest = run;
    } else {
      run = 0;
    }
  }
  return '`'.repeat(Math.max(3, longest + 1));
}

/** `[a-z0-9+-]{1,20}` or `text`. A lang is a highlighter token, never free text in the output. */
export function normaliseLang(raw: unknown): string {
  if (typeof raw !== 'string') return LANG_FALLBACK;
  const lower = raw.toLowerCase();
  return LANG_RE.test(lower) ? lower : LANG_FALLBACK;
}

/** An `xxd`-style dump, for `show: "bytes"`. Computed here so the export and the console agree. */
export function hexDump(bytes: Buffer): string {
  const lines: string[] = [];
  for (let off = 0; off < bytes.length; off += 16) {
    const chunk = bytes.subarray(off, off + 16);
    const hex = Array.from(chunk, (b) => b.toString(16).padStart(2, '0'))
      .join(' ')
      .padEnd(47, ' ');
    const ascii = Array.from(chunk, (b) => (b >= 0x20 && b <= 0x7e ? String.fromCharCode(b) : '.')).join('');
    lines.push(`${off.toString(16).padStart(8, '0')}  ${hex}  |${ascii}|`);
  }
  return lines.join('\n');
}

/** The value forms §4.3 accepts, after evaluation. */
function bytesOf(value: unknown): { bytes: Buffer; source: 'text' | 'b64' } | { ref: string } | { bad: string } {
  if (typeof value === 'string') return { bytes: Buffer.from(value, 'utf8'), source: 'text' };
  if (value && typeof value === 'object') {
    const obj = value as Record<string, unknown>;
    if (typeof obj.b64 === 'string') {
      // A malformed base64 is the author's bug, so it is named rather than silently truncated by
      // Buffer.from's lenient decoder.
      const cleaned = obj.b64.replace(/\s+/g, '');
      if (!/^[A-Za-z0-9+/]*={0,2}$/.test(cleaned)) return { bad: 'b64 is not base64' };
      return { bytes: Buffer.from(cleaned, 'base64'), source: 'b64' };
    }
    if (typeof obj.ref === 'string') return { ref: obj.ref };
  }
  if (value === null || value === undefined) return { bad: 'value is empty' };
  return { bad: `value must be a string, {b64}, or {ref} — got ${Array.isArray(value) ? 'an array' : typeof value}` };
}

/**
 * Register `{% code %}` on a Liquid instance.
 *
 * THE RENDER IS ASYNC, and the engine must therefore use `parseAndRender`, never
 * `parseAndRenderSync`. Under the sync API a yielded promise is handed back to the tag unresolved,
 * and both consequences were measured on 10.30.0: a tag that writes it straight to the emitter puts
 * the literal string `[object Promise]` in the document, and this tag, which reads a field off the
 * resolution, throws a RenderError. A crash is the better of the two and neither is acceptable, so
 * there is no sync path here; `codeTag.test.ts` pins it so a refactor cannot quietly add one.
 */
export function registerCodeTag(liquid: Liquid, deps: CodeTagDeps): void {
  const limit = deps.limitBytes ?? blockLimitBytes();

  liquid.registerTag('code', {
    parse(this: Record<string, unknown>, token: TagToken) {
      const tk = new Tokenizer(token.args);
      this.langToken = tk.readValue();
      tk.skipBlank();
      if (tk.peek() === ',') tk.advance();
      this.valueToken = tk.readValue();
      if (!this.valueToken) {
        throw new Error('{% code %} needs a language and a value: {% code "http", result.request %}');
      }
      const named: Record<string, unknown> = {};
      for (;;) {
        tk.skipBlank();
        if (tk.peek() === ',') {
          tk.advance();
          tk.skipBlank();
        }
        const name = tk.readIdentifier();
        if (!name || !name.content) break;
        tk.skipBlank();
        if (tk.peek() === ':') {
          tk.advance();
          named[name.content] = tk.readValue();
        } else {
          named[name.content] = true;
        }
      }
      this.named = named;
    },

    *render(this: Record<string, any>, ctx: Context, emitter: Emitter): Generator<unknown, void, unknown> {
      const lang = normaliseLang(yield evalToken(this.langToken, ctx));
      const value = yield evalToken(this.valueToken, ctx);
      const named: Record<string, unknown> = {};
      for (const [k, v] of Object.entries(this.named as Record<string, unknown>)) {
        named[k] = v === true ? true : yield evalToken(v as Token, ctx);
      }

      const id = deps.sink.next();
      const wantRedaction = ALWAYS_REDACT.has(lang) || named.redact === true;
      const showBytes = named.show === 'bytes';

      const got = bytesOf(value);
      if ('bad' in got) {
        // A tag whose value makes no sense is an author error and fails the render, unlike an
        // unreadable ref, which is an environment fact and degrades. The distinction is the point:
        // one is fixed by editing the template, the other cannot be.
        throw new Error(`{% code %}: ${got.bad}`);
      }

      let raw: Buffer;
      let fullBytes: number;
      let source: CodeBlockRecord['source'];
      let ref: string | undefined;
      let unresolved: string | undefined;

      if ('ref' in got) {
        source = 'ref';
        ref = got.ref;
        const res = (yield deps.resolveRef(got.ref, limit)) as RefResolution;
        if ('unresolved' in res) {
          unresolved = res.unresolved;
          raw = Buffer.alloc(0);
          fullBytes = 0;
        } else {
          raw = res.bytes;
          fullBytes = res.fullBytes;
        }
      } else {
        source = got.source;
        raw = got.bytes;
        fullBytes = raw.length;
      }

      // Truncate BEFORE redaction, so the redaction rules see a whole message rather than one cut
      // mid-header, and so the cap bounds the work redaction has to do.
      const shown = raw.length > limit ? raw.subarray(0, limit) : raw;
      // COMPARED AGAINST `fullBytes`, NOT against `limit`. A resolver that honours the cap itself —
      // which is the efficient thing to do, since reading 4 MiB to show 1 MiB is 3 MiB of waste —
      // hands back a buffer already at the cap, and a `shown.length > limit` test would then call a
      // truncated block whole. The authority on the original size is the resolver's own count.
      const truncated = fullBytes > shown.length;

      let stored = shown;
      let redacted = false;
      if (wantRedaction && shown.length > 0) {
        const before = shown.toString('latin1');
        const after = deps.redactLatin1(before);
        if (after !== before) {
          stored = Buffer.from(after, 'latin1');
          redacted = true;
        }
      }

      const record: CodeBlockRecord = {
        id,
        lang,
        bytes: stored,
        redacted,
        truncated,
        fullBytes,
        source,
        ...(ref ? { ref } : {}),
        ...(unresolved ? { unresolved } : {}),
        // The originals are kept ONLY when redaction changed something: an unredacted copy of bytes
        // nobody redacted is a second place for the same secret to live.
        ...(redacted ? { raw: Buffer.from(shown) } : {}),
      };
      deps.sink.add(record);

      const content = unresolved
        ? unresolvedMarker(ref!, unresolved)
        : showBytes
          ? hexDump(stored)
          : stored.toString('utf8');

      const fence = fenceFor(content);
      // THE ID TRAVELS IN THE INFO STRING, as `lang kontra-block=b1`. remark splits an info string
      // into `lang` (the first word) and `meta` (the rest), so the marker survives parsing, is
      // matched by render.ts, and is deleted there before anything is stored — which is why a
      // reader never sees it. Matching by POSITION instead would attach a run's bytes to the first
      // literal fenced block any template author happened to write.
      //
      // A leading newline before the closing fence whether or not the content ends in one: a fence
      // that is not at the start of a line is not a fence, and would swallow the rest of the report.
      emitter.write(
        `\n${fence}${lang} kontra-block=${id}\n${content}${content.endsWith('\n') ? '' : '\n'}${fence}\n`
      );
    },
  });
}

/**
 * What a block shows when its ref was not read.
 *
 * Says the ref, says why, and says it in words a reader cannot mistake for the payload. An empty
 * block here would be indistinguishable from a ref that held nothing, which is the confusion that
 * cost a peer engineer a live run in a different part of this system.
 */
function unresolvedMarker(ref: string, why: string): string {
  return `[kontra] this block was not read: ${why}\n[kontra] ref: ${ref}\n[kontra] the run's own data is unaffected; the bytes may still exist in object storage`;
}
