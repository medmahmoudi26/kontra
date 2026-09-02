# 24. v2 is the system of record; the pre-v2 corpus moves to `legacy/`

## Status

**Accepted, 2026-08-14.** Archives **0001–0022** into `docs/adr/legacy/`. It is 0023's companion,
not its successor: **0023** decided what v2 *is*, and this one decides what happens to the eighteen
records 0023 did not mention. Together they are the whole of `docs/adr/`.

Nothing here changes code. It changes what a reader is entitled to believe.

## Context

Nineteen records, ~1,850 lines, written across v1's entire life. **0023** then deleted the graph
interpreter, the **Activity** kind, the whole `step`/`arun` vocabulary, two of four state tiers, and
the framework-owned loop. What it did not do — could not do, one ADR at a time — is tell a reader
which of the other eighteen documents still describe running code.

The drift is not hypothetical, and it is not confined to the ADRs v2 explicitly supersedes:

- **0003** claims enforcement lives in `src/schema/validateRun.ts` and `mapAndRoute`. Both went with
  the interpreter. The *principle* (validate on the execution path, not advisorily) survives; every
  file path in the record is a 404.
- **0012** still carries a "Pending: D, E, F" list — staged work against a dispatcher that no longer
  exists. A reader who picks up that list is implementing v1.
- **0016** was superseded by **0018** and sat in the index for months anyway, marked and cited.
- **0002** enumerates `step.proto` as a live contract file. It was deleted on 2026-08-13.
- **0006** describes minting a runId "before starting the interpreter", and **0014**'s
  materialization hangs off the same deleted component.

This is the failure the **step purge** already paid for once, at corpus scale. `@actor.step` had no
implementation for months and still survived in `contracts/`, both generated SDKs, a Go method with
zero callers, and two ADRs — and it kept sending agents to build a pipeline that does not exist.
The cost of a stale record is not that it is wrong; it is that it is *confidently* wrong, in the one
place we tell people to go for the reasons behind the code.

The reason a per-document staleness judgement cannot fix this is visible above: **drift is
per-paragraph.** 0003's principle is live and its file paths are dead. 0015's tier scopes and key
scheme are live, its tier *count* is not, and its mechanism section names a runtime we removed.
0014's parquet-and-range-reads is live and its producer is deleted. Every one of these is a
document that is neither current nor discardable — which is exactly the document that gets cited as
authority.

## Decision

1. **A hard line at v2, not a staleness audit.** Every ADR numbered below 0023 moves to
   `docs/adr/legacy/`, including the ones still substantially correct. The line is drawn by *era*
   because era is a fact and staleness is a judgement — and it is the judgement that has failed
   repeatedly. A reader gets one bit, reliably, instead of a per-paragraph verdict they have to
   re-derive against the tree.
2. **`docs/adr/` holds only records that describe v2.** Today that is 0023 and this one. An ADR at
   the top level is a claim about the running system; anything else belongs one directory down.
3. **Numbers are neither reused nor rewritten.** `legacy/0018` keeps 0018. v2 continues at 0025.
   Renumbering v2 from 0001 was the tidier layout and lost on two facts: `ADR 0023` appears 183
   times in source, and a v2 `0001` would collide with legacy `0001` (Nexus) — manufacturing the
   precise ambiguity this ADR exists to remove.
4. **Nothing is deleted.** This is deliberately *not* what happened to 0009 and 0013, which were
   deleted outright because the concept they described had no implementation left to supersede.
   These eighteen argued decisions that are still the reason today's code looks the way it does.
   0018's Dapr-removal argument in particular should be read by anyone who proposes a placement
   directory again — including §14 of any future v2 record.
5. **What survives is restated below, not inherited by reference.** An archived record cannot be
   load-bearing. If a v1 decision still binds v2, it is written out in §6 in v2's vocabulary, and
   the legacy file is cited only for the argument that produced it. This section is the one that
   makes the move safe rather than lossy.
6. **A legacy ADR is evidence about *why*, never authority about *what*.** Citing one as live design
   is a bug, the same class of bug as citing 0009. Legacy records may name deleted files, retired
   words (**Turn**, **Chunk**, **Manifest**, `arun`, `step`), and superseded runtimes; that is what
   a historical record does.
7. **Existing in-code `ADR 00XX` comments stay as they are.** `ADR 0015` in `statekv.py` still
   resolves — to `legacy/0015` — and is correct *as provenance*. Rule 6 governs how to read it.
   Rewriting 100+ comments to say `legacy/` would churn every SDK file to restate what the folder
   already says.

## What v2 carries forward

The invariants below are **live**, stated here in v2's terms. The citation is where the argument
lives, not where the authority does.

