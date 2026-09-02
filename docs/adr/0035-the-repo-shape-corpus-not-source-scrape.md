# 35. The repo shape: `runtime/` imports `sdk/`, and a contract with two writers gets a corpus

## Status

**Accepted, 2026-08-27.** Records the directory shape the architecture-v2 program moves to, and the
two rules that make the shape something other than a rename. It changes no code. Like **0024**, what
it changes is what a reader is entitled to believe — and, new here, what a *test* is entitled to
assert.

Supersedes no ADR. It leans on **0023** §17 for what a **Batch**'s content hash is and why a
committed **Unit** is keyed by it; on **0029** §3/§5 for the retention TTL the second measurement
below is taken against; on **0031** §2/§6 for the appliance's artifact layout, which §5 carves out
of this restructure explicitly; and on **0024**'s carried-forward `CONTRACT_VERSION` debt — "still a
decision, still not implemented; both wire strings remain independent literals" — which is the
oldest instance of the problem §3 generalises, and which `contract/` is the place to pay.

**The first move is already landed.** `orchestrator/src` became `backend/` and `orchestrator/web`
became `frontend/` in `25e5262`. The rest of §1 is in flight. That ordering matters to this record
rather than merely dating it: the landed commit produced finding 3 below, which is the sharpest
evidence for §3 in the repo, and it produced it within one commit of the restructure starting.

## Context

The architecture survey found nine things in the TypeScript trees and nine in the Go and Python
trees, and they reduce to one shape. This repo's cross-seam contracts are pinned by hand-copied
literals and by tests that read another language's *source text* and grep it. That works exactly as
well as the coincidence it rests on, which is that the file is where the test remembers it and the
literal is spelled the way the test remembers it — neither of which is the contract.

Six things were established against the tree before deciding. **Three of them by execution rather
than by reading**, and those three are the ones that carry weight, because every one of them
contradicts a comment already in the tree that says the opposite.

### 1. `batch_id` and `BatchID` disagree today, and the golden that guards them is pinned on the one input class where they cannot

Both SDKs derive a **Batch**'s content hash the same way on paper — canonical JSON of
`[method, units, params]`, sorted keys, no whitespace, sha256, first 16 hex characters
(`actorkit/python/internals/engine.py:124`, `actorkit/go/internal/engine/engine.go:236`). They
disagree in fact, because each language's default JSON encoder escapes a different set of
characters. Go's `encoding/json` HTML-escapes `<`, `>` and `&`; Python's `json.dumps` defaults to
`ensure_ascii=True` and escapes every non-ASCII rune.

Executed, method `probe`, one **Unit**, empty params:

| Unit value | Python `batch_id` | Go `BatchID` |
|---|---|---|
| `https://acme.com/?a=1&b=2` | `d8868b6200c19340` | `6250da42654ec10a` |
| `café` | `4fc02e4dd607c61d` | `d0de34ff30eafbc4` |
| `<script>` | `7e16c1cbdab1e24b` | `6513f87dedc4a622` |
| `plain-ascii` | `76457373b3bbfe8c` | `76457373b3bbfe8c` |

Go's payload for the first row is `["probe",["https://acme.com/?a=1&b=2"],{}]`; Python's is the
same bytes with a literal `&`. Three of this repo's four canonical inputs diverge, and the fourth —
a bare ASCII token with no punctuation — is the only one that agrees.

That fourth row is what the golden is made of. `actorkit/go/internal/engine/statekey_congruence_test.go`'s
`TestTheBatchHashMatchesThePythonPeer` pins `{"url": "a"}`, `{"url": "b"}` and `{"depth": 2}` —
plain ASCII, no symbols, no query string — and both encoders still produce `e52b49473394dd1d` and
`461d1ef077c54fe5` after the divergence above is demonstrated. Its own comment says "a drift in
either encoder shows up here." It cannot. It is pinned inside the one input class where the two
encoders are incapable of differing, while **a URL with a query string is this repo's canonical
Unit** — `&` is in the seed list of every campaign this repo has run.

