/**
 * REDACTION, THE TYPESCRIPT ARM — the third language to implement rules that two SDKs already agree
 * on, pinned by `shared/conformance/redaction.json`.
 *
 * ── WHY THERE IS NOW A THIRD IMPLEMENTATION AT ALL ─────────────────────────────────────────────
 *
 * `shared/conformance/README.md` used to say this corpus had "Python and Go arms, no TypeScript arm",
 * and gave a reason that was true at the time: "the orchestrator does not compose asks or
 * narrations, so there is no third writer of *these* rules to drift." Run reports make it false. The
 * orchestrator now redacts an HTTP message before it is stored in a report snapshot, which is a third
 * writer of the same rules, so the README's row and this file arrived together.
 *
 * ── WHAT IS HERE, AND THE THREE RULES ARE NOT ONE RULE ─────────────────────────────────────────
 *
 *   • {@link redactSentence} — a secret-shaped assignment and a bare `bearer|basic` credential, in
 *     prose. Replaces the VALUE only, so the key survives: "there was a token here and kontra would
 *     not carry it" is a more useful line than a sentence with a hole in it.
 *   • {@link redactValue} — a whole-key match over a parsed tree, for a HITL ask's fields. A
 *     different marker, because an ask's reader needs to be told why their field is gone.
 *   • {@link redactHttp} — an HTTP request or response, for a report's `{% code "http" %}` block.
 *
 * They are deliberately not merged. The corpus's own header says so, and both SDKs argue it in their
 * module headers: the whole-key rule is anchored to a key in a mapping, the sentence rule has to find
 * a word inside prose, and the HTTP rule has to take a value to end of line. One regex doing all
 * three would be wrong for all three.
 *
 * ── NOT A SECURITY BOUNDARY, AND NOT SOLD AS ONE ───────────────────────────────────────────────
 *
 * The same sentence appears in both SDKs and it is meant. This catches the ordinary mistake — a value
 * that happened to be in scope reaching a document that names what it is carrying. A determined leak
 * defeats it trivially. For reports, the audited reveal path exists because a reader sometimes needs
 * the original bytes, and because redaction being imperfect is a reason to keep the originals
 * reachable rather than a reason to pretend.
 *
 * ── ONE KNOWN CROSS-LANGUAGE DIVERGENCE, MEASURED, AND NOT MINE TO FIX HERE ────────────────────
 *
 * `\s` in Go's regexp is ASCII-only; in JavaScript and Python it includes U+00A0. So for the SENTENCE
 * rule, `token: abc123def456` gives `token: [redacted]` here and in Python, and
 * `token:[redacted]` in Go — the non-breaking space is swallowed into the value there. No corpus case
 * covers it, so the corpus does not currently notice. This file matches Python, which is the majority
 * and the behaviour that preserves the byte. The HTTP rule below avoids the whole question by
 * spelling its whitespace classes `[ \t]`, which is also what RFC 9112 permits.
 */

/** What replaces a value in prose, a header or a JSON member. Pinned as `sentence_redacted`. */
export const REDACTED = '[redacted]';

/**
 * What replaces an ask's field. Pinned as `value_redacted`.
 *
 * A LONGER MARKER THAN THE OTHER ONE, on purpose: an ask is read by a person who is being asked to
 * decide something, and "[redacted]" alone would leave them wondering whether the field was empty.
 */
export const REDACTED_VALUE = '[redacted by kontra: an ask travels through history in the clear]';

/** The word list, as the sentence and HTTP rules spell it — with HYPHEN OR UNDERSCORE affixes. */
const SECRET_WORDS =
  'password|passwd|pwd|secret|secrets|token|api[_-]?key|apikey|access[_-]?key|' +
  'private[_-]?key|credential|credentials|authorization|cookie|session[_-]?key';

/** A header name's character set, per RFC 9110's `token` production. */
const HEADER_NAME_CHARS = "[A-Za-z0-9!#$%&'*+.^_`|~-]";