| Invariant, as it holds in v2 | Argued in |
|---|---|
| **Refs are the only payload that crosses a workflow boundary.** Bulk never rides in a history. Two **Actors** are two processes and their data travels as refs — 0023 §16's intra-Actor composition is an optimisation inside one **Method**, never a replacement for this. | `legacy/0007` |
| **The claim-check codec is the data-movement layer on the execution path**, byte-compatible across languages and conformance-pinned; querying output is a read-only side-channel that never sits on that path. | `legacy/0005` |
| **Identity is immutable and content-pinned.** `(name, version)` never moves under a schema change, and it pins to an OCI digest rather than a mutable tag. v2's flag day is the reason this matters more, not less: every image is invalid at once. | `legacy/0004`, `legacy/0011` |
| **Dispatch is Nexus, one operation per `(name, version)`.** v2 adds a **Method** name and a **Session** id to the payload; it does not add a second transport. | `legacy/0001` |
| **The actor process *is* a Temporal activity worker.** There is no second runtime, no placement directory, no lease. v2's per-**Session** task queue (0023 §6) is this decision followed one step further: the queue name is the address. | `legacy/0018` |
| **buf owns the envelope's compat; the catalog's registration gate owns author I/O compat** — a `(name, version)` re-registered with a moved input/output/params schema is refused at `POST /api/actors`. The ownership SPLIT is what carries forward; the second owner was named as an external schema registry here and there was never one running, so **0027** supersedes that half and records the cross-version check nobody is doing. | `legacy/0008`, superseded by **0027** |
| **Schema is enforced on the execution path, not advisorily.** The enforcing component moved (0023 deleted the interpreter that hosted it); the rule that unvalidated input must not reach an actor did not. | `legacy/0003` |
| **Run ids are server-minted, immutable and unique per execution.** What a **Run** *is* changed — 0023 §12 makes it one execution of a caller's workflow — but it is still keyed by an id nobody may supply. | `legacy/0006` |
| **A Dataset is plain parquet, range-read directly, with DuckLake owning the catalog.** 0023 §1 changes only the *producer*: any caller's workflow may publish and seal one, where materialization used to be the interpreter's privilege. | `legacy/0014` |
| **State tiers are scoped, and their key scheme is frozen.** The *scopes* and keys survive; the *count* does not — 0023 §19 leaves three (`self.*`, `object_state`, `global_state`) and retires `session_state` and `arun_state`. Read 0015 for the key scheme, never for the tier list. | `legacy/0015` |
| **A dispatch can be keyed, and the key has durable state.** Load-bearing in v2, not merely surviving: 0023 §8 and §10 are built on it, and §10's "keys are optional" is the extension. | `legacy/0022` |
| **The orchestrator owns provisioning.** Pulumi in TypeScript, in-process, durable like every other operation. Untouched by v2 — it is the one seam 0023 does not reach. | `legacy/0019` |
| **`CONTRACT_VERSION` as one constant.** Still a decision, still not implemented; both wire strings remain independent literals. Carried forward as debt, not as a claim. | `legacy/0010` |

Retired outright by 0023 and carried forward by nothing: **0012** (the dispatcher shards),
**0016** (Dapr as local runtime, already superseded by 0018), and **0021**'s **Activity** kind
(0021's other half — that a caller may drive dispatch from their own workflow — is not carried
forward either, because in v2 it is not an extension but the only way in).

## Considered and rejected

- **Annotating each Status line with a staleness note instead of moving the file.** This is exactly
  what 0016 had — "**superseded by 18**", in the index, in bold — and it was cited as live design
  regardless. A note is a thing you have to read *and believe*; a directory is a thing you cannot
  miss. Rejected on demonstrated failure, not on taste.
- **Deleting them, as 0009 and 0013 were deleted.** That precedent does not transfer: 0009 described
  a contract no SDK ever read, so there was nothing to preserve. These eighteen record real
  trade-offs against real alternatives, and the alternatives will be proposed again.
- **Splitting the corpus per seam, under `CONTEXT-MAP.md`'s contexts.** The right eventual shape and
  premature now, at two live records. It would also force a seam judgement on every legacy file, at
  the moment we are deciding to stop making per-file judgements.
- **Moving 0023 into a `v2/` subfolder beside `legacy/`.** Symmetric and worse: it makes the current
  records harder to find than the archived ones, and adds a path component to 183 citations' worth
  of muscle memory. Current work lives at the top; history goes in a folder.

## Consequences

- **`docs/adr/` is the live set and nothing else.** A fresh agent reads 0023, this one and 0020
  (the Dashboard, decided after v2) and is current — which is the whole point, and the measure to
  protect. If it grows past what one sitting can hold, split by
  seam (rejected above) rather than letting the top level drift back into an archive.
- **Every wiki link into `../adr/00XX` is repointed** at `../adr/legacy/`, and `docs/wiki/ADRs.md`
  becomes a two-table index. The links still resolve; they now resolve somewhere honest.
- **Risk accepted: a live invariant I failed to restate in §6 is now filed as history.** The §6 table
  was built by reading all eighteen, but it is a judgement and judgements are what this ADR distrusts.
  Mitigation is §6's rule — a legacy record is still readable evidence, so a missed invariant is
  *misfiled*, not lost. If you find one, promote it into §6 rather than moving the file back.
- **0017 and 0020 were missing from the repo when this was written, and are not any more.**
  `docs/adr/` is gitignored (`.gitignore:62`), so a record only exists here if someone ran
  `git add -f`; those two had been written and never force-added, and `docs/wiki/ADRs.md` cited
  0017 as though it were readable. Both were restored on `main` (PR #8). **0017 is pre-v2 and is
  filed under `legacy/` by the rule above** — its invariant survives in §6, because the
  two-dimensional run status is exactly what `/api/runs/:runId` still answers. **0020 stays at the
  top level**: it was decided after v2 and describes a live part of the running system, so the
  top level is three files, not two. **Every new ADR needs `git add -f`** — including this one.
- **The `arun`/`step` vocabulary now lives only in `legacy/` and git history**, which is the right
  place for it and the same treatment the retired **Turn** got.
