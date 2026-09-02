# Architecture Decision Records

The full records live in [`docs/adr/`](../adr/).

**Seven of them describe the system.** Everything else is history, archived under
[`docs/adr/legacy/`](../adr/legacy/) by ADR 0024 — a hard line drawn at v2, not a staleness audit.
A record at the top level is a claim about the running system; a record in `legacy/` is evidence
about why, never authority about what.

## Current

| # | Decision |
|---|---|
| 20 | [The Dashboard: read-only Terminals over the Fleet's tmux](../adr/0020-dashboard-read-only-terminals-screen-attach.md) — decided after v2 and about a live feature, so it is not legacy despite its number |
| 23 | [v2: one deployed kind, addressable Sessions, and the caller owns the loop](../adr/0023-v2-one-kind-sessions-caller-owned-loop.md) — 23 numbered decisions, with the rejected alternatives. The spec. |
| 24 | [v2 is the system of record; the pre-v2 corpus moves to `legacy/`](../adr/0024-v2-is-the-system-pre-v2-corpus-is-legacy.md) — the archive decision, and the table of what v1 invariants v2 carries forward |
| 25 | [The reduced event log outlives retention](../adr/0025-reduced-event-log-outlives-retention.md) |
| 26 | [Scratch is a drawing that produces code, not an execution model](../adr/0026-scratch-is-a-drawing-not-an-execution-model.md) |
| 27 | [The external schema registry is removed; per-actor I/O compat is owned in-process](../adr/0027-schema-registry-removed-in-process-compat.md) — supersedes `legacy/0008`'s second half; its §4 named the cross-version check nobody was doing, and the consequences record what closed it (input BACKWARD, output FORWARD, reported at registration and never refused) |
| 28 | [Emit is decoupled from the Unit, and the caller redirects it](../adr/0028-emit-decoupled-from-unit-caller-redirects.md) — amends 23's §3, §13, §15, §16, §18, §23; the input cursor and output offset split, the caller supplies the output **Dataset** as a third **Method** parameter, the per-**Unit** emit and the isolate flag retire for `(results, dropped)`, and **Batch** survives as the cheap ref beside a named **Dataset** |

| 29 | [A Dataset has a name and tags: derived identity, operator-owned retention](../adr/0029-live-datasets-name-tag-retention.md) — extends 23's §11 with what a Dataset is *called* and how long it survives: a derived run-grain name (no name→path registry), tags as a **set** with the record as the authority and `KontraTag` as a one-way projection, and a TTL this repo owns clocked from last write. Carries two corrections found on implementation — the run fragment is a **digest**, and the name comes from the **caller's** manifest, snapshotted at start |
| 30 | [The code editors become read-only viewers](../adr/0030-code-editors-become-read-only-viewers.md) — reverses `32c27b0` on the part that made them *editable* and keeps every part that made them *legible*; extends 20's "a dashboard view of code the operator did not author here has no input path" from Terminals to the two source viewers, at the deployed digest |

Read 23 for what the system is and 24 §"What v2 carries forward" for the v1 rules that still bind.
20 is orthogonal: it is how you WATCH a fleet, not how one runs.

The next ADR is **0031**. This paragraph used to say `docs/adr/` was gitignored and that a new
record only landed if you `git add -f` it — the reason 0017 and 0020 went missing from history.
That rule is **gone from `.gitignore`** as of today, so a plain `git add` tracks a new record.
Force-adding is still harmless; verify either way with `git show HEAD:docs/adr/<file>` rather than
by looking at the file on disk, which is what made the original failure invisible.

## Legacy

Eighteen records, 0001–0022, in [`docs/adr/legacy/`](../adr/legacy/) — see its
[README](../adr/legacy/README.md) for the per-record index and what became of each. They were
archived by **era**, so several are still substantially correct; correctness is not what earns a
place at the top level. Expect dead file paths (`interpreter.ts`, `mapAndRoute`, `step.proto`) and
retired words (**Turn**, **Chunk**, **Manifest**, **Activity**, `arun`, `step`).

**9 and 13 were deleted rather than archived** on 2026-08-13 — the "step" ADRs, whose concept had no
implementation left to supersede. Older records still cite them; those links are dead on purpose.
The legacy README carries the full tombstone.

In-code `ADR 00XX` comments below 0023 resolve into `legacy/` and were deliberately not rewritten:
they are accurate as provenance, and repointing 100+ SDK comments would churn every file to restate
what the folder already says.
