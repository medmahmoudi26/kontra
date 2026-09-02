# Legacy decision records (pre-v2)

**These are history. Do not cite them as live design.**

Every record here was written before **v2** ([ADR 0023](../0023-v2-one-kind-sessions-caller-owned-loop.md)),
which deleted the graph interpreter, the **Activity** kind, the `step`/`arun` vocabulary, two of the
four state tiers, and the framework-owned loop. They are archived by *era*, not by staleness — some
are still substantially correct, and being correct is not what earns a document a place at the top
level. See [ADR 0024](../0024-v2-is-the-system-pre-v2-corpus-is-legacy.md) for why the line was
drawn this way.

## How to read one

- **Evidence about *why*, never authority about *what*.** If it argues for a decision that still
  binds v2, that decision is restated in ADR 0024's carry-forward table, in v2's vocabulary. Cite
  0024 for the rule; cite the file here for the argument.
- **Expect dead file paths.** `interpreter.ts`, `mapAndRoute`, `validateRun.ts` and `step.proto` are
  all gone. A record naming them is not lying — it was true when written.
- **Expect retired words.** **Turn**, **Node**, **Chunk**, **Manifest**, **Activity**, `arun`,
  `@actor.step`, `concurrent_aruns`, `session_state`, `arun_state`. `CONTEXT.md` records what
  replaced each.
- **A "Pending" or "Phase D/E" list here is not a to-do.** 0012's is the clearest trap: it stages
  work against a dispatcher v2 removed.
- **In-code `ADR 00XX` comments resolve here.** They are correct as provenance. Read them under the
  rule above.

## Index

| # | Decision | Fate under v2 |
|---|---|---|
| 1 | [Nexus operations + namespace-per-author tenancy](0001-nexus-operations-namespace-per-author.md) | carried forward — dispatch is still Nexus, one op per `(name, version)` |
| 2 | [proto-envelope / Apicurio-JSON split with an opaque payload](0002-proto-envelope-apicurio-json-split.md) | split survives; the file list is stale (`step.proto` deleted 2026-08-13) |
| 3 | [Enforce I/O schema server-side, not advisory](0003-enforce-io-schema-server-side.md) | rule carried forward; every named file went with the interpreter |
| 4 | [Immutable, content-pinned (name, version) identity](0004-immutable-content-pinned-identity.md) | carried forward |
| 5 | [Claim-check codec = execution; output query = read-only side-channel](0005-claim-check-execution-iceberg-query.md) | carried forward |
| 6 | [Server-minted immutable runId, distinct from the user graph label](0006-server-minted-immutable-run-id.md) | id rule carried forward; what a **Run** *is* changed (0023 §12) |
| 7 | [Refs as the sole workflow currency / CAS run-independence](0007-refs-as-currency-cas-run-independence.md) | carried forward — load-bearing |
| 8 | [Compat ownership split: buf owns the envelope, Apicurio owns author I/O](0008-compat-ownership-buf-envelope-apicurio-io.md) | carried forward |
| 10 | [CONTRACT_VERSION as a single constant the wire strings reference](0010-contract-version-single-constant.md) | carried forward as debt — still not implemented |
| 11 | [Content-pinned actor identity via OCI image digest](0011-content-pinned-actor-identity-oci-digest.md) | carried forward |
| 12 | [Split EntryInput: the actor runs one batch, the dispatcher shards](0012-split-entryinput-actor-runs-one-batch-dispatcher-shards.md) | **superseded by 0023.** Its Pending list is not work |
| 14 | [Dataset query engine: parquet datasets + direct browser range-reads](0014-dataset-query-engine-parquet-direct-browser-reads.md) | format carried forward; the producer is now any caller (0023 §1) |
| 15 | [Three-tier state model + the `arun_state` / `concurrent_aruns` naming](0015-three-tier-state-and-naming.md) | key scheme carried forward; **the tier list is wrong** (0023 §19) |
| 16 | [Dapr is the local actor runtime; Temporal owns distribution](0016-dapr-local-actor-runtime-temporal-distribution.md) | **superseded by 0018** before v2 even started |
| 18 | [Temporal is the actor runtime; Dapr is removed](0018-temporal-native-actor-runtime.md) | carried forward — read before proposing a placement directory |
| 19 | [The orchestrator owns provisioning (Pulumi)](0019-orchestrator-owns-provisioning-pulumi.md) | carried forward — v2 does not reach this seam |
| 21 | [There is a caller's side; an Activity is a third kind of deployed code](0021-callers-side-workflows-and-the-activity-kind.md) | **Activity kind retired** by 0023 §9; the caller's side became the only way in |
| 22 | [A dispatch can be keyed; `object_state` is that key's state](0022-object-state-and-keyed-dispatch.md) | carried forward — load-bearing in 0023 §8/§10 |

## The gaps in the numbering

**9 and 13 were deleted, not archived** (2026-08-13). They were the "step" ADRs — a per-step failure
policy, and "a step processes one unit". Unlike everything above, they had no implementation left to
preserve: `@actor.step` had been renamed, `a.Step` had zero callers, and `step.proto`'s
`StepOptions` and `kontra.v1.RetryPolicy` were read by no SDK in any language — so 0009's
"author-declared retry, implemented" described a contract nothing wired. The real retry is
`MaximumAttempts: 3`, hardcoded in `runtime/handler/workflow.go`. Records above still cite them; those links
are dead on purpose.

**17 and 20 are in the repo, and `docs/adr/` is not gitignored any more.** Both halves of what this
paragraph used to say have since stopped being true, and it is left corrected rather than deleted
because the correction is the useful part. 0017 is `legacy/0017-output-queryable-completion-semantics.md`
and 0020 is `../0020-dashboard-read-only-terminals-screen-attach.md`; `.gitignore` carries no `adr`
pattern at all, `git ls-files docs/adr/` lists 34 records, and `git status --ignored docs/adr/`
reports nothing. **A new ADR needs a plain `git add`** — the `git add -f` this file used to demand
was correct under the old rule and is now cargo. ADR 0024's Consequences section still describes the
old rule; it is a RECORD and is left as written, which is what rule 6 above is for.

## Why this directory still exists

Kept, deliberately, and this is the stated reason rather than an omission. ADR 0024 §4 decided
"nothing is deleted" and argued it against the precedent of 0009 and 0013, which *were* deleted:
those two described a contract no SDK ever read, so there was nothing to preserve. These eighteen
argued real trade-offs against real alternatives, **and the alternatives get proposed again** — 0018's
Dapr-removal argument most of all. The cost of keeping them is one directory and the reading rules
above; the cost of deleting them is that the next person to propose a placement directory has to
re-derive why there isn't one. Re-confirmed by the architecture-v2 sweep, which was asked to remove
this corpus or say why not.
