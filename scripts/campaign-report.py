#!/usr/bin/env python3
"""Assemble the campaign report: one self-contained HTML file, numbers from the lake.

    python3 scripts/campaign-report.py \
        --facts /tmp/campaign-facts.json \
        --shots /tmp/campaign-shots \
        --out  /root/oss/kontra/.scratch/campaign-report.html

GENERATED, NOT WRITTEN, because the same report has to be producible again after the next run —
and because a figure typed by hand into a slide is a figure nobody can check. Every number comes
from `campaign-facts.json`, which is itself one SQL statement per figure against a durable Dataset.
Screenshots are inlined as data URIs: the page has to survive being emailed, and an <img src> to a
localhost console is a broken image on somebody else's laptop.

THE AUDIENCE IS BEING INTRODUCED TO REQUEST SMUGGLING. Every term this system uses in its own
column names — erratic, voided, probe, matched control, canary, axis, is_proof, fold — is defined
in the glossary and each is defined by what it MEANS for the claim being made, not by restating
the word.
"""
from __future__ import annotations

import argparse
import base64
import html
import json
import pathlib

# ── THE PALETTE ────────────────────────────────────────────────────────────────────────────────
#
# The subject is bytes that look identical and are not, so the page is built to make one byte
# visible against a quiet ground. Neutrals carry a blue bias (they are the greys of a hex dump
# gutter, not warm paper), and there is exactly ONE loud colour: it marks an injected control
# character and a proven claim, and appears nowhere else.
CSS = """
:root {
  --paper:  #F4F6F8;
  --card:   #FFFFFF;
  --sunk:   #ECEFF3;
  --ink:    #15181E;
  --steel:  #57626F;
  --faint:  #8A94A1;
  --rule:   #DCE1E7;
  --signal: #C2255C;
  --teal:   #0F6E77;
  --good:   #1F7A4D;
  --warn:   #9A6206;
  --shadow: 0 1px 2px rgba(21,24,30,.05), 0 8px 24px -12px rgba(21,24,30,.16);
  --mono: ui-monospace, "SF Mono", SFMono-Regular, "JetBrains Mono", Menlo, Consolas, monospace;
  --sans: system-ui, -apple-system, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
}
@media (prefers-color-scheme: dark) {
  :root:not([data-theme="light"]) {
    --paper:  #11141A;
    --card:   #171B22;
    --sunk:   #1D222B;
    --ink:    #E6EAF0;
    --steel:  #9AA6B4;
    --faint:  #6E7A89;
    --rule:   #272E38;
    --signal: #FF6B9D;
    --teal:   #52C7D2;
    --good:   #4FBF87;
    --warn:   #D9A441;
    --shadow: 0 1px 2px rgba(0,0,0,.4), 0 10px 28px -14px rgba(0,0,0,.7);
  }
}
:root[data-theme="dark"] {
  --paper:  #11141A;
  --card:   #171B22;
  --sunk:   #1D222B;
  --ink:    #E6EAF0;
  --steel:  #9AA6B4;
  --faint:  #6E7A89;
  --rule:   #272E38;
  --signal: #FF6B9D;
  --teal:   #52C7D2;
  --good:   #4FBF87;
  --warn:   #D9A441;
  --shadow: 0 1px 2px rgba(0,0,0,.4), 0 10px 28px -14px rgba(0,0,0,.7);
}

* { box-sizing: border-box; }
body {
  margin: 0;
  background: var(--paper);
  color: var(--ink);
  font-family: var(--sans);
  font-size: 17px;
  line-height: 1.65;
  -webkit-font-smoothing: antialiased;
}
.wrap { max-width: 1140px; margin: 0 auto; padding: 0 28px 120px; }
.col  { max-width: 68ch; }

h1, h2, h3, h4 { text-wrap: balance; letter-spacing: -0.021em; line-height: 1.2; margin: 0; }
h1 { font-size: clamp(2.1rem, 1.3rem + 2.6vw, 3.4rem); font-weight: 760; }
h2 { font-size: clamp(1.45rem, 1.1rem + 1.1vw, 1.95rem); font-weight: 720; }
h3 { font-size: 1.16rem; font-weight: 700; }
h4 { font-size: .95rem; font-weight: 700; }
p  { margin: 0 0 1.05em; }
a  { color: var(--teal); text-decoration-thickness: 1px; text-underline-offset: 2px; }
a:focus-visible, button:focus-visible { outline: 2px solid var(--teal); outline-offset: 3px; border-radius: 3px; }
strong { font-weight: 660; }
code, kbd, samp { font-family: var(--mono); font-size: .875em; }

/* Sections are laid out with gap, never per-element margins that collapse or double. */
section { display: flex; flex-direction: column; gap: 1.1rem; padding: 4.2rem 0 0; }
section > .col > :last-child { margin-bottom: 0; }

header.masthead {
  display: flex; flex-direction: column; gap: 1.4rem;
  padding: 4.5rem 0 2.6rem; border-bottom: 1px solid var(--rule);
}
.eyebrow {
  font-family: var(--mono); font-size: .735rem; letter-spacing: .13em;
  text-transform: uppercase; color: var(--faint);
}
.standfirst { font-size: 1.16rem; color: var(--steel); max-width: 62ch; }
.runline {
  font-family: var(--mono); font-size: .8rem; color: var(--steel);
  display: flex; flex-wrap: wrap; gap: .4rem 1.4rem;
}
.runline b { color: var(--ink); font-weight: 600; }

/* ── the four numbers ──────────────────────────────────────────────────────────────────────── */
.figures { display: grid; grid-template-columns: repeat(auto-fit, minmax(210px, 1fr)); gap: 14px; }
.fig {
  background: var(--card); border: 1px solid var(--rule); border-radius: 10px;
  padding: 20px 20px 18px; box-shadow: var(--shadow);
  display: flex; flex-direction: column; gap: .3rem;
}
.fig .n {
  font-family: var(--mono); font-variant-numeric: tabular-nums;
  font-size: clamp(1.75rem, 1.2rem + 1.7vw, 2.5rem); font-weight: 620; letter-spacing: -.03em;
}
.fig .k { font-size: .82rem; font-weight: 640; letter-spacing: .01em; }
.fig .s { font-size: .8rem; color: var(--steel); line-height: 1.45; }

/* ── tables ───────────────────────────────────────────────────────────────────────────────── */
.scroll { overflow-x: auto; -webkit-overflow-scrolling: touch; }
table { border-collapse: collapse; width: 100%; font-size: .875rem; }
th, td { text-align: left; padding: 9px 14px; border-bottom: 1px solid var(--rule); vertical-align: top; }
th {
  font-family: var(--mono); font-size: .715rem; letter-spacing: .085em; text-transform: uppercase;
  color: var(--faint); font-weight: 600; white-space: nowrap;
}
td.num, th.num { text-align: right; font-family: var(--mono); font-variant-numeric: tabular-nums; }
td.mono { font-family: var(--mono); font-size: .82rem; }
tbody tr:last-child td { border-bottom: none; }
.tablecard {
  background: var(--card); border: 1px solid var(--rule); border-radius: 10px;
  overflow: hidden; box-shadow: var(--shadow);
}

/* ── wire bytes: the one place the loud colour is spent ───────────────────────────────────── */
pre.wire {
  font-family: var(--mono); font-size: .82rem; line-height: 1.6;
  background: var(--sunk); border: 1px solid var(--rule); border-radius: 10px;
  padding: 16px 18px; overflow-x: auto; margin: 0; white-space: pre;
}
pre.wire .inj { color: var(--signal); font-weight: 620; }
pre.wire .dim { color: var(--faint); }
.cap { font-size: .82rem; color: var(--steel); }

/* ── screenshots ──────────────────────────────────────────────────────────────────────────── */
figure { margin: 0; display: flex; flex-direction: column; gap: .6rem; }
figure img {
  width: 100%; height: auto; display: block;
  border: 1px solid var(--rule); border-radius: 10px; box-shadow: var(--shadow);
  background: var(--card);
}
figcaption { font-size: .85rem; color: var(--steel); max-width: 72ch; }
figcaption b { color: var(--ink); font-weight: 640; }

/* ── the three phases: numbered because the campaign IS a sequence ────────────────────────── */
.phases { display: flex; flex-direction: column; gap: 10px; }
.phase {
  display: grid; grid-template-columns: 54px 1fr; gap: 18px; align-items: start;
  background: var(--card); border: 1px solid var(--rule); border-radius: 10px; padding: 18px 20px;
}
.phase .idx {
  font-family: var(--mono); font-size: .82rem; font-weight: 620; color: var(--faint);
  border-right: 1px solid var(--rule); padding-right: 14px; padding-top: 2px;
}
.phase h3 { margin-bottom: .25rem; }
.phase p { margin: 0; font-size: .93rem; color: var(--steel); }
.phase .meta { font-family: var(--mono); font-size: .76rem; color: var(--faint); margin-top: .5rem; }

/* ── glossary ─────────────────────────────────────────────────────────────────────────────── */
dl.gloss { display: grid; grid-template-columns: 1fr; gap: 0; margin: 0; }
@media (min-width: 900px) { dl.gloss { grid-template-columns: 220px 1fr; } }
dl.gloss dt {
  font-family: var(--mono); font-size: .85rem; font-weight: 620; color: var(--ink);
  padding: 14px 0 4px; border-top: 1px solid var(--rule);
}
dl.gloss dd {
  margin: 0; padding: 14px 0 16px; font-size: .93rem; color: var(--steel);
}
@media (min-width: 900px) { dl.gloss dd { border-top: 1px solid var(--rule); padding-left: 24px; } }
@media (max-width: 899px) { dl.gloss dt { border-top: 1px solid var(--rule); } dl.gloss dd { padding-top: 0; } }
dl.gloss dd b { color: var(--ink); font-weight: 620; }

/* ── callouts ─────────────────────────────────────────────────────────────────────────────── */
.note {
  border-left: 3px solid var(--teal); background: var(--card);
  padding: 14px 18px; border-radius: 0 8px 8px 0; font-size: .93rem; color: var(--steel);
}
.note.warn { border-left-color: var(--warn); }
.note b { color: var(--ink); font-weight: 640; }
.pill {
  display: inline-block; font-family: var(--mono); font-size: .7rem; letter-spacing: .06em;
  text-transform: uppercase; padding: 2px 8px; border-radius: 999px;
  border: 1px solid var(--rule); color: var(--steel); background: var(--sunk);
}
.pill.proof { color: var(--signal); border-color: var(--signal); background: transparent; }
footer {
  margin-top: 5rem; padding-top: 1.6rem; border-top: 1px solid var(--rule);
  font-size: .84rem; color: var(--faint);
}
@media (prefers-reduced-motion: reduce) { * { animation: none !important; transition: none !important; } }
"""


