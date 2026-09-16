# 48. The console migrates to Svelte, one surface at a time, behind a route split

Date: 2026-09-16

## Status

Accepted, and **carried out** — 2026-09-16. Every Surface serves from Svelte, the React console is
deleted, and there is one bundle again. What that cost and what it left undone is in
[Outcome](#outcome) at the end; the decisions below are unchanged and are what was followed.

Amends nothing. It constrains [ADR 0038](0038-two-repositories-one-version-one-licence.md) — the
console stays one repository and one version — and depends on
[ADR 0045](0045-the-console-signs-in-rather-than-carrying-a-baked-bearer.md), because two bundles
that each sign in separately would be two logins.

## Context

The console is slower and less smooth than comparable tools, and the reasons are measurable rather
than felt. Measured on the built output and the source, 2026-09-16:

**453 KB of JavaScript on first paint**, out of 6.2 MB across sixty-odd chunks. Code splitting
works — mermaid, cytoscape and katex are separate and only fetched when a view needs them — so the
6.2 MB is not what a visitor pays. The 453 KB is.

**Twelve files use `setInterval`, and the live surfaces poll.** The visible loops are 10s and 15s; a
running container logs `GET /api/runs` and `GET /api/datasets/runs?limit=500` on repeat. A polled
surface is stale for the length of its interval and then jumps, and the jump is what reads as slow.

**Two SSE endpoints exist and nothing consumes them.** `/api/runs/{runId}/stream` and
`/api/sources/actor/{id}/schema/stream` are both in the console's generated route table and neither
is subscribed to. The second one is the whole of "the form tracks your editor" — `kontra serve
--watch` re-registers a contract on every save and the console does not listen.

**The type is 10px.** 273 of 378 size declarations are 10px or 11px, with 27 more at 9px — and 336
of them are arbitrary values (`text-[11px]`) against 48 scale tokens, so there is no scale, only
per-site sizes. In a VS Code panel or on a phone this is hostile.

### What the migration actually costs, measured

The framework is a smaller part of this codebase than it looks:

| | count | cost |
|---|---|---|
| Non-test `.ts` modules that import React | 11 of 88 | rewrite |
| Non-test `.ts` modules with no framework | **77 of 88** | move unchanged |
| Components (`.tsx`) | 71 | rewrite |
| Logic tests | **100 of 130** | port unchanged |
| Render tests | 30 | rewrite |
| Playwright e2e | 3 specs + harness | framework-agnostic |

Rewrite 82 files, move 177. And the React-only libraries are concentrated: `ag-grid-react` appears
in exactly one file, `@xyflow/react` in three, `@uiw/react-codemirror` in three. shadcn/ui turns out
to be three primitives — badge, button, input — and no Radix at all.

## Decision

**1. Svelte 5, and the split is at the ROUTE.** The orchestrator serves two SPA bundles; a migrated
Surface's first path segment resolves to the Svelte bundle, everything else to React. The seam is a
URL.

*There is no interop layer, and that is the point.* Mounting Svelte components inside the React
shell would mean two reactivity systems sharing state and every shared component written twice — the
arrangement these migrations die in. The cost paid instead is a duplicated shell and a full page
load when navigating between a React Surface and a Svelte one, for the length of the migration.

`SPA_SURFACES` in `server.ts` becomes two sets. It already carries a comment about being a copy of
the console's own list pinned by `spaFallback.test.ts`; that pinning now covers both sets, because
the failure it prevents — a Surface that 404s on cold load — is the same failure twice over.

**2. The framework-free 77 become `@kontra/console-core`, extracted BEFORE any Svelte is written.**
Both apps import it. One source of truth for schema derivation, run state, dataset accrual and
addresses, so the two consoles cannot come to disagree about what a Run is.

*The alternative was copying and deleting as Surfaces migrate*, and it fails in a specific way: for
the length of the migration a bug fixed in one copy stays live in the other — on the Surfaces not
yet migrated, which are the ones still in front of people.

**3. Nothing polls. Every Surface derives from a subscription.** The two unconsumed SSE endpoints
are wired as part of the first Surfaces that need them, and `setInterval` is not used for data.
This is what "reactive to code changes" means here: the operator edits an Actor, `--watch`
re-registers the contract, and an open form grows a field without being refreshed.

**4. Panel-first. ~390px is the primary width; wider viewports EARN columns.** An IDE side panel and
a phone are the same constraint, and a layout designed at desktop does not narrow into one.

*Measured, on our own prototype.* It was built page-first and had 59px of horizontal overflow at
390px and 129px at 320px. The four bugs behind that were all desktop assumptions: an event log whose
rows overlapped their own text, a card table that widened the page because grid items default to
`min-width: auto`, a nav whose brand and tenant chip pushed the document sideways, and 132px of a
390px screen spent on lane labels. None is findable by shrinking a desktop layout; all are avoided
by starting narrow.

**5. 14px base, 12px hard floor, one scale, tokens only.** Roughly 12 / 14 / 16 / 20 / 24. Nothing a
person reads goes below 12px; 11px is permitted only for uppercase micro-labels. Arbitrary sizes at
call sites are not allowed — that is how 336 of them accumulated.

This costs density. Some tables lose a column or gain a second line, and that is the trade being
bought deliberately rather than discovered later.

**6. `/dev` — the IDE embed — is the first Surface.** Smallest, no React-only dependency, already a
standalone route, already panel-shaped. It exercises the route split, panel-first layout, the type
scale and SSE reactivity in one slice, and it improves the surface that today's 10px type hurts
most.

*Datasets-first was considered and rejected.* It would prove the hard case first — 11 React cell
renderers, ag-grid's vanilla core, CodeMirror, the 1.2 MB chunk — but it puts two library ports in
the first slice and delays everything shipping until the densest table in the product is rebuilt.

**7. The goal is React deleted, with a costed checkpoint at Datasets.** After `/dev`, Catalog,
Actors, Secrets and Settings have shipped, the Datasets port is estimated against what those
actually took, and the decision to continue is made against that number.

*The gate is written down because of decision 6.* Choosing the easy Surface first means the
expensive one stays uncosted for longer; the checkpoint is where that debt is paid, at the last
moment when the evidence exists and the money has not been spent.

## Consequences

**Two bundles, two shells, one login.** For the length of the migration the console has two front
doors and the navigation between them is a page load. ADR 0045's session is what keeps it one login;
a bundle that authenticated separately would make the split visible in the worst possible place.

**`@kontra/console-core` must stay framework-free, permanently.** It is the load-bearing assumption
of the whole plan. A React import in it silently ends the arrangement, so it needs a guard, not a
convention.

**The e2e suite becomes the migration's contract.** Three Playwright specs cannot tell which
framework is underneath, which makes them the only tests that prove a migrated Surface still behaves
like the one it replaced. They are worth extending before the port, not after.

**Density drops.** Decision 5 is a deliberate loss of information per screen in exchange for a
console that is readable in a panel and on a phone. Anyone expecting the current row counts will
find fewer.

**The timeline is not in this decision.** The run-as-timeline view prototyped alongside this is a
Surface-level design question for Workflows, and it stands or falls on its own.

## What this does not decide

Whether the Svelte app eventually absorbs the Dashboard's terminals; whether Svelte Flow is the
right replacement for React Flow, which needs a spike rather than an argument; and what happens to
the 30 render tests — ported to `@testing-library/svelte` or replaced by e2e coverage — which is
cheap to decide once the first Surface exists.

## Outcome

Recorded the day the last slice landed, because a migration's value is in what it actually measured
rather than in what it set out to do.

| | before | after |
|---|---|---|
| JavaScript on first paint | 472 KB | **60.9 KB** (23.6 gzipped) |
| Built output, all chunks | 6.2 MB | **1.8 MB** |
| `setInterval` for data | 12 files | **0**, guarded |
| SSE endpoints consumed | 0 of 2 | **2 of 2** |
| Smallest type a person reads | 9px | **12px**, guarded |
| Horizontal overflow at 320/390/1280px | untested | **0px**, 10 routes × 3 widths in CI |
| Playwright specs passing | 19 | **19**, one assertion scoped (below) |

**The e2e suite passed with one edit, and the edit is the wall.** `dashboard.spec.ts` searched the
whole page for the word `snapshots`; the React console pinned ONE Terminal, so that was unambiguous,
and the Monitor now draws every Terminal the streamer serves. The assertion is scoped to the tile
under test. The same file's spec at line 230 was written to start proving itself "the day the wall
arrives" — it now does. Nothing else in either spec changed.

**Three things were found missing only by deleting React**, each of which had shipped as a surface
and was not a replacement for what it replaced:

 - the Monitor drew a LIST OF TERMINAL NAMES — no xterm, no socket, no converge — behind a comment
   saying "xterm is a mount, not a port". `/monitor` was already serving Svelte, so the terminal
   wall had been dark in the product since that slice shipped, and the e2e suite did not catch it
   because it drives the app through its own Vite config rather than through the orchestrator.
 - Workflows could READ runs and not START one. The console's whole purpose was unreachable.
 - nothing called `session.install()` and there was no sign-in screen, so every API call went
   unauthenticated (ADR 0045). On a gated install every surface would have rendered its own 401 as
   "empty".

The common shape is that each surface was ported until it looked right, and a browser test that
drove the OLD app reported green throughout. What the deletion did was remove the old app the tests
were driving.

**What is NOT ported, and is therefore gone from the product until it is.** 49 of 80 framework-free
modules in `@kontra/console-core` are now unreachable from the console. They are kept rather than
deleted, because each is the derivation a port would need: `run/query` and `datasets/cells` (the
dataset SQL workbench and its cell renderers), `panels/methodCall` (calling a Method from the Actors
surface), `panels/workflowThread` and `panels/transcriptDrill` (the workflow thread and turn drill),
`panels/runStats`, `panels/runMachines`, `panels/runDatasets` (what a run did, by machine and by
dataset), `panels/grid/wall` and `panels/chrome/*` (the draggable tile wall, its menus and its
scroll physics), `panels/folderWorkbench` (editing an actor's files in the console),
`panels/widgets/*`, `panels/ask` (HITL turns).

That list is the honest size of the remaining work, and it is larger than the seven-surfaces
headline suggests.
