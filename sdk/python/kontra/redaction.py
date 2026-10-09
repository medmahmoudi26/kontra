"""Sentence redaction — the guard that outlived narration (ADR 0050 §2).

WHY THIS IS ITS OWN MODULE. It used to live in `kontra/narrate.py`, and when `speak` was removed
this came with it rather than going with it: `redact` is pinned by a CROSS-LANGUAGE CONFORMANCE
CORPUS (`shared/conformance/redaction.json`) with a Go peer in `sdk/go/hitl/redaction_conformance_test.go`,
so it is a contract two SDKs agree on, not narration machinery. Deleting it with its old host would
have broken that agreement to remove a feature it has nothing to do with.

WHAT IT IS AND IS NOT. **Not a security boundary and not sold as one.** It is a guard against the
ordinary mistake — an f-string that interpolated something which happened to be in scope — and it
catches that one where the sentence names what it is carrying. A determined leak defeats it
trivially; that is not the threat it addresses.
"""

from __future__ import annotations

import re

__all__ = [
    "REDACTED",
    "BEARER_RE",
    "SECRET_ASSIGNMENT_RE",
    "HTTP_CREDENTIAL_HEADER_RE",
    "JSON_CREDENTIAL_PAIR_RE",
    "one_line",
    "redact",
    "redact_http",
]

#: What replaces a value. Carries characters no credential pattern here accepts, which is what makes
#: the two passes in `redact` unable to re-match each other's output.
REDACTED = "[redacted]"

SECRET_ASSIGNMENT_RE = re.compile(
    r"\b(?:[A-Za-z0-9]+[_-])?"
    r"(?:password|passwd|pwd|secret|secrets|token|api[_-]?key|apikey|access[_-]?key|"
    r"private[_-]?key|credential|credentials|authorization|cookie|session[_-]?key)"
    r"(?:[_-][A-Za-z0-9]+)?"
    # THE SCHEME WORD IS STEPPED OVER, NOT CAPTURED. `Authorization: Bearer eyJhbGciOi…` is the
    # commonest spelling of all of these, and a matcher that took the first word after the colon
    # would redact `Bearer` and leave the credential standing beside it.
    r"\s*[:=]\s*(?:(?:bearer|basic|token)\s+)?(\S+)",
    re.IGNORECASE,
)

#: `Authorization: Bearer …` without the header name, which is how it is usually pasted, and
#: `Basic …` beside it. Bounded to something long enough to be a credential rather than a word, so
#: `bearer of bad news` is prose and `Bearer eyJhbGciOi…` is not.
BEARER_RE = re.compile(r"\b(?:bearer|basic)\s+([A-Za-z0-9._~+/=-]{12,})", re.IGNORECASE)


def one_line(sentence: str) -> str:
    """`sentence` with every run of whitespace collapsed to one space, and trimmed.

    Normalisation and not truncation: every word survives, in order, and the author's meaning with
    them. Nothing else about the prose is touched.
    """
    return re.sub(r"\s+", " ", sentence).strip()


def redact(sentence: str) -> str:
    """`sentence` with the value of any secret-shaped assignment replaced by {@link REDACTED}.

    IT REPLACES RATHER THAN DROPPING THE SENTENCE, and it does not raise. A run that died because
    its log line was impolite would be an outage created by a safety rule, and the author's fix —
    stop putting the value in the sentence — is the same either way.
    """

    def cut(m: "re.Match[str]") -> str:
        # Only the VALUE is replaced; the key stays, because "there was a token here and kontra
        # would not carry it" is a more useful line than a sentence with a hole in it.
        return m.group(0)[: m.start(1) - m.start(0)] + REDACTED

    # Assignments first, so a named header is redacted as a whole; the bare-scheme pass then picks
    # up the pasted `Bearer …` that named nothing, and cannot re-match what the first pass left
    # behind ({@link REDACTED} carries characters no credential pattern here accepts).
    return BEARER_RE.sub(cut, SECRET_ASSIGNMENT_RE.sub(cut, sentence))

# ── THE HTTP MESSAGE RULE ───────────────────────────────────────────────────────────────────────
#
# A THIRD RULE AND NOT A WIDENING OF THE SECOND, for a measured reason. `SECRET_ASSIGNMENT_RE`
# captures `(\S+)` — one run of non-whitespace — which is right for prose and wrong for a header:
#
#     Cookie: a=1; b=2                        -> Cookie: [redacted] b=2
#     Set-Cookie: sid=zzz; Path=/; HttpOnly   -> Set-Cookie: [redacted] Path=/; HttpOnly
#
# Measured, in both languages. That is the worst failure shape available: the line reads as redacted
# while the second cookie pair is still in the clear, and a reviewer scanning a code block sees
# `[redacted]` and moves on. Widening `(\S+)` to end-of-line would fix it and would also change
# prose redaction everywhere — `token=abc and then more words` would lose the rest of the sentence,
# and Go's `narrate.Summary` is a live caller. So headers get their own rule, and the sentence rule
# is left exactly as it was.
#
# WHY THE WHITESPACE CLASSES ARE SPELLED `[ \t]` AND NOT `\s`. Go's regexp `\s` is ASCII-only while
# Python's and JavaScript's include U+00A0, and this rule runs over a LATIN-1 view of raw bytes, where
# byte 0xA0 is U+00A0. The existing sentence rule already diverges because of that —
# `token:\xa0abc123def456` redacts to `token:[redacted]` in Go and `token:\xa0[redacted]` in Python,
# which the corpus does not catch — and a new rule gets to not inherit it. `[ \t]` is also what RFC
# 9112 actually permits around a field value.