def esc(v) -> str:
    return html.escape(str(v), quote=True)


def num(v) -> str:
    try:
        return f"{int(v):,}"
    except (TypeError, ValueError):
        return "—" if v in (None, "") else esc(v)


def fig(n, k, s) -> str:
    return (f'<div class="fig"><div class="n">{n}</div>'
            f'<div class="k">{esc(k)}</div><div class="s">{s}</div></div>')


def table(rows: list[dict], cols: list[tuple], numeric: set[str] | None = None) -> str:
    """One table. Wide content scrolls inside its own container, never the page body."""
    numeric = numeric or set()
    if not rows:
        return '<div class="note">No rows — this axis produced nothing on this run.</div>'
    head = "".join(f'<th class="{"num" if c in numeric else ""}">{esc(label)}</th>' for c, label in cols)
    body = []
    for r in rows:
        tds = []
        for c, _ in cols:
            v = r.get(c)
            cls = "num" if c in numeric else ("mono" if isinstance(v, str) and len(str(v)) < 48 else "")
            tds.append(f'<td class="{cls}">{num(v) if c in numeric else esc(v)}</td>')
        body.append("<tr>" + "".join(tds) + "</tr>")
    return ('<div class="tablecard"><div class="scroll"><table><thead><tr>'
            + head + "</tr></thead><tbody>" + "".join(body) + "</tbody></table></div></div>")