**The honest bound, which the test states itself and which this ADR carries forward rather than
dropping:** byte-equality here is not a wire contract. One **Batch** is served by one process in one
language, so the hash does not cross the SDK boundary today, and nothing is presently broken by the
divergence. What is broken is the *guard*: a test advertising protection it does not provide is
worse than no test, because it is the reason nobody writes the one that would.

### 2. The retention TTL guard is one literal deep, and the browser's copy is not guarded at all

Executed against `control/orchestrator/src/data/retention.ts`. Changing `DATASET_RETENTION_TTL_MS` from
`24 * 60 * 60 * 1000` to 48 hours fails **exactly one test** — `control/orchestrator/src/data/retention.test.ts:82`,
`expect(DATASET_RETENTION_TTL_MS).toBe(24 * HOUR)` — out of 28 in that file. Every other assertion
in the suite derives its expectations from the constant, so all 27 of them follow the edit wherever
it goes.

Then make the edit anyone would make next: update the literal in the test to `48 * HOUR`. The file
goes green at 28 of 28. And the browser's independent copy —
`frontend/src/datasets/expiry.ts:DATASET_TTL_MS = 24 * 60 * 60 * 1000` — is untouched, unnoticed and
unasserted-against: the whole frontend suite runs **123 files, 2,115 tests, all passing**, with a
48-hour server and a 24-hour browser telling an operator different things about the same **Dataset**.

`expiry.ts`'s own header already predicted this in the sentence it opens with: "Two implementations
of one policy is a drift with no loud failure mode, so the drift is made cheap to see: same words,
same order, same constants, and a comment on each side pointing at the other." Comments on each side
pointing at the other is the mitigation this repo reaches for, and it is exactly the mitigation
**0024** already rejected once for ADR staleness: a note is a thing you have to read *and* believe.

### 3. The restructure's own first commit turned fifteen source scrapes red — and not one of them because a contract moved

`25e5262` renamed a directory. It renamed nothing that any test in this class is *about*. Every
queue name, workflow type, stack project, tag regex, credential field and activity name those tests
pin is still spelled exactly as it was. Fifteen of them failed anyway, all with `FileNotFoundError`,
because five Python files resolve their target as `ROOT / "orchestrator" / "src"` with no guard:

