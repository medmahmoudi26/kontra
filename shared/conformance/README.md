# conformance/

A **corpus** is a JSON file of inputs and expected outputs that every implementation of one
contract executes. This directory is all of them.

## Why a corpus and not a test each side writes

A contract with two writers and no registration step drifts, and the drift has no loud failure
mode — which is the whole finding behind ADR 0035. Two things this repo has done instead, and what
each one cost:

**A hand-copied golden.** `runtime/go/engine` pinned the Batch content hash against two digests
computed from the Python peer. Both were plain ASCII with no symbols — the one input class where
Go's HTML escaping and Python's non-ASCII escaping *cannot* differ — while a URL with a query
string is this repo's canonical Unit. Its own comment claimed "a drift in either encoder shows up
here". It could not, and the two SDKs had been hashing the same Batch to different keys.

**A source scrape.** Around twenty tests assert on the literal TEXT of another language's source
file. They break on any refactor that preserves behaviour, and they pass while the *values* drift,
because a substring is not a value. The one honest exception is a security invariant no type can
express; ADR 0035 rule two names it.

**A COUNT IN A COMMENT.** The Actor's task queue was derived in eight places across four
languages, and three of those places carried a comment telling the reader how many peers there
were and where they lived. They said "a fourth", "THE FIFTH" and "a SIXTH", none of them counting
the same set, and one named a peer file that had been renamed out of the tree. A count is a fact
about the repo that only a human can check and only a human can update — which makes it the least
reliable kind of documentation there is, and it silently made the CLI's derivation, which had no
test at all, look covered. `queues.json` is what replaced all three.

A corpus fails on the thing that actually matters — a value one side computes differently — and
survives everything that does not.

## The corpora