/**
 * An assignment whose key names a secret.
 *
 * THE SCHEME WORD IS STEPPED OVER, NOT CAPTURED. `Authorization: Bearer eyJhbGciOi…` is the commonest
 * spelling of all of these, and a matcher that took the first word after the colon would redact
 * `Bearer` and leave the credential standing beside it.
 */
export const SECRET_ASSIGNMENT_RE = new RegExp(
  `\\b(?:[A-Za-z0-9]+[_-])?(?:${SECRET_WORDS})(?:[_-][A-Za-z0-9]+)?\\s*[:=]\\s*(?:(?:bearer|basic|token)\\s+)?(\\S+)`,
  'gi'
);

/**
 * `Bearer …` or `Basic …` with no key beside it, which is how it is usually pasted.
 *
 * The 12-character floor is what keeps `bearer of bad news` as prose. It applies ONLY to this rule:
 * `Authorization: Bearer short` is still redacted, because the header name triggers the assignment
 * rule, which has no floor.
 */
export const BEARER_RE = /\b(?:bearer|basic)\s+([A-Za-z0-9._~+/=-]{12,})/gi;

/**
 * A header line whose NAME names a credential, capturing the whole value to end of line.
 *
 * Built from the word list with header-name affixes, which is why `X-Api-Key`, `Set-Cookie`,
 * `Proxy-Authorization` and `X-Amz-Security-Token` all match without being named twice.
 *
 * `[ \t]` AND NOT `\s`, so that all three languages agree on byte 0xA0 — see this file's header.
 */
export const HTTP_CREDENTIAL_HEADER_RE = new RegExp(
  `^(?:[ \\t]*${HEADER_NAME_CHARS}*(?:${SECRET_WORDS})${HEADER_NAME_CHARS}*[ \\t]*:[ \\t]*)([^\\r\\n]+)`,
  'gim'
);

/**
 * A JSON member whose KEY names a secret.
 *
 * The sentence rule misses these entirely — measured, `{"password": "hunter2"}` comes back untouched —
 * because the closing quote sits between the key word and the colon, so `\s*[:=]\s*` never matches.
 * An `http` block carrying a login request stored the password in the clear without this.
 */
export const JSON_CREDENTIAL_PAIR_RE = new RegExp(
  `"[A-Za-z0-9_-]*(?:${SECRET_WORDS})[A-Za-z0-9_-]*"[ \\t]*:[ \\t]*"((?:[^"\\\\]|\\\\.)*)"`,
  'gi'
);

/** A whole key in a mapping, anchored, with UNDERSCORE-ONLY affixes — a different rule, see below. */
export const SECRET_KEY_RE = new RegExp(
  `^(.*_)?(password|passwd|pwd|secret|secrets|token|api_?key|apikey|access_?key|private_?key|credential|credentials|authorization|cookie|session_?key)(_.*)?$`,
  'i'
);

/** How deep {@link redactValue} walks. Matches both SDKs' bound exactly. */
const MAX_DEPTH = 8;

/**
 * Replace capture group 1 of every match, keeping BOTH SIDES of the group.
 *
 * SPAN-BASED, which is what Go's `cutGroup` has always done. Python's original `cut` truncated the
 * match at the end of group 1 — correct for the sentence patterns, where the value is last, and wrong
 * for {@link JSON_CREDENTIAL_PAIR_RE}, whose match continues past the value: it turned
 * `{"password": "hunter2"}` into `{"password": "[redacted]}`, eating the closing quote and storing
 * invalid JSON. Python now does it this way too, so all three languages replace by the same method
 * rather than by three spellings that happen to agree on the cases somebody wrote down.
 *
 * THE `d` FLAG, FOR EXACT GROUP OFFSETS. JavaScript gives no group indices without it, and the
 * obvious substitute — searching the match for the group's text — is wrong exactly when the text
 * before the group can contain the group's own content: `token=token` has `token` both before the
 * value and as the value, and `indexOf` would replace the key. `d` is supported from Node 16, far
 * under this repo's `>=22.13.0` floor.
 *
 * A FRESH RegExp PER CALL, because a `/g/` pattern carries `lastIndex` between calls: a shared one
 * would make the second call on the same input behave differently from the first.
 */