- `tests/test_fleet_client.py` — 10 tests (the infra queue, `stackWorkflow`'s exported name,
  `FLEET_PROJECT`, `DATASET_QUEUE`, the resolved bundle's controller, the activity names, `TAG_RE`,
  `KONTRA_MAX_PARALLEL_SESSIONS`, `SECRET_NAME_RE`, the credential field's spelling)
- `tests/test_actor_probe.py`, `tests/test_hitl_ask.py`, `tests/test_dispatch_summary.py` — 1 each
- `tests/test_narration.py` — 2

A test that fails when the contract is intact and the directory moved is not testing the contract.
It is testing the directory — and §1 has five more directory moves to make.

### 4. The same commit turned one scrape permanently green, which is the worse half

`cli/appliance/kv_clients_test.go:68` still builds `filepath.Join(root, "orchestrator")` and stats
`orchestrator/node_modules/ioredis`; the skip message on the next line already reads
"`control/orchestrator/node_modules/ioredis` is not installed (pnpm install)". `TestRealStateStoreReadsThisStore`
therefore skips unconditionally and forever, and the esbuild invocation below it targets
`orchestrator/src/stateStore.ts`, which no longer exists. It is green in CI. Fifteen loud failures
are a morning's work; one silent permanent skip is a guarantee nobody knows they lost, on the one
test that proves the embedded KV is readable by the real client (**0031** §1).

### 5. The corpora exist, and this repo already drives them correctly four times

The pattern that works is in the tree, four times over, and in every case the fixture is *data both
sides execute* rather than text one side reads:

| Corpus | Runners |
|---|---|
| `shared/conformance/codec/fixtures.json` | `actorkit/python/internals/test_codec_conformance.py`, `control/orchestrator/src/codec/conformance.test.ts`, `actorkit/go/internal/codec/conformance_test.go`, `runtime/handler/internal/codec/conformance_test.go`, `cli/appliance/codec_test.go` — five runners, three languages |
| `actorkit/conformance/catalog.json` | `tests/test_catalog_conformance.py`, `control/orchestrator/src/catalog.conformance.test.ts` (against the real Fastify server), `actorkit/go/internal/registrar/conformance_test.go` |
| `actorkit/conformance/blobkey.json` | `actorkit/python/internals/test_blobkey_conformance.py`, `control/orchestrator/src/codec/shard.test.ts`, `actorkit/go/internal/unitstore/conformance_test.go`, `cli/appliance/s3_test.go` |
| `actorkit/conformance/output_dataset.json` | `tests/test_output_dataset_conformance.py`, `actorkit/go/internal/engine/output_dataset_conformance_test.go` — **Python and Go only; there is no TypeScript arm** |

Against those four sit roughly **twenty tests that assert on the literal text of another language's
source file** — 15 Python-greps-TypeScript (finding 3), 2 Python-greps-Go, 2 TypeScript-greps-Python
(`control/orchestrator/src/transcript.test.ts`, `control/orchestrator/src/hitl.test.ts`, both reading
`actorkit/python/lib/hitl.py` for `ASK_MEMO_PREFIX`), and 1 Go-greps-Python
(`cli/appliance/kv_test.go:TestScriptDigestsMatchBothSDKSources`, which extracts the Lua bodies out
of `redis_kv.py` and `rediskv.go` by string-slicing between delimiters and byte-compares them). Plus
one test that greps **its own module**: `actorkit/go/internal/temporalhost/host_test.go`'s
`TestServeInstallsTheClaimCheckCodec` does `os.ReadFile("host.go")` and asserts the substrings
`DataConverter:` and `codec.DataConverter` appear in it.

Two of these deserve naming, because they are the ones where the corpus is not merely available but
*already the thing being compared*. The Lua scripts ARE bytes with a SHA1 the fleet sends as
`EVALSHA` — a fixture holding those bytes is strictly better than a string-slice between
`_CAS_LUA = """` and `"""`. And the claim-check codec, whose installation `host_test.go` greps its
own source for, is the single most conformance-covered thing in this repo: five runners over
`shared/conformance/codec/fixtures.json`. The test's own comment says so ("The format is already pinned by
the cross-language corpus"), and then reaches for `strings.Contains` anyway, because what it wants
to know — did `Serve` dial with the converter — was easier to grep than to dial.

### 6. The one-way arrow is already a rule in this repo, written as a comment, enforced by nothing

`actorkit/python/lib/contract.py`'s header states it outright: "`nexusrpc` is imported at module
scope here, which is why this module is imported LAZILY from `workflows.py`: `import actorkit` must
stay free of a Temporal dependency, the same rule `lib/actor.py` follows."

Python holds it, by hand. Every crossing from `lib/` into `internals/` is deferred into a function
body — `from internals.temporal.host import serve` at `actor.py:625`,
`from internals.temporal.wfhost import serve_workflows` at `catalog.py:1980`,
`from internals.schema import …` inside two `hitl.py` functions — and the only module-scope crossing
is `testing.py:22`'s `from internals.batch import Batch, Dataset`, which is safe by accident:
`internals/batch.py` imports `hashlib` and `typing` and nothing else.

Go does not hold it. `actorkit/go/lib/serve.go:9` imports `internal/temporalhost` at package scope,
and that package imports `go.temporal.io/sdk/activity`, `/client` and `/worker`. So Temporal is in
the Go author surface's transitive graph right now. `serve.go`'s own comment is already policing the
import graph by hand — "kept local so `lib/` imports only `internal/temporalhost` + `internal/core`,
not a shared identity package" — which is the tell: someone is maintaining an invariant that nothing
checks, in a comment, in the file that violates it.

## Decision

### 1. The shape

| Directory | What belongs in it | Was |
|---|---|---|
| `backend/` | the orchestrator's TypeScript: API, workflows, activities, materializer, data plane, infra host | `orchestrator/src` — **landed** (`25e5262`) |
| `frontend/` | the SPA and the design system it draws with | `orchestrator/web`, absorbing `ds-bundle/` — **landed** |
| `contract/` | types and constants only, **zero runtime dependencies** — the things two or more seams must agree on | NEW |
| `sdk/` | the author surface: `catalog`, `ask`, `speak`, `dataset`, `fleet`, `probe`, `secrets` | `actorkit/*/lib` |
| `runtime/` | the engines, the codecs, the state stores, the Temporal hosts | `actorkit/*/internal(s)` |
| `handler/` | the Go **Actor** host, the codec server, the CAS | unchanged |
| `cli/` | the `kontra` binary and the appliance sub-packages (`kv`, `objstore`, `registry`, `bundle`, `temporalsrv`) | unchanged |
| `proto/` | the `.proto` files buf owns | `contracts/kontra` |
| `conformance/` | **the one cross-language gate** — every corpus, in one place | `shared/conformance/codec` plus `actorkit/conformance`, today split |
| `examples/ docs/ scripts/ infra/ tests/` | unchanged | unchanged |

`build/`, `tmp/` and `node_modules/` are gitignored, and already are (`.gitignore:8`, `:32`, `:34`).

Two entries are doing work beyond tidiness and should be read as decisions rather than as placement.
**`contract/` is where a value with two readers goes**, and it exists because §3's rule needs somewhere
to put the values that have no business being copied at all: a constant one package imports is not a
contract with two writers, it is a constant, and it needs no corpus and no scrape. `CONTRACT_VERSION`
— **0024**'s oldest carried-forward debt, "both wire strings remain independent literals" — is the
first thing that belongs there, and the retention TTL of finding 2 is the second within one language.
**`conformance/` is one directory because it is one gate**: the corpora are split across
`shared/conformance/codec/` and `actorkit/conformance/` today, and a contributor who has to know which of
two trees holds the fixtures is a contributor who writes a scrape instead.

### 2. Rule one: `runtime/` may import `sdk/`. `sdk/` may import nothing of `runtime/`.

`sdk/` is what an author writes against. Its transitive import graph must contain **no Temporal, no
Redis and no object store** — not the clients, not the types, not a module that pulls one in three
hops down. `runtime/` depends on `sdk/` and never the reverse.

This is the sentence that separates the restructure from a rename, and it is the one thing §1 claims
that a directory listing cannot verify. A one-way arrow asserted by a README stops being true on the
first import somebody adds in a hurry, and it stops being true *silently* — the code runs, the tests
pass, and the only symptom is that a year later `import actorkit` costs four seconds and drags a gRPC
client into a workflow sandbox that was supposed to be a passthrough.

**So a test per language enforces it, and the test is the decision.** Python asserts that importing
the `sdk` package and walking `sys.modules` yields no `temporalio`, no `redis`, no `boto3`/`aiobotocore`;
Go asserts it over `go list -deps ./sdk/...`, which is the compiler's own answer and not a heuristic;
TypeScript asserts it over the resolved import graph of the package entrypoint. Three tests, one
sentence each, in the three languages that have an author surface. There is no version of this rule
that survives as prose, because finding 6 *is* this rule, written as prose, in the tree, already
violated in one of the two languages that state it.

**What the rule costs, stated now so the slice that hits it does not re-derive it.** `serve` is the
crossing. An author calls it to start a host, so the author surface has to name a thing that lives
in `runtime/`, and today Go pays for that with a package-scope import that puts Temporal in `sdk/`'s
graph while Python pays with a deferred import inside a function body. Python's answer is the one
that satisfies the rule and it is the one that generalises: the entrypoint resolves `runtime/` at
call time, not at import time. That is a real constraint on how `serve` is written in Go, it is not
free, and the test is what makes choosing it non-optional. It is left open below only in the sense
that the mechanism is the implementing slice's to pick — the arrow is not.

### 3. Rule two: a contract with two writers gets a corpus, not a source scrape

**A cross-language or cross-package contract is gated by a fixture that both sides execute.** Not by
a test that reads the other side's source and greps it. Not by a copied literal with a comment
pointing at its twin.

The corpus is the file. Both implementations load it, run it, and compare their answer to the answer
recorded in it. It is language-neutral by construction, it fails on the machine of whichever side
drifted, and it cannot be satisfied by a file living where a test remembers. Finding 5 is four
working examples of this, one of which drives five runners across three languages, so nothing here is
new machinery — it is naming a pattern the repo already has and declaring the alternative a defect.

**A source scrape is a finding, not a pattern.** Every one of the ~20 in finding 5 goes, and what
replaces it is decided by what the test was actually trying to know:

- **A value both sides must spell the same** — a queue name, a workflow type, a memo prefix, a signal
  prefix, a regex, a TTL. If it has one writer, it moves to `contract/` and is imported. If it
  genuinely has two writers because the languages cannot share code, it becomes a row in a corpus
  both sides read: each side asserts *its own* derivation against the fixture, and neither side ever
  opens the other's file.
- **A derivation both sides must compute the same** — a **Batch** hash, a blob key, a session queue
  name, an EVALSHA digest. This is what a corpus is *for*, and finding 1 is what happens when the
  corpus is present but its rows were chosen to be easy. **A corpus is only as good as its adversarial
  rows.** The `batch_id` corpus this ADR requires carries `&`, `<`, `>` and non-ASCII on the first
  four rows, because those are the four inputs on which the two encoders provably differ, and a
  fixture that omits them is finding 1 again with a new file name.
- **A behaviour one side must have** — "`Serve` dials with the claim-check codec". Execute it. A test
  that greps `host.go` for `DataConverter:` passes on a commented-out line and fails on an equivalent
  refactor; a test that boots the host and inspects the client's converter answers the question that
  was asked. `control/orchestrator/src/secrets/actorFetch.conformance.test.ts` and `slotFetch.conformance.test.ts`
  already do the harder version of this — they boot the real Fastify server and shell out to the real
  `actorkit.secrets` under `.venv/bin/python` — so "execute the peer" is also a pattern this repo has
  rather than one it needs.

### 3a. The one exception, and what earns it

**A security invariant that no type can express may be pinned by reading source, and nothing else
may.**

The exemplar is `control/orchestrator/src/panels/readonly.test.ts`, and it is the exception because it argues its
own case in the header rather than assuming it:

> ADR 0020's finding (3) is the reason this test exists rather than a comment: through a **read-only**
> (`-r`) tmux client, both `run-shell "touch …"` and `send-keys` into an interactive shell EXECUTED,
> as the Machine's root, and tmux returned no error. `-r` gates a client's keystroke handling, not
> tmux commands. So "read-only" can never be a property of tmux — it is a property of this code,
> which means it is only true for as long as nobody adds a write.
>
> A grep is a blunt instrument, and that is the point: the invariant it guards ("no route, no message
> type, no code path can put bytes into a session's channel") is not something a type can express, and
> the next person to reach for `send-keys` should have to delete a test that explains why they must
> not.

Three things earn it, and a future scrape must carry all three or it is a finding:

1. **The invariant is a negative over an open set** — *no* code path writes. A corpus enumerates
   cases; a negative over every future file cannot be enumerated, which is why this test reads the
   whole directory and asserts the file list is complete rather than reading a fixed set.
2. **No type can express it.** Not "no type currently does" — no type can. A `send-keys` string is a
   `string`.
3. **The measurement is stated.** The reason `-r` is insufficient is a thing somebody ran, not a
   thing somebody feared, and it is written down where the next reader meets it.

`frontend/src/panels/widgets/untrusted.test.ts` (no raw HTML, no fetch, mermaid only behind the lazy
import) and `control/orchestrator/src/panels/attach.test.ts` (no writable stdin to a **Machine**) are the same
class and are kept on the same terms. Everything else in finding 5 is not.

One correction while the exception is being written down: the header's closing line, "`cli/fleet.go`'s
own suite pins its program the same way, by reading the source", names the wrong file. `cli/fleet_test.go`
reads no source at all; the suite it describes is `control/orchestrator/src/infra/fleet.test.ts`. A stale citation
inside the record of the one legitimate exception is the smallest possible instance of this ADR's
whole subject, and it is left standing here as a citation rather than fixed silently in a document
that changes no code.

### 4. What is NOT changing — the compose service names

`orchestrator-api`, `orchestrator-infra` and `orchestrator-probe` keep those names. They are **DNS
names and Docker labels**, not paths, and nothing about §1 touches them:

- `cli/scale.go:435` sets a served worker's control-plane address to `http://orchestrator-api:8088`,
  and `scale.go:389` names the whole compose plane by those hostnames — a worker on the fleet resolves
  that string over the VPC.
- `cli/db.go:240` finds the running container by `label=com.docker.compose.service=orchestrator-api`.
- `control/orchestrator/src/queues.ts` and `control/orchestrator/src/main.ts` describe the three roles by those names, and
  **0031** §1's collapse table is keyed on them.
- `orchestrator-infra` and `orchestrator-probe` are live services in `docker-compose.yml`;
  `orchestrator-api` is the appliance's supervised child since **0031** §1 and is still the name in
  every address and label above.

A directory named `backend/` serving a container named `orchestrator-api` is not an inconsistency to
tidy. The directory names the *code*; the service names the *role*, and the role is still the
orchestrator.

### 5. What is NOT changing — the appliance bundle's on-disk layout

The staged tree is `<stage>/orchestrator/dist`. The hydrated tree is `<data>/orchestrator/<digest>`.
The manifest says `Bundle: "orchestrator"` and `Entrypoint: ["node/bin/node", "orchestrator/dist/src/main.js"]`.
The SPA lands at `<root>/orchestrator/web/dist`. None of it moves.

**This is a deployment artifact contract, not a directory.** `runtime/handler/internal/hydrate` reads the
manifest back and lays the tree down from it (`runtime/handler/internal/hydrate/integrity_test.go:327` builds
`dest/orchestrator/web/dist`); `cli/appliance/bundle/hydrate.go:141` roots the hydrated tree at
`<data>/orchestrator/<digest>` precisely so that "which bundle is this" is answered by a path.
**0031** §2 makes the digest the identity and makes a mismatch re-hydrate rather than proceed — so
renaming `orchestrator/` to `backend/` inside the bundle changes **every bundle digest that has ever
been built**, invalidates every hydrated tree on every machine, and buys a directory name that is
still accurate, because the bundle holds the orchestrator ROLE and not the `backend/` source tree.

`25e5262` already demonstrates the split and is the model to follow: `stageOrchestrator` now reads
its source from `filepath.Join(opts.RepoRoot, "backend")` and still writes it to
`filepath.Join(opts.StageDir, "orchestrator", "dist")`. Source path moved, artifact path did not.
Anyone tempted to "finish the rename" inside `cli/appliance/` should read this section first.

## Consequences

- **Half of what this ADR decides is checkable by a directory listing and half is not.** The listing
  is the cheap half and it is done in one commit per directory. §2 and §3 are the half that needs a
  test each, and a restructure that lands §1 without them has performed a rename and called it an
  architecture. The order in the program is deliberate: the rules land with the moves, not after
  them.
- **Fifteen currently-red tests are not fixed by repointing them.** Finding 3's tests fail on a path.
  Repointing `orchestrator/src` to `control/orchestrator/src` makes them green in an afternoon and reinstates
  exactly the coupling that broke them, one directory move before the next one. Each is triaged into
  §3's three buckets — a `contract/` import, a corpus row, or an executed assertion — and the ones
  that turn out to pin a value with a single writer simply disappear, which is the correct outcome
  and the reason the count goes down rather than across.
- **The `batch_id` corpus is new work with a known first failure.** Writing it fails immediately: the
  first row with an `&` in it disagrees across the two SDKs today. That is not a reason to soften the
  rows; it is the corpus doing its job on its first run, and the divergence it exposes has to be
  resolved (one encoder is made to match the other, and which one is not this ADR's call) before the
  gate can be green. Finding 1's bound stands until then — nothing in production is presently wrong,
  because the hash does not cross the boundary. The exposure is one Go caller away, since **0023**
  §22's reversal gave Go the caller's side.
- **`sdk/` gets a dependency budget, and the budget is enforced at build time.** Anything an author
  imports is now something three tests have an opinion about. The practical effect is that new
  convenience in the author surface has to justify what it drags in, which is the intended friction:
  the author surface is the one part of this repo that a user's `import` statement pulls into their
  own process.
- **A corpus is a file two languages both have to change together, which is a cost.** Adding a row
  means adding it to a fixture and rerunning three or five suites, versus adding a `strings.Contains`
  to one. That asymmetry is why the scrapes exist and it is not going away; the ADR's answer is that
  a cheap test of the wrong thing costs more, and finding 2 is what that costs looks like — a
  server and a browser disagreeing about a **Dataset**'s life, both green.
- **`conformance/` becoming one directory moves fixtures that four suites resolve by relative path.**
  Mechanical, and it is the kind of move that finding 3 says will break scrapes rather than corpora:
  a corpus runner reads a path, so it fails loudly at the fixture load, on every arm, at once.

## Considered and rejected

- **Keep `orchestrator/` and add the two rules.** This was available and it is the smaller change.
  Rejected because `orchestrator/` holding both the server and the browser is what let
  `frontend/src/datasets/expiry.ts` and `control/orchestrator/src/data/retention.ts` be "the same package" in
  everyone's head while being two deployment artifacts in fact — which is finding 2's precondition,
  not merely its setting.
- **Repoint the fifteen scrapes at `backend/` and move on.** Rejected above and worth restating as a
  rejection: it is the change that makes the symptom go away and leaves the mechanism, and the next
  directory move in this very program re-breaks them.
- **Ban source scrapes with no exception.** Rejected on `readonly.test.ts`, which guards a negative
  over an open set of files that no type can express, and which argues that case in its own header
  with a measurement. A rule with no exception would have deleted the best test in this class, and a
  rule whose exception is "when it feels justified" is not a rule — hence §3a's three conditions.
- **Make `sdk/` a published package with a dependency manifest and let the package manager enforce
  the arrow.** Attractive in TypeScript, unavailable in the other two: Go's `internal/` visibility is
  a directory rule that says nothing about what `lib/` may import (finding 6 is a legal Go program),
  and Python has no manifest that constrains a function-scoped import. A test per language is the
  only mechanism all three have.
- **Rename the appliance bundle's internal layout to match `backend/`.** Rejected in §5. Every bundle
  digest changes, every hydrated tree on every machine is invalidated, `runtime/handler/internal/hydrate` has
  to be versioned across the change, and the name being replaced is not wrong.
- **Rename the compose services to match the directories.** Rejected in §4. They are addresses a
  remote worker resolves and labels a CLI filters on; the blast radius is the fleet and the reward is
  a spelling.

## Open

- **Where `serve` lives, mechanically.** §2 fixes the arrow, not the technique. Python's deferred
  import satisfies the rule today; Go needs an equivalent, and "an equivalent" is one of a
  registration hook, an `sdk/serve` shim whose body resolves `runtime/` at call time, or moving the
  entrypoint out of the author surface entirely. The test is written first, in every case.
- **Which encoding the two sides converge on.** The two escape *different* things — Go escapes `<`,
  `>` and `&`, Python escapes every non-ASCII rune — so neither output is a subset of the other and
  "make Go match Python" is as much a two-sided edit as its mirror. The one canonical form both can
  actually reach is minimal escaping: Go's `Encoder.SetEscapeHTML(false)` and Python's
  `ensure_ascii=False`. That changes the hash of every **Batch** carrying `&`, `<`, `>` or a
  non-ASCII byte, and a **Batch** hash is a commit key (**0023** §17) — so a live **Session**
  resuming across the change resumes into a different slot, and **Units** already committed under the
  old hash are re-run rather than skipped. Not decided here; what is decided is that the corpus
  exists and carries the rows that force the decision.
- **Whether `output_dataset.json` gains a TypeScript arm.** It has Python and Go runners and no TS
  one, which is a two-of-three corpus in a repo where the caller's side exists in both those
  languages and the orchestrator reads the result. Bounded, cheap, and not scoped into this program.
- **What `contract/` may contain in a polyglot repo.** "Types and constants, zero runtime deps" is
  unambiguous in TypeScript and needs a shape in Python and Go, where a shared constant across three
  languages is itself a contract with three writers — which by §3 makes it a corpus, which makes
  `contract/` a TypeScript-side package with a conformance corpus behind it rather than a fourth
  copy. Stated here because it is the first thing the implementing slice will hit and the last thing
  that should be improvised.