| File | What it pins |
|---|---|
| `batchid.json` | The Batch content hash: canonical JSON of `[method, units, params]`, sorted keys, no whitespace, **raw UTF-8 with no HTML escaping**, sha256, first 16 hex. |
| `queues.json` | Every name an **Actor** is addressed by: the shared task queue, the sessions queue, one live **Session**'s queue, the Nexus endpoint, and the tmux session a **Worker** runs in — including every fallback. |
| `blobkey.json` | The object key layout for a committed Unit. |
| `bundleref.json` | Where a **Bundle** lives once it is an OCI artifact (ADR 0036): the repository, the two references (tagged and pinned), and the two distribution-spec URLs. Go and TypeScript arms. The MANIFEST url has one implementation — only the control plane resolves a tag — which is recorded here rather than mirrored into a Go helper nothing would call. |
| `catalog.json` | The Actor catalog's emitted descriptor. |
| `output_dataset.json` | The output-Dataset author surface. |
| `slug.json` | A Run id as one path segment and one SQL identifier — the first half of a temp Dataset's name. Three writers: both SDKs' slug, and the orchestrator's `safeName` on top of it. |
| `redaction.json` | The three rules both SDKs apply before an **ask** or a narration reaches Temporal history: a whole-key match on an ask's fields, and two word-in-a-sentence matches — a secret-shaped assignment, and a bearer/basic credential. Python and Go arms, no TypeScript arm. **That is not the same gap `output_dataset.json` has**: the orchestrator does not compose asks or narrations, so there is no third writer of *these* rules to drift. `control/orchestrator/src/state.ts:capAndRedact` is a DIFFERENT rule on a different path — it redacts credential-shaped keys out of a state-tier READ — and merging the two into one corpus would be the mistake the `queues.json` note below describes, one rule read as two surfaces. |
| `codec/fixtures.json` | The claim-check codec's wire format. |
| `workerhealth.json` | The sick-but-not-dead judgement (ADR 0037): the two counters both actor hosts export, the three thresholds, and the verdict table the **Warden** turns two readings into. **THREE ARMS, AND ONE OF THEM IS A READER** — `runtime/python/internals/metrics.py` and `runtime/go/engine/metrics.go` WRITE the series independently, and `cli/warden/sickworker.go` on a Machine parses whichever it finds and divides one by the other. That is the shape with no loud failure: each side renders a valid exposition and passes its own tests, and a rename leaves the Warden dividing by an absent series on every Worker of one language — visible only as a `loads` chip stuck on `unknown`, which is what an uninstrumented Worker looks like too. |
| `ociref.json` | The OCI reference grammar and the two rules a `--push` DESTINATION has that a pull does not (ADR 0036: kontra owns no registry). **ONE IMPLEMENTATION, THREE READERS**, which is a different shape from every other row here and is why it exists: `cli/build.go`, `cli/scale.go` and `cli/warden/driver_podman.go` each name an **Artifact**, and each used to answer for itself. `.scratch/warden/issues/15-*` measured the cost — one message for two opposite truths, `a/b` (never deployed, and deploying fixes it) and `café` (can never be an image, and nothing does), with two remedies both unreachable for the second. So the corpus does not pin two languages agreeing; it pins that the three readers have not re-derived the grammar, by driving all three over the same rows. Carries `queues.json`'s adversarial names on purpose, plus `1:2` — a legal VERSION that cannot be a tag. **The `allow` section is a FOURTH reader and a different question** (slice 14, `cli/internal/trustpolicy/trustpolicy.go`): which registries a **Machine** pulls from, and in what order the refusals happen. It is here rather than in a corpus of its own because it is asked of the SAME two fields the grammar produces — a policy that split a reference for itself is not merely inconsistent, it is a bypass (`strings.HasPrefix(ref, "ghcr.io")` admits `ghcr.io.evil.example/x`; both rows are in the file). Its `coupling` key names the mutations that must turn grammar and policy red together. |
| `lease.json` | A **Lease** (ADR 0037): the id grammar `<holder>#<nonce>`, the two activity names, and the **Lease** workflow's HTTP wire. **THREE ARMS AND TWO SEPARATE CONTRACTS.** `sdk/python/actorkit/fleet.py` WRITES the id and `control/orchestrator/src/lease.ts` READS it back — that is what answers "who is holding this Fleet" — and the failure has no loud mode at all: an id held under one spelling and dropped under another is a **Lease** that is never dropped, which is **Machines** that bill with nothing left anywhere that knows about them. The hold succeeds, the drop succeeds against a key the **Lease** workflow is not holding, and `Map.delete` returns quietly. Separately `control/orchestrator/src/lease.ts` WRITES the **Lease** workflow's JSON and `cli/lease.go` decodes it, which is `terminal.json`'s contract in a second place, with the same asymmetric containment — and the same reason it matters here is sharper: an EMPTY `holder` is a real state (an unattributed **Lease**, expiring on its clock), so a drifted `holder` key makes every **Lease** read as adopted rather than merely blank. |
| `placement.json` | A **Fleet**'s DESIRED STATE — the arguments one `kontra-fleet` converge carries (ADR 0037). **TWO WRITERS AND ONE READER, AND THE READER IS SILENT IN BOTH DIRECTIONS.** `sdk/python/actorkit/fleet.py` builds it from `hold()`/`place()`/`up()` and `cli/fleet.go` from `kontra fleet up\|deploy`; `coerceFleetArgs` narrows whatever arrives and DISCARDS the rest without a word — which is how `--tmux` rode as a boolean for a release, letting a Machine deploy "successfully" and never be viewable. The other direction is worse than a dropped flag, because Pulumi's desired state is TOTAL: a key a writer stops sending is a request to REMOVE what it describes, and an absent `machines` coerces to 0, builds no Droplets, and DESTROYS the Fleet while reporting success. So the corpus pins key SETS rather than a golden — the two writers genuinely compute different bytes for the same shape, one from a resolver over the network and one from an Artifact on disk — plus the coercion table the writers are checked against. **SINCE PACKING (slice 11) IT PINS A COUNT AS WELL AS A SET**, and a SECOND key set one level down: a **Fleet** carries several placements now, so a `placements` array naming one of two is the same silent removal as a missing key — it deletes the co-tenant's install command, runs that **Worker**'s teardown, and reports success. `placement_keys` is the entry-level list, `spread` is the row that pins a VALUE (`spread=True` crosses as the NUMBER `workers`, because `never_boolean` forbids the flag itself and a writer that dropped the key would place on every Machine anyway and pass any assertion about the key alone), and the two writers reach the packed cases by different doors — Python through `f.place()` twice, Go through the `fleet up` INHERIT path, which is the only place `kontra fleet` can get a packed converge wrong. |
| `terminal.json` | The **Terminal** wire: `control/orchestrator/src/panels/types.ts` serialises it, `cli/panels.go` decodes it, and nothing joins them but matching string literals. **THE CONTAINMENT IS ASYMMETRIC AND THAT IS THE POINT** — the WRITER's key set is pinned exactly, because a key it stops sending is one some reader still waits for; the READER's need only be a subset, because ignoring `paneCols` is correct and growing a field for it would be ceremony. What a reader may NOT do is declare a key nobody sends: `encoding/json` leaves it at the zero value on every response, and `dashIfEmpty` draws that identically to a Machine with no Fleet. Written after finding two such keys live — `campaign`, years after the writer renamed it to `fleet`, blanking that column in `kontra panels` on every Fleet; and `role`, which no writer has ever sent and no Go line ever read. |