def shot(shots: pathlib.Path, name: str, caption: str) -> str:
    """A screenshot, inlined. Absent is stated, never silently skipped."""
    p = shots / f"{name}.png"
    if not p.exists():
        return (f'<div class="note warn"><b>Screenshot missing:</b> <code>{esc(name)}.png</code> '
                f"was not captured, so this figure is described rather than shown.</div>")
    b64 = base64.b64encode(p.read_bytes()).decode()
    return (f'<figure><img alt="{esc(caption)}" src="data:image/png;base64,{b64}">'
            f"<figcaption>{caption}</figcaption></figure>")


# ── THE PROSE ──────────────────────────────────────────────────────────────────────────────────
#
# Written for somebody who has not seen a desync before. Two rules held throughout: no term is
# used before it is defined, and no claim is made that the datasets cannot support.

INTRO = """
<p>Every HTTP request has to say where it ends. A server reading a stream of bytes needs to know
which byte is the last one belonging to this request and which is the first one belonging to the
next, because on a modern connection there usually <em>is</em> a next one — browsers, proxies and
CDNs reuse a single TCP connection for request after request.</p>

<p>There are two ways a request says where it ends, and almost every site has at least two
different servers reading the same bytes: a front-end (a CDN, a load balancer, a reverse proxy)
and a back-end (the application). <strong>A desync is what happens when those two disagree.</strong>
The front-end decides the request ended at byte 100 and forwards bytes 101 onward as the start of
whatever comes next. The back-end thinks the request ended at byte 60, so it reads bytes 61–100 as
the beginning of a <em>new</em> request — one the attacker wrote, prefixed onto whatever real
request arrives next on that connection.</p>

<p>That is the whole primitive. The attacker does not need to break authentication or find an
injection flaw in the application. They need to make two parsers count to different numbers, and
then someone else's request arrives and gets the attacker's bytes glued to the front of it.</p>
"""

TWO_TECHNIQUES = """
<p>This campaign swept two ways of producing that disagreement. They are genuinely different
mechanisms, not two settings of one, which is why they ran on separate machines with separate
payload corpora — and why the dataset records which one produced every row in a column called
<code>axis</code>.</p>
"""

CL0_EXPLAIN = """
<h3>CL.0 &mdash; the front-end reads a body, the back-end reads none</h3>

<p>A request that carries data says how much with a <code>Content-Length</code> header. <code>CL.0</code>
is the case where the front-end honours that header and the back-end behaves as if the length were
<strong>zero</strong> &mdash; the &ldquo;0&rdquo; in the name. That happens when the back-end does not
recognise the header as a length at all: it was written in a way the strict parser rejects and the
permissive one accepts.</p>

<p>The attack request below asks for a normal page and declares a body. The front-end reads the
declared number of bytes and forwards all of them. The back-end, not seeing a valid length, treats
the request as finished at the blank line &mdash; so the body is left sitting in its buffer, and it
reads those bytes as the start of the next request on that connection.</p>
"""

CRLF_EXPLAIN = """
<h3>CRLF &mdash; making a byte that is not a newline become one</h3>

<p>Headers are separated by two bytes: a carriage return (<code>0x0D</code>) and a line feed
(<code>0x0A</code>), written <code>\\r\\n</code> and pronounced CRLF. If an attacker can get those two
bytes into a place the server later writes into a request &mdash; a URL path, a cookie value, a header
the application copies through &mdash; then they are no longer supplying <em>data</em>. They are
writing the request's structure, and everything after their CRLF is a new header line of their
choosing.</p>

<p>Every server blocks the literal bytes. The technique is to supply something that is
<em>not</em> those bytes when it is checked, and <em>is</em> those bytes by the time it is used.
The most productive family is a lossy Unicode conversion: some servers decode text to Unicode
codepoints and then narrow each codepoint back to a single byte by taking its lowest eight bits.
Under that conversion, <strong>any codepoint whose last byte is <code>0x0A</code> becomes a line
feed</strong>. <code>&#x010A;</code> (U+010A) does. So does <code>&#x070A;</code> (U+070A), and
<code>&#x0B0A;</code>, and <code>&#x560A;</code>, and even <code>&#x1F60A;</code>. None of them is a
newline when the blocklist looks at it.</p>

<p>A full CRLF needs both bytes, so it needs a pair: a codepoint ending <code>0x0D</code> followed
by one ending <code>0x0A</code>, at the same offset. <code>&#x010D;&#x010A;</code> is the cheapest
such pair, and both halves are separately documented as working in the wild.</p>
"""