function cutGroupExact(re: RegExp, text: string, marker: string = REDACTED): string {
  const withD = re.flags.includes('d') ? re.flags : `${re.flags}d`;
  const pattern = new RegExp(re.source, withD.includes('g') ? withD : `${withD}g`);
  let out = '';
  let at = 0;
  for (let m = pattern.exec(text); m !== null; m = pattern.exec(text)) {
    const indices = (m as RegExpExecArray & { indices?: Array<[number, number] | undefined> }).indices;
    const span = indices?.[1];
    // A zero-length match would spin `exec` forever; nudging `lastIndex` is the standard guard. None
    // of these patterns can match empty — every one requires at least a key and a separator — but the
    // guard costs nothing and the alternative is a hung renderer.
    if (m[0].length === 0) pattern.lastIndex += 1;
    if (!span) continue;
    out += text.slice(at, span[0]) + marker;
    at = span[1];
  }
  return out + text.slice(at);
}

/**
 * `sentence` with the value of any secret-shaped assignment replaced.
 *
 * IT REPLACES RATHER THAN DROPPING, and it does not throw. A run that died because its log line was
 * impolite would be an outage created by a safety rule, and the author's fix — stop putting the value
 * in the sentence — is the same either way.
 */
export function redactSentence(sentence: string): string {
  // Assignments first, so a named header is redacted as a whole; the bare-scheme pass then picks up
  // the pasted `Bearer …` that named nothing, and cannot re-match what the first pass left behind,
  // because REDACTED carries brackets no credential pattern here accepts.
  return cutGroupExact(BEARER_RE, cutGroupExact(SECRET_ASSIGNMENT_RE, sentence));
}

/**
 * `message` — an HTTP request or response — with every credential value replaced.
 *
 * THREE PASSES, IN THIS ORDER:
 *
 *   1. Credential HEADER values, to end of line. First, because it replaces the whole value, so the
 *      later passes find REDACTED where a credential was and cannot be fooled by the half-redacted
 *      line the sentence rule alone leaves on `Cookie: a=1; b=2`.
 *   2. {@link redactSentence}, unchanged — which catches a credential in a request line's query
 *      string (`GET /x?token=abc`) and a bare pasted `Bearer …`, neither of which is a header.
 *   3. JSON members whose key names a secret, which pass 2 cannot see.
 *
 * IDEMPOTENT, which a re-rendered snapshot depends on.
 */
export function redactHttp(message: string): string {
  const withHeaders = cutGroupExact(HTTP_CREDENTIAL_HEADER_RE, message);
  return cutGroupExact(JSON_CREDENTIAL_PAIR_RE, redactSentence(withHeaders));
}

/**
 * A parsed tree with every value under a secret-shaped KEY replaced.
 *
 * A DIFFERENT RULE FROM THE OTHER TWO, and the affix characters are the visible difference:
 * {@link SECRET_KEY_RE} allows `_` only, so `api-key` as a mapping key is NOT redacted here, while
 * `X-Api-Key` as a header name IS redacted by the HTTP rule. That asymmetry is deliberate in both
 * SDKs and copying one pattern for both jobs would quietly change the ask contract.
 */
export function redactValue(value: unknown, depth = 0): unknown {
  if (depth > MAX_DEPTH) return value;
  if (Array.isArray(value)) return value.map((v) => redactValue(v, depth + 1));
  if (value !== null && typeof value === 'object') {
    const out: Record<string, unknown> = {};
    for (const [k, v] of Object.entries(value as Record<string, unknown>)) {
      out[k] = SECRET_KEY_RE.test(k) ? REDACTED_VALUE : redactValue(v, depth + 1);
    }
    return out;
  }
  return value;
}

/** `sentence` with every run of whitespace collapsed to one space, and trimmed. */
export function oneLine(sentence: string): string {
  return sentence.replace(/\s+/g, ' ').trim();
}