## Adding one

1. Write the corpus with a `why` per case. A case whose reason is not written down is a case
   nobody can tell from a typo when it goes red.
2. Include the inputs that BREAK — the characters, the empty collection, the ordering. A corpus of
   the easy cases is the shape of every guard in this list that failed to guard anything.
3. Drive it from **every** implementation, and assert in each driver that the corpus is not empty
   and still contains its interesting inputs. A corpus that silently shrank to nothing passes.
4. Delete the hand-written golden it replaces. Two guards for one contract is one guard and one
   thing to keep in sync.

`output_dataset.json` has Python and Go arms but no TypeScript arm, which is recorded here rather
than left to be discovered: it is a two-of-three corpus today.

## When a corpus has one writer

`ociref.json` breaks the rule at the top of this file — it pins a contract with ONE implementation,
not two. It is here anyway, and the reason is worth stating rather than leaving as an inconsistency:
the drift it guards is not between languages, it is between **call sites**. Three places name an
**Artifact** and each of them can quietly grow its own regexp and its own sentence, which is exactly
what had happened. The failure has the same shape a two-writer drift has — nothing red, each site
passing its own tests, and an operator told the wrong one of two opposite things — so it gets the
same instrument. What makes it a corpus rather than a unit test is that every site is driven over
the same rows, so "one shared answer" is checkable: break the grammar in one place and all three go
red, which `cli/ociref_conformance_test.go` records as measured.

## When a corpus records a difference rather than an agreement

`queues.json` §tmux_session carries two expected answers per row — `worker` and `machine` — and
they differ on three of them. That is not a bug the corpus is failing to catch; it is two rules
that were being read as one because both produce `<actor>-<version>` on every ordinary input.
`worker` names an **Actor**'s **Worker** and falls back to `actor`; `machine` names a fleet
**Machine**, which still has Terminals when nothing is placed on it, and falls back to the fleet's
tag and then to `fleet`.

**A difference that is not written down is one nobody can tell from a typo when it goes red**, so
the rows where the two disagree say why in their own `why`, and a driver asserts that they do.
Every OTHER difference found while writing this corpus was a drift and was fixed: `cli/internal/tmux/tmux.go`
returned an unattachable `''`/`'_'` where its TypeScript peer answered `actor`, and
`cli/fleet.go` passed a name through that `panels/discovery.ts` refuses outright.