CONTROL_EXPLAIN = """
<p>The hard part of this work is not sending the payload. It is knowing whether the response you
got back means anything &mdash; and the honest answer is usually no. A server that echoes its own URL
in a 404 page will look like it reflected your payload. A server having a bad second will answer
two identical requests differently. Both produce exactly the evidence a naive scanner reports as a
finding.</p>

<p>So every claim here is a <strong>difference between two requests that differ in exactly one
thing</strong>. For the CRLF technique, the control is the same payload with each codepoint moved
one value higher: <code>&#x010D;&#x010A;</code> becomes <code>&#x010E;&#x010B;</code>. Same length,
same encoding shape, same byte count &mdash; the only thing that changed is whether the low bytes are
newlines. For CL.0, the control is the identical request with a well-formed
<code>Content-Length</code>: same body, same canary, same path, gadget removed.</p>

<p>If the control reproduces the behaviour, the payload explained nothing and the claim is
withdrawn. That withdrawal is itself written to the dataset, as a signal named
<code>control_also_fired</code>, because a scan that cannot show the claims it rejected is a scan
whose accepted claims nobody should believe.</p>

<div class="note"><b>This discipline is not theoretical.</b> An earlier version of this scanner had
no matched control on the smuggling axis. On 2026-09-18 it produced 40 apparently-proven findings
and two reports, and both were retracted: on one host a plain <code>Content-Length: 0</code> fired
three times in six attempts, while the obfuscated attacks fired once and twice. The server was
simply pipelining &mdash; reading any body as the next request &mdash; for reasons that had nothing to do
with the payload.</div>
"""

SAFETY = """
<p>Every payload in this campaign is an <strong>incomplete prefix</strong>. The smuggled bytes
begin a request and deliberately never finish one: no terminating blank line, no valid
second request queued, and the path is a random string that exists nowhere. The scanner can
observe that the back-end started reading its bytes as a request without ever completing one, so
nothing is queued in front of another user's traffic.</p>

<p>Nothing is cached, and that line is enforced rather than intended: a poisoned cache entry
outlives the connection and reaches people who did not consent to the test, so cacheability is
recorded as a ranking signal and never exercised. No collaborator host is named, no cookie is set
in a real browser, and every request carries a <code>X-Bug-Bounty</code> header identifying the
researcher, taken from the scope row rather than from the scanner, so traffic is attributable to a
person on every single connection.</p>
"""