#: Header names whose value is a credential. Built from the word list with HEADER-NAME affixes, which
#: is why `X-Api-Key`, `Set-Cookie`, `Proxy-Authorization` and `X-Amz-Security-Token` all match
#: without being named: the affix class allows the hyphens a header name uses. The six names spec §5
#: lists are each pinned by their own corpus case rather than by a second spelling here.
#:
#: The VALUE IS THE ONLY CAPTURING GROUP, so `cut` — which replaces group 1 — works unchanged.
HTTP_CREDENTIAL_HEADER_RE = re.compile(
    r"^(?:[ \t]*[A-Za-z0-9!#$%&'*+.^_`|~-]*"
    r"(?:password|passwd|pwd|secret|secrets|token|api[_-]?key|apikey|access[_-]?key|"
    r"private[_-]?key|credential|credentials|authorization|cookie|session[_-]?key)"
    r"[A-Za-z0-9!#$%&'*+.^_`|~-]*[ \t]*:[ \t]*)([^\r\n]+)",
    re.IGNORECASE | re.MULTILINE,
)

#: A JSON object member whose KEY names a secret. The sentence rule misses these entirely — measured,
#: `{"password": "hunter2"}` comes back untouched — because the closing quote sits between the key and
#: the colon and so `\s*[:=]\s*` never matches. An `http` code block carrying a login request would
#: therefore store the password in the clear, which is the one gap worth closing beyond §5's letter:
#: the rule exists so that an export of a request never contains the credential it sent.
#:
#: Escaped characters inside the value are matched so a value containing `\"` is still redacted whole.
JSON_CREDENTIAL_PAIR_RE = re.compile(
    r'"[A-Za-z0-9_-]*'
    r"(?:password|passwd|pwd|secret|secrets|token|api[_-]?key|apikey|access[_-]?key|"
    r"private[_-]?key|credential|credentials|authorization|cookie|session[_-]?key)"
    r'[A-Za-z0-9_-]*"[ \t]*:[ \t]*"((?:[^"\\]|\\.)*)"',
    re.IGNORECASE,
)


def redact_http(message: str) -> str:
    """`message` — an HTTP request or response, as text — with every credential value replaced.

    THREE PASSES, IN THIS ORDER, and the order is what makes them compose:

    1. Credential HEADER values, to end of line. Runs first because it replaces the whole value, so
       the passes after it find {@link REDACTED} where a credential was and cannot be fooled by the
       half-redacted line the sentence rule alone would leave.
    2. The existing sentence and bearer rules, unchanged. This is what §5 means by reusing them: they
       catch a credential in a request line's query string (`GET /x?token=abc`) and a bare pasted
       `Bearer …`, neither of which is a header.
    3. JSON members whose key names a secret, which pass 2 cannot see.

    IDEMPOTENT. `[redacted]` carries brackets that no credential pattern here accepts, so a second
    application changes nothing — which matters because a stored snapshot may be re-rendered.

    NOT A SECURITY BOUNDARY, exactly as `redact` is not: it is a guard against a credential reaching a
    document by accident, and the audited reveal path exists because a reader sometimes needs the
    original bytes.
    """

    with_headers = _cut_group(HTTP_CREDENTIAL_HEADER_RE, message)
    with_prose = redact(with_headers)
    return _cut_group(JSON_CREDENTIAL_PAIR_RE, with_prose)


def _cut_group(pattern: "re.Pattern[str]", text: str) -> str:
    """`text` with capture group 1 of every match replaced, keeping BOTH SIDES of the group.

    `redact`'s own `cut` helper truncates the match at the end of group 1, which is correct there
    because the value is the last thing the sentence patterns match. It is wrong for a pattern whose
    match continues past the value: the first version of {@link JSON_CREDENTIAL_PAIR_RE} turned
    `{"password": "hunter2"}` into `{"password": "[redacted]}` — the closing quote eaten, producing
    invalid JSON in a stored snapshot. A test caught it.

    This is a span-based replacement, which is what Go's `cutGroup` has always done
    (`sdk/go/narrate/narrate.go`). The two languages now do the same thing by the same method rather
    than by two spellings that happen to agree on the cases anybody wrote down.
    """
    out: list[str] = []
    at = 0
    for m in pattern.finditer(text):
        if m.start(1) < 0:
            continue
        out.append(text[at : m.start(1)])
        out.append(REDACTED)
        at = m.end(1)
    out.append(text[at:])
    return "".join(out)
