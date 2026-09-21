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

__all__ = ["REDACTED", "BEARER_RE", "SECRET_ASSIGNMENT_RE", "one_line", "redact"]

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