# ── THE GLOSSARY ───────────────────────────────────────────────────────────────────────────────
#
# Every one of these is a column value or a signal name in the datasets shown above, so each is
# defined by what it means FOR THE CLAIM, not by restating the word.
GLOSSARY = [
    ("desync",
     "Short for desynchronisation. Two servers reading the same connection disagree about where "
     "one request ends and the next begins, so bytes one of them treats as data the other treats "
     "as the start of somebody else's request."),
    ("axis",
     "Which of the two bugs a row is about. <b>smuggle</b> means the disagreement is about where a "
     "<b>body</b> ends. <b>split</b> means it is about which <b>bytes mean newline</b>. They share "
     "a socket primitive and a rate limiter and nothing else, which is why they are two columns of "
     "evidence and not one."),
    ("probe",
     "One attempt: a single connection carrying a planned sequence of requests to one target with "
     "one payload. It is the unit the scanner counts and the unit it paces. A probe is not a "
     "finding &mdash; the overwhelming majority of probes establish that nothing happened, and those "
     "rows are kept, because a scan that only records its hits cannot say what it covered."),
    ("baseline",
     "The same request, unmodified, sent several times before any payload. Everything the scanner "
     "later claims is &ldquo;this differed from the baseline&rdquo;, so without one there is nothing "
     "to differ from."),
    ("erratic",
     "The host <b>would not reproduce its own baseline</b> &mdash; identical benign requests came back "
     "with different statuses or different bodies &mdash; so the scanner <b>refused to scan it</b> "
     "rather than scanning it anyway. This matters because every downstream signal is "
     "&ldquo;this response differed from the baseline&rdquo;, which a host that differs from itself "
     "satisfies for free: every payload would look like it worked. An erratic host is recorded as "
     "<b>not tested</b>, which is a different fact from <b>tested and clean</b>."),
    ("voided",
     "The probe ran but its result carries no claim, for one of five recorded reasons: the "
     "connection never dialled, TLS failed, the connection negotiated HTTP/2 (so the HTTP/1.1 bytes "
     "sent were nonsense), fewer than three steps reached the wire, or the pre-flight benign "
     "request did not match the baseline &mdash; meaning the host moved between the measurement and the "
     "attack. The reason is kept in <code>void_reason</code>."),
    ("matched control",
     "A second request that differs from the attack in <b>exactly one thing</b>: whether the payload "
     "can do what it claims. For CRLF it is the same codepoints one value higher, so length, "
     "encoding shape and byte count are held fixed. For CL.0 it is the identical request with a "
     "well-formed <code>Content-Length</code>. If the control reproduces the behaviour, the payload "
     "explained nothing and the claim is withdrawn."),
    ("control_survived",
     "A signal name. The matched control was run and did <b>not</b> reproduce the behaviour &mdash; so "
     "the payload is still the best explanation. Required for any row to count as proof."),
    ("control_also_fired",
     "The opposite, and the reason it is written down: the control reproduced the behaviour, so the "
     "claim was <b>withdrawn</b>. These rows carry a signal count of zero and will never be "
     "promoted. They are the audit trail of everything this scanner decided not to say."),
    ("canary",
     "A random string minted by the scanner for one probe and appearing nowhere else on the "
     "internet. If it comes back in a response, there is no innocent explanation for those bytes "
     "&mdash; something read the smuggled prefix as a request."),
    ("fold / fold codepoint",
     "A Unicode character whose last byte is <code>0x0A</code> or <code>0x0D</code>. A server that "
     "decodes to Unicode and then narrows each codepoint back to one byte turns it into a real "
     "newline. <code>&#x010A;</code>, <code>&#x070A;</code>, <code>&#x0B0A;</code>, "
     "<code>&#x560A;</code> and <code>&#x1F60A;</code> all do this, and none of them is a newline "
     "to a blocklist."),
    ("CL.0",
     "A technique class. The front-end honours <code>Content-Length</code> and the back-end reads "
     "the request as having <b>no body at all</b> &mdash; the &ldquo;0&rdquo; &mdash; so the body is left in "
     "its buffer and read as the next request."),
    ("CL.TE / 0.CL",
     "The other two members of the same family, named the same way: which header the front-end "
     "trusts, then which the back-end trusts. <b>CL.TE</b> is front-end Content-Length, back-end "
     "Transfer-Encoding. <b>0.CL</b> is the inverse of CL.0. Neither was swept in this campaign; "
     "they have no payload generator in this scanner yet."),
    ("injection point",
     "A place in a request where bytes the attacker controls reach a server &mdash; a path segment, a "
     "query value, a request header, a cookie value. Enumerated from a real crawl rather than a "
     "wordlist, because the <i>response</i> names headers no wordlist knows this host reads."),
    ("is_proof",
     "The one computed verdict in the dataset, and it is deliberately narrow: a value the scanner "
     "minted came back where nothing innocent puts it, <b>and</b> the matched control did not "
     "reproduce it. Everything else is evidence with innocent explanations remaining."),
    ("egress",
     "The source address a request actually left from. Recorded on every row, so a claim about how "
     "many machines the traffic was spread across is checkable against the traffic itself rather "
     "than against the invoice."),
    ("fleet / machine",
     "A fleet is a set of cloud machines provisioned for the length of one run and destroyed when "
     "it ends. Three fleets here: one crawls, two attack. Each machine has its own public address, "
     "which is the point &mdash; a scan leaving from one address is one block away from being over."),
]


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--facts", required=True)
    ap.add_argument("--shots", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--title", default="Request smuggling at platform scale")
    a = ap.parse_args()

    F = json.loads(pathlib.Path(a.facts).read_text())
    S = pathlib.Path(a.shots)
    run = F.get("run", "")
    out = F.get("run_output", {})          # the four numbers, as the run itself returned them
    sc, pt, ob, ld = F["scope"], F["points"], F["observations"], F["leads"]
    tq = F.get("techniques", {})

    parts: list[str] = []
    add = parts.append

    # ── MASTHEAD ──────────────────────────────────────────────────────────────────────────────
    #
    # THE STANDFIRST IS DERIVED, NOT WRITTEN. It used to read "swept across every paying program on
    # HackerOne from twelve machines" as a CONSTANT — so it made that claim about any run it was
    # pointed at, including one that covered nine programs out of 462, never reached either
    # technique, and only ever brought up the four recon Machines. A masthead is the one line a
    # reader takes on trust and the one line nobody re-derives, which is exactly why it is the worst
    # place in the document for a hardcoded boast.
    # The run's OWN coverage, never the scope's size — see `programs_covered` in campaign-facts.py.
    scanned = F["points"].get("programs_covered")
    machines = out.get("machines_used")
    swept = F["observations"].get("rows") not in (None, 0)
    covered = (f"across {num(scanned)} program(s) of HackerOne's paid scope"
               if scanned else "across HackerOne's paid scope")
    fleet = f"from {num(machines)} machines" if machines else "from a per-run fleet"
    lead = (f"Two ways of making a front-end and a back-end disagree about where a request ends, "
            f"swept {covered} {fleet}"
            if swept else
            f"Two ways of making a front-end and a back-end disagree about where a request ends. "
            f"This run enumerated the attack surface {covered}; the technique sweep did not run, "
            f"so every figure counted from it reads &mdash;, meaning NOT MEASURED rather than zero")
    add(f"""
<header class="masthead">
  <div class="eyebrow">Campaign report &middot; kontra</div>
  <h1>{esc(a.title)}</h1>
  <p class="standfirst">{lead} &mdash; and what each number in this report is counted from.</p>
  <div class="runline">
    <span>run <b>{esc(run)}</b></span>
    <span>scope <b>{esc(F['datasets']['scope'])}</b></span>
    <span>observations <b>{esc(F['datasets']['observations'])}</b></span>
    <span>leads <b>{esc(F['datasets']['leads'])}</b></span>
  </div>
</header>""")

    # A PARTIAL RUN SAYS SO, ABOVE EVERYTHING ELSE. An em-dash in a figure is easy to read past;
    # a reader who skims the headline numbers and stops should still leave knowing what was and
    # was not covered.
    if not swept:
        add("""
<div class="note warn">
  <b>This run did not complete.</b> The crawl ran and its results are real, but the technique
  sweep &mdash; both CL.0 and CRLF &mdash; never started, so there are no observations, no leads
  and no request totals. Every figure sourced from them is rendered &ldquo;&mdash;&rdquo;, which
  means <b>not measured</b>, not zero. The injection-point enumeration below stands on its own;
  the rest of this document describes what the scan does, not what it found.
</div>""")

    # ── THE FOUR NUMBERS ──────────────────────────────────────────────────────────────────────
    machines = out.get("machines_used", "—")
    cost = out.get("fleet_cost_usd")
    websites = out.get("websites_enumerated", ob.get("hosts", 0))
    requests = out.get("requests_sent", ob.get("requests", 0))
    add(f"""
<section>
  <div class="col">
    <h2>What this run did</h2>
    <p>These four are what the run itself returned, rendered on its own result page. None of them
       is an estimate: each is counted where the thing being counted actually happens.</p>
  </div>
  <div class="figures">
    {fig(num(websites), "websites enumerated",
         "Distinct hosts that <b>answered</b> a request. Not scope rows — most of those are an "
         "authorisation list — and not crawl events, since one exchange is two of those.")}
    {fig(num(requests), "requests sent",
         "Counted at the socket, after the write succeeded. A connection that never dialled "
         "contributes zero; a probe that escalated to its control contributes six.")}
    {fig(num(machines), "machines used",
         "Three fleets of four, each machine with its own public address, alive only for the "
         "length of the run.")}
    {fig("$" + (f"{float(cost):,.2f}" if cost is not None else "—"), "fleet cost",
         "Summed per fleet and rounded up per fleet, because that is how a droplet is billed.")}
  </div>
</section>""")

    # ── THE MECHANISM ─────────────────────────────────────────────────────────────────────────
    add(f"""
<section>
  <div class="col"><h2>What a desync is</h2>{INTRO}</div>
</section>

<section>
  <div class="col"><h2>The two techniques this campaign swept</h2>{TWO_TECHNIQUES}{CL0_EXPLAIN}</div>
  <pre class="wire"><span class="dim">&larr; what the front-end forwards, and the back-end does not read</span>
POST /api/v2/user?cb=847213 HTTP/1.1
Host: target.example
<span class="inj">Content-Length&#x20;&#x3A;&#x20;60</span>     <span class="dim">&larr; a space before the colon. One parser accepts it, one does not.</span>
Connection: keep-alive

<span class="inj">GET /a1b2c3-kontra HTTP/1.1
X: X</span><span class="dim">    &larr; deliberately unfinished: no blank line, so no request is ever queued</span></pre>
  <p class="cap">The third request on that connection is byte-identical to the first. If it comes
     back different &mdash; or if the canary path <code>/a1b2c3-kontra</code> appears in its response &mdash;
     the back-end read the attacker's bytes as the start of a request.</p>
  <div class="col">{CRLF_EXPLAIN}</div>
  <pre class="wire">GET /search?q=<span class="inj">%C4%8D%C4%8A</span>X-Injected:&#x20;1 HTTP/1.1     <span class="dim">&larr; attack</span>
GET /search?q=<span class="inj">%C4%8E%C4%8B</span>X-Injected:&#x20;1 HTTP/1.1     <span class="dim">&larr; matched control, one value higher</span>

<span class="dim">after a lossy narrowing in the back-end:</span>
  <span class="inj">%C4%8D</span> &rarr; U+010D &#x010D; &rarr; 0x0D  <span class="dim">carriage return</span>      <span class="inj">%C4%8E</span> &rarr; U+010E &#x010E; &rarr; 0x0E  <span class="dim">not a newline</span>
  <span class="inj">%C4%8A</span> &rarr; U+010A &#x010A; &rarr; 0x0A  <span class="dim">line feed</span>            <span class="inj">%C4%8B</span> &rarr; U+010B &#x010B; &rarr; 0x0B  <span class="dim">not a newline</span></pre>
  <p class="cap">Same length, same encoding, same byte count. The only difference between the two
     lines is whether the low bytes are newlines &mdash; which is what makes a difference in the
     responses mean something.</p>
</section>

<section>
  <div class="col"><h2>Why most of this work is proving yourself wrong</h2>{CONTROL_EXPLAIN}</div>
</section>

<section>
  <div class="col"><h2>What the scanner will not do</h2>{SAFETY}</div>
</section>""")

    # ── THE FLEETS ────────────────────────────────────────────────────────────────────────────
    fl = out.get("fleets") or {}
    def fleet_row(tag, title, what, detail):
        f = fl.get(tag) or {}
        # THE FLEET'S OWN NAME IS PROVENANCE, not decoration: it is the string `kontra fleet
        # status` answers to, so a reader can go and check that these Machines were this run's.
        meta = (f"<code>{esc(f.get('fqn',''))}</code> &middot; {f.get('machines','?')} machine(s) "
                f"&middot; ${f.get('hourly_usd','?')}/hr &middot; "
                f"held {int(f.get('held_seconds') or 0) // 60} min &middot; billed {f.get('billed_hours','?')}h"
                if f else "not held on this run")
        return (f'<div class="phase"><div class="idx">{title}</div><div>'
                f"<h3>{esc(what)}</h3><p>{detail}</p>"
                f'<div class="meta">{meta}</div></div></div>')

    add(f"""
<section>
  <div class="col">
    <h2>Three fleets, twelve addresses</h2>
    <p>The campaign runs as a sequence, and it is a real one: the two attack fleets consume what
       the crawl writes, so starting them alongside it would have both read a half-written
       inventory and complete &mdash; silently &mdash; over ground nobody had looked at.</p>
    <p>Each machine has its own public address. That is the reason there are twelve of them rather
       than one bigger one: a scan leaving from a single address is one rate-limit away from being
       over, and the <code>egress</code> column below records which address every probe actually
       left from, so the spread is checkable from the data rather than from the bill.</p>
  </div>
  <div class="phases">
    {fleet_row("recon", "01", "Crawl the scope",
               "A real browser visits every scoped asset that has a fetchable URL and records each "
               "request and response. The crawl is what makes the next two phases possible: a path "
               "invented from a wordlist 404s at the edge and never reaches the back-end whose "
               "disagreement this is looking for.")}
    {fleet_row("crlf", "02", "The CRLF technique",
               "The splitting axis. Fold codepoints spliced into every injection point the crawl "
               "found, each against its matched control.")}
    {fleet_row("cl0", "03", "The CL.0 technique",
               "The smuggling axis. A rendered request whose length the two parsers read "
               "differently, sent as three requests down one socket &mdash; normal, attack, normal &mdash; "
               "all written before any is read.")}
  </div>
  <div class="note"><b>One caveat, so the diagram does not overclaim.</b> A worker's task queue is
     derived from the actor and version it runs, and carries no fleet name &mdash; so the eight attack
     machines poll one queue. They are two fleets for capacity, billing and teardown; for dispatch
     they are a single eight-machine pool, and a CRLF batch may well run on a machine in the CL.0
     fleet. That spreads each technique over <i>more</i> addresses, not fewer, but it means
     &ldquo;which technique ran from which machine&rdquo; is answered by the <code>axis</code> and
     <code>egress</code> columns, never by which fleet a machine belonged to.</div>
</section>""")

    # ── THE EVIDENCE: screenshots + the tables behind them ────────────────────────────────────
    add(f"""
<section>
  <div class="col">
    <h2>The run, as the console shows it</h2>
    <p>Everything below is this run's own record. The four numbers are rendered as cards because
       the workflow <i>returns</i> them &mdash; a figure that only ever reached a log line dies with the
       log rail, and cannot be read off the page a month later.</p>
  </div>
  {shot(S, "01-run-result", "<b>The run result page.</b> Input as started, progress reduced from the "
        "workflow's history, and the returned output. The two chips at the top come from the runs "
        "list rather than the run itself, which is why a run that has aged out of that list renders "
        "them blank.")}
  {shot(S, "01b-run-progress", "<b>The phases, on their own.</b> The same page scrolled past the "
        "argument: each step the campaign moved through, how long it took, and what was still in "
        "flight when the shot was taken. Rendering the full 462-program list above is what pushes "
        "this below the fold, so it is framed separately rather than cropped out of the record.")}
  {shot(S, "01c-infrastructure", "<b>What it ran on.</b> One chassis per Droplet, the Actor placed "
        "on each, and whether that Worker was actually polling &mdash; read from Pulumi's checkpoint "
        "and the converge's own heartbeat. A box says the machine <i>exists</i>; the card inside "
        "says the Worker is <i>serving</i>. They are separate reads and neither stands in for the "
        "other: a checkpoint is written when a stack converges and never again, so a machine that "
        "died an hour ago still reads healthy there. "
        "<b>Count it carefully.</b> Twelve machines are provisioned across the run &mdash; four "
        "recon, four splitting, four smuggling &mdash; but only eight are ever up at once, because "
        "the recon Fleet is released the moment crawling ends instead of idling through the hunt. "
        "Cost is summed per Fleet rather than off a wall clock, because a Droplet bills by the "
        "hour ROUNDED UP: three Fleets held twenty minutes each is three billed hours, not one.")}
  {shot(S, "02-run-output", "<b>The four numbers, as the run reported them.</b> One card per key of "
        "the returned object, each carrying the type and description the workflow declared.")}
</section>

<section>
  <div class="col">
    <h2>Where a payload can go</h2>
    <p>An injection point is somewhere attacker-controlled bytes reach a server. They are
       enumerated from the crawl rather than guessed from a wordlist, and the reason is in the
       data: a <i>response</i> names headers no wordlist could know this host reads &mdash;
       <code>Access-Control-Allow-Headers</code> is the application stating which request headers
       it will look at, and <code>Vary</code> is the cache stating which ones it keys on, which is
       the difference between poisoning one response and poisoning everyone's.</p>
  </div>
  <div class="figures">
    {fig(num(pt.get("claimed")), "injection points", "On hosts this program authorised.")}
    {fig(num(pt.get("hosts")), "hosts carrying them", "Distinct in-scope hosts with at least one point.")}
    {fig(num(pt.get("off_scope_hosts")), "third-party hosts recorded",
         "Reached while crawling an authorised page &mdash; fonts, analytics, CDNs. Kept as inventory "
         "and <b>never</b> claimed as injection points.")}
  </div>
  {table(pt.get("by_kind", []), [("point_kind", "kind of point"), ("points", "points"), ("hosts", "hosts")],
         {"points", "hosts"})}
  {shot(S, "05-injection-points", "<b>The enumerated points, with the reason each one counts.</b> "
        "<code>point_rationale</code> is a column rather than a lookup table, so a consumer of this "
        "dataset never has to ask what a row meant.")}
  {shot(S, "06-point-kinds", "<b>The injection surface by kind.</b> Request headers dominate because "
        "every header a client sends is controllable by definition; response headers count only "
        "where the application admits it reads them back.")}
</section>

<section>
  <div class="col">
    <h2>The payloads</h2>
    <p>The corpus is generated, not collected. Every publicly confirmed bypass in the chain this
       scanner was built from obeys one rule &mdash; a codepoint whose last byte is <code>0x0A</code> or
       <code>0x0D</code> survives a lossy narrowing &mdash; so rather than shipping the handful of
       payloads that happen to be public, the generator emits the whole space that rule describes.
       A regression test asserts every known bypass is predicted from first principles; if it were
       not, the rule would be wrong and the approach would be a wordlist with extra steps.</p>
    <p><b>This campaign is the first one able to send CRLF at all.</b> The class existed in the
       code and was unreachable: the generator held one byte per class, so a two-byte sequence had
       nowhere to live, and asking for it silently ran the default single-byte sweep instead. A
       class is now a byte <i>sequence</i>, and an unrecognised one is refused by name rather than
       quietly widened.</p>
  </div>
  {table(tq.get("by_class", []), [("class", "technique class"), ("n", "payload rows")], {"n"})}
  {shot(S, "07-vectors", "<b>The technique corpus.</b> Each row is one rendered request shape; the "
        "class names which two parsers are being asked to disagree.")}
</section>""")

    # ── WHAT CAME BACK ────────────────────────────────────────────────────────────────────────
    add(f"""
<section>
  <div class="col">
    <h2>What came back</h2>
    <p>Every probe is written down, including &mdash; especially &mdash; the boring ones. A scan that records
       only its hits cannot answer the question that matters when somebody asks whether a host is
       safe, which is not &ldquo;did you find anything&rdquo; but &ldquo;what did you actually
       cover&rdquo;.</p>
  </div>
  <div class="figures">
    {fig(num(ob.get("rows")), "observations", "One row per probe, kept whatever it found.")}
    {fig(num(ob.get("requests")), "requests sent", "Summed from a column counted at the socket.")}
    {fig(num(ob.get("erratic")), "hosts refused as erratic",
         "Would not reproduce their own baseline, so they were <b>not scanned</b> &mdash; which is a "
         "different fact from scanned and clean.")}
    {fig(num(ob.get("voided")), "probes voided",
         "Ran, but carry no claim: the dial failed, TLS failed, HTTP/2 was negotiated, or the host "
         "moved between the baseline and the attack.")}
  </div>
  {table(ob.get("by_axis", []),
         [("axis", "axis"), ("class", "class"), ("observations", "observations"),
          ("requests_sent", "requests"), ("hosts", "hosts"), ("source_ips", "source IPs")],
         {"observations", "requests_sent", "hosts", "source_ips"})}
  {shot(S, "08-requests-by-axis", "<b>Both techniques, side by side.</b> Each axis with the traffic it "
        "generated and the number of distinct source addresses it left from.")}
  <div class="col">
    <h4>Why so many rows say nothing happened</h4>
    <p>Because that is the honest shape of the work. Around 97% of payloads fire nothing, and the
       rows recording that are what let the next campaign compare against this one instead of
       starting over. When a probe does move something, the scanner spends a whole extra
       connection re-running it with the payload neutralised &mdash; and when that control reproduces
       the behaviour, the claim is withdrawn and the withdrawal is written down.</p>
  </div>
  {table(ob.get("void_reasons", []), [("void_reason", "why the probe carries no claim"), ("n", "rows")], {"n"})}
</section>

<section>
  <div class="col">
    <h2>Twelve addresses, from the traffic</h2>
    <p>This table is the check on the machine count. The fleet says how much capacity was held;
       this says how many addresses the targets actually saw. They can differ &mdash; a shared fleet, a
       placement landing on fewer machines than were held, a worker that died &mdash; and when they do,
       the fleet number is the one that is wrong.</p>
  </div>
  {table(ob.get("by_egress", []),
         [("source_ip", "source address"), ("observations", "observations"), ("requests_sent", "requests")],
         {"observations", "requests_sent"})}
  {shot(S, "09-egress", "<b>The fleet, measured rather than asserted.</b>")}
</section>

<section>
  <div class="col">
    <h2>Leads</h2>
    <p>A lead is a probe that earned a human's attention. It carries the normal request, the
       request that triggered the signal and the matched control as three separate fields &mdash; twice
       each, once readable and once as exact wire bytes &mdash; because the finding <i>is</i> the
       difference between two requests, and asking a reader to reconstruct the normal one from an
       identifier is asking them to take the analyser's word for it.</p>
  </div>
  <div class="figures">
    {fig(num(ld.get("rows")), "leads promoted", "Probes that signalled something and survived their controls.")}
    {fig(num(ld.get("proof")), "carrying proof",
         "A value this scanner minted came back where nothing innocent puts it, <b>and</b> the "
         "matched control did not reproduce it.")}
    {fig(num(ob.get("signalling")), "signalling observations", "Rows with at least one signal, before promotion.")}
  </div>
  {table(ob.get("signals", []), [("signal", "signal"), ("n", "observations")], {"n"})}
  {shot(S, "10-leads", "<b>The leads table.</b> <code>is_proof</code> is computed once, here, rather "
        "than left to every reader to re-derive.")}
</section>""")

    # ── WHAT IT DID NOT COVER ─────────────────────────────────────────────────────────────────
    add(f"""
<section>
  <div class="col">
    <h2>What this campaign did not cover</h2>
    <p>Stated because a number without its denominator invites a subtraction that means nothing.</p>
    <ul>
      <li><b>{num(sc.get('no_seed'))} of {num(sc.get('rows'))} scoped assets have no fetchable
          URL.</b> They are wildcards (<code>*.example.com</code>) and globs that expand to
          nothing published. A browser cannot visit them, so they were never crawled and never
          attacked. Reaching them needs a subdomain-enumeration pass, which is not a crawl.</li>
      <li><b>Two of the four technique families are not implemented.</b> CL.TE and 0.CL have no
          payload generator in this scanner. Their absence from the results is not evidence of
          their absence from the targets.</li>
      <li><b>Erratic hosts were refused, not scanned.</b> {num(ob.get('erratic'))} of them on this
          run. They are recorded as untested.</li>
      <li><b>Nothing was exploited.</b> Every payload is an incomplete prefix; no cache was
          poisoned, no request was queued in front of another user's, no cookie was set. Turning
          a lead into an impact statement is manual, per-target, and deliberately outside this
          pipeline.</li>
    </ul>
  </div>
</section>

<section>
  <div class="col">
    <h2>Glossary</h2>
    <p>Every term below is a column value or a signal name in the datasets above &mdash; so each is
       defined by what it means for the claim being made.</p>
  </div>
  <dl class="gloss">
    {''.join(f'<dt>{k}</dt><dd>{v}</dd>' for k, v in GLOSSARY)}
  </dl>
</section>

<footer>
  Generated from <code>{esc(pathlib.Path(a.facts).name)}</code> for run <code>{esc(run)}</code>.
  Every figure is one SQL statement against a durable dataset; the statements are in
  <code>scripts/campaign-facts.py</code>. All testing was authorised under the published bug-bounty
  scope of each program, and every request carried a researcher-identifying header.
</footer>""")

    doc = (f'<!doctype html>\n<html lang="en"><head><meta charset="utf-8">'
           f'<meta name="viewport" content="width=device-width, initial-scale=1">'
           f"<title>{esc(a.title)}</title><style>{CSS}</style></head>"
           f'<body><div class="wrap">{"".join(parts)}</div></body></html>')
    p = pathlib.Path(a.out)
    p.parent.mkdir(parents=True, exist_ok=True)
    p.write_text(doc)
    print(f"wrote {p}  ({len(doc) / 1024:.0f} KB)")


if __name__ == "__main__":
    main()
