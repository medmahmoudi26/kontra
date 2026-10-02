# desync — HTTP desync and header-injection scanning

A kontra actor, and the packages it is built from:

    probe/    the connection primitive — dials a socket, writes exact bytes, records
              what came back. It does NOT decide what anything means.
    inject/   where a payload can go in a crawled exchange, and how it renders there
    vectors/  the fold space: every codepoint that truncates to a control byte
    detect/   sampling, the stability gate, and the signal analysis
    unit/     the input contract: one crawled request AND the response it produced
    lab/      a local reproduction with known ground truth (vulnerable/patched/erratic)
    cmd/      foldscan (the CLI) and lab (run the reproduction on fixed ports)

## Two rules

These are `probe`'s, and they set the shape of everything built on it.

**1. The unit of observation is a connection, not a request.** A desync is the
property that request N changes how the server reads request N+1. `Run` takes an
ordered list of steps — `baseline` → `mutated` → `canary` — puts all of them on one
socket, and attributes response bytes, timing and socket events to the step that
caused them. `ModePipelined` writes every step before reading any, which is the only
way CL.0 is observable: the smuggled prefix has to be in the server's buffer when the
next request lands.

**2. It emits observations, never verdicts.** Classification into CL.0 / splitting /
response-splitting happens later, in SQL, over the dataset. Re-scanning 37k hosts
costs hours and goodwill; re-querying a dataset costs nothing. Every column exists so
detection logic can improve without touching the network again — which only works if
the scanner never threw the raw bytes away. `StepObs.RespRaw` is populated *before*
any parse is attempted, and a parse failure with non-empty raw bytes is a signal, not
noise: it is what a stacked or truncated response looks like.

## Verified facts about github.com/sw33tLie/http

The fork's value is **entirely on the write path**. Verified by reading the source at
`v0.0.0-20251029225617-e8254e59a930`:

| | Status |
|---|---|
| `ValidHeaderFieldName` | patched to `return true` (`internal/httpguts/httplex.go`) |
| `ValidHeaderFieldValue` | patched to `return true`; original loop commented out |
| Header **value** newlines via the `Header` map | **still flattened** — `header.go:210` runs `headerNewlineToSpace` |
| Header ordering via the `Header` map | **still sorted** — `sortedKeyValues` |
| `MethodOnlyRequest` raw mode | package-level **global** (`request.go:1620`), not per-request |
| `ReadResponse` | **stock** — uses stdlib `net/textproto`, still rejects malformed status lines and header blocks |

Consequences, all locked by tests:

- Anything carrying a raw newline, or needing exact header order, must be written as
  raw bytes. Disabled validation buys the *name* path, not the *value* path.
  → `TestHeaderMapFlattensNewlines`
- Because the read path is stock, raw response bytes are the ground truth and parsing
  is best-effort. → `TestUnparseableResponseIsStillRecorded`
- `MethodOnlyRequest` being a global makes mixed-mode concurrency a race. This package
  sidesteps it entirely by owning the socket and never using `Transport`.

## The HTTP/2 result

`TestH2RawNewlineInHeaderValue` settles whether the h2 → h1 downgrade family is
reachable from Go. **It is.** A header value of `canary\r\nx-injected: yes` arrives in
the HTTP/2 HEADERS frame *byte for byte*:

```
wire value: "canary\r\nx-injected: yes"
```

`headerNewlineToSpace` lives only in `writeSubset`, which the h2 encoder never calls,
and the patched validators clear the way. So a frontend that downgrades h2 to h1
without re-validating will serialise that LF into a backend request line.

The test decodes the frame with a bare `hpack.Decoder` rather than
`Framer.ReadMetaHeaders`, because `ReadMetaHeaders` runs *unpatched* `x/net`
validation and would reject the exact value being measured.

## The test that matters most

`TestRawBytesReachTheWireVerbatim` byte-compares what left the socket against what was
intended, using every hostile construction from the report corpus in one payload
(space before colon, leading tab, `+`-prefixed value, a line with no colon, a bare LF
in a value). The failure it guards against is silent: a payload gets normalised
somewhere, the scan runs at full speed, and every target reports clean. Asserting on
status codes would never catch it.

## Not in scope here

Payload generation, header-name discovery, and classification. This package is the
thing they all sit on.

## Running

```
GOWORK=off GOTOOLCHAIN=go1.26.4 go test -race ./...
```

## As a kontra actor

`desync` is a kontra actor (`actor.json`, `main.go`), so the same scanner that `foldscan`
drives from a terminal runs across a fleet. The two share `detect`/`inject`/`vectors`
deliberately: a vector you debug with the CLI is the vector the fleet sends.

    kontra serve --actor examples/go/desync --engine go --tmux
    kontra dataset create exchanges.jsonl exchanges

…then call it from a workflow you serve (there is no dispatch verb — ADR 0023 §12):

```python
async for batch in catalog.dataset("exchanges").batches(100, order_by="url"):
    obs, dropped = await desync.scan(
        batch, out,
        params={"tier": 1, "class": "lf", "max_points": 40, "rate_ms": 1000},
    )
```

**Input is a crawled exchange, not a URL** — the `unit.Unit` above. Its natural upstream is
`webcrawl`, which emits one object per HTTP request and response; feed one's output Dataset to the
other and the response half starts naming injection points (`Access-Control-Allow-Headers`, `Vary`,
`Set-Cookie`) that no wordlist knows about.

**Output is one `detect.Observation` per probe**, emitted on its Unit the moment it exists rather
than returned as a list — a scan of one exchange can produce hundreds, and streaming makes each one
durable as it happens. Classification stays in SQL over the dataset, per rule 2 above.

Params mirror the `validator` flags: `tier`, `class`, `only`, `points`, `max_points`, `screen`,
`no_screen`, `repeat`, `screen_repeat`, `rate_ms`, `timeout_ms`, `concurrency`.

Two properties worth knowing before you point it at real hosts:

- **`rate_ms` is per HOST and the pacer is shared across every Unit in flight.** Crawler-fed
  input puts many exchanges on one host, so raising `concurrency` widens the sweep across
  DIFFERENT hosts rather than hitting one harder. (`detect.Scanner` is safe for concurrent use
  for exactly this reason; a scanner per Unit would pace each one independently and quietly
  multiply the rate by the concurrency.) The Method owns that window itself — the framework
  stopped owning it in ADR 0023 §18.
- **A capped enumeration is emitted, not dropped.** `max_points` bounds cost, and the units it
  drops are reported as an observation — a bounded sweep that reads as complete is how a scan
  says "clean" about ground it never covered.
