# foldscan

Detects CR/LF injection reachable through **lossy Unicode narrowing** — the bug class behind
six paid criticals on one 8x8 host between 2023 and 2025.

## The rule

Every confirmed bypass in that report chain obeys one rule:

> a codepoint whose **least-significant byte is 0x0A** (or 0x0D) survives a lossy 32→8 or
> 16→8 narrowing in the backend.

```
%0A          U+000A   raw                    %E0%AC%8A    U+0B0A   ଊ  (#3340283)
%C4%8A       U+010A   Ċ  (#2704607)          %E5%98%8A    U+560A   嘊 (#2095676)
%DC%8A       U+070A   ܊  (unreported)        %F0%9F%98%8A U+1F60A  😊
```

`+0x100` in the codepoint is `+4` in the UTF-8 lead byte, so the two-byte space is exactly
seven vectors — `C4 C8 CC D0 D4 D8 DC` — of which two were public. A blacklist can only
remove points from that set, which is why the same host paid out five times.

PortSwigger's flamer documents the same primitive ("a server decodes UTF-8 then truncates
codepoints mod-256") but delivers it as raw wire bytes in a header. foldscan delivers it
**percent-encoded in the path**, which reaches a different gadget: an application that
URL-decodes the path and reforwards it into a backend request line.

## Input: a unit, not a URL

Each unit is one crawled exchange — a request **and** the response it produced. The request
says where you *can* inject. The response says where you *should*, and names points the
request never mentions:

| Source | What it tells you |
|---|---|
| `Access-Control-Allow-Headers` | headers the application admits it reads |
| `Vary` | headers the **cache** keys on — the difference between poisoning one response and everyone's |
| `Set-Cookie` | cookies it reads back |
| response-only header names | proxy-set, therefore usually proxy-read |
| echoed request-header values | a known path from input to output |

No static wordlist can know this host takes `X-Tenant-Id`. That is the structural advantage
of a crawler-fed scanner, and it is why the input is a unit.

**Delivery differs by context**, so every point carries a rendering:

| Rendering | Contexts | Form |
|---|---|---|
| `percent` | path, query, cookie, form body | `%DC%8A` — the server unescapes |
| `wire` | header values and **names** | raw `DC 8A` — nothing unescapes a header |
| `json` | JSON string values | `\u070a` — the parser decodes, then the narrowing runs |

`%DC%8A` in a header value arrives as seven ASCII characters and tests nothing. Getting this
wrong produces a scanner that is busy and blind.

**Two-phase, so cost stays bounded.** Each point is screened with two high-prior vectors;
only a point that reacts earns the full sweep. On voapi that was 4 points → 1 live, saving
87 requests.

## Pipeline

Modelled on http-terminator, minus the Burp dependency, so each stage maps 1:1 onto an actor:

```
seeker ──▶ flamer ──▶ validator ──▶ investigator
  (the      generate    probe and     confirm with
  corpus    the vector  record        a canary
  analysis) space       signals
```

```
foldscan seed         -u URL                          # bootstrap a unit (crawler's job)
foldscan points                                       # what would be probed, and why
foldscan flamer       [-tier 1|2|3] [-class lf,cr] [-only ID,...]
foldscan validator    [-tier N] [-repeat N] [-rate DUR]  < targets.jsonl
foldscan investigator [-rate DUR]                        < observations.jsonl
foldscan summary                                         < observations.jsonl
foldscan schema
```

Every stage is a line-oriented JSON filter. `foldscan schema` prints the contracts:
`unit/v1` → `point/v1` → `vector/v1` → `observation/v1` → `confirmation/v1`.

## The four oracles

Ported from terminator's validator:

| oracle | what it does |
|---|---|
| **stability gate** | baseline sent N×; an erratic host is *refused*, not scanned (terminator blacklists erratic domains). It is a filter, not a proof: a host noisy at rate `p` survives `N` repeats with probability `p^N + (1-p)^N` — 28% at `p=0.4, N=3`. Raise `-repeat` on noisy scope. |
| **matched control** | `%DC%8A` vs `%DC%8B` — same length, same shape, only folding differs |
| **contamination** | repeats that disagree are the signal, whatever they contain |
| **response shape** | `dual_response`, `truncated_body`, `cl_mismatch`, `leaked_headers`, `unclosed_html` — terminator's `FindingType`, verbatim |

**The matched control is load-bearing.** Comparing `/%DC%8A` against `/` makes every target
that echoes the path look vulnerable — every reflective 404 on the internet. Against
`/%DC%8B`, path length, byte count and encoding shape are held fixed and exactly one thing
varies: whether the low byte is a newline. On the patched lab this took false positives
from **26 to 0**.

Two subtler corrections that the lab caught, both of which had silently pinned an oracle to
"always true":

- Body comparison redacts every representation of what we injected — encoded form,
  once-decoded form, wire bytes, folded byte — **symmetrically on both arms** and **in the
  body only**. Redacting `0x0A` from one arm and `0x0B` from the other erases header line
  endings on one side only.
- A canary counts as injected only when it appears in the fold arm and **not** in a control
  arm carrying the same canary through the same point. The earlier rule — "the canary must
  follow `://`" — was tuned to one target's error format and silently confirmed nothing
  anywhere else.

## Safety

The validator and investigator send **detection probes only**: a path that folds to a
newline, and at most one injected header. Neither completes a smuggled request, so neither
can poison a response queue or capture another user's traffic. Turning a confirmed finding
into that proof is a manual, per-target act. Default pacing is one request per second per
host.

## Lab

```
go run ./cmd/lab -mode vulnerable   # blacklists the reported payloads; every other fold lands
go run ./cmd/lab -mode patched      # never narrows; the false-positive gate
go run ./cmd/lab -mode erratic      # random statuses; the stability gate must refuse it
```

`lab` is a package, not just a binary, because killing a `go run` parent does not kill the
binary it spawned — a stale lab holding the port made three different modes report
identical results until the harness started verifying the mode before scanning.

## Tests

```
GOWORK=off GOTOOLCHAIN=go1.26.4 go test -race ./...
```

`TestRulePredictsEveryKnownBypass` is the load-bearing one: six payloads found by hand over
three years, one per patch cycle, must all fall out of the rule. If they don't, this is a
wordlist with extra steps.
