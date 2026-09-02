# design-sync notes — @kontra/frontend

Repo-specific gotchas for future syncs. Read this before re-running anything.

## Where the sync's inputs live — read this first

Three files sit **inside the package**, not under `.design-sync/`, and all three are
load-bearing. The 2026-08-14 sync left them untracked, so the next branch that ran a sync
had none of them and the whole setup had to be rediscovered. They are committed now:

| File | What it is |
|---|---|
| `frontend/.ds-entry.ts` | the barrel — what is IN the design system |
| `frontend/.ds-tailwind.css` | the Tailwind entry `cfg.buildCmd` compiles |
| `frontend/.ds-tsconfig.json` | the esbuild resolution fix (see below) |

`frontend/.ds-styles.css` is the compiled output of the second one and is gitignored.

## esbuild resolves `foo.ts` to `Foo.tsx` — the fix is `.ds-tsconfig.json`

**Symptom:** the bundle dies with dozens of `No matching export in ".../WorkflowContract.tsx"
for import "readSchema"`, naming a file the importer never mentioned, plus
`Detected cycle while resolving import "SPARK_MIN"`.

**Cause:** this package pairs a component with its logic under one name — `Spark.tsx` draws
what `spark.ts` computes; `WorkflowContract.tsx` renders what `workflowContract.ts` reads.
esbuild's resolver tries `.tsx` BEFORE `.ts` *and* matches directory entries
case-insensitively, so `import … from './workflowContract'` lands on `WorkflowContract.tsx`.
The filesystem here is genuinely case-sensitive (checked) — this is esbuild's own lookup.
Vite is unaffected: its `resolveExtensions` put `.ts` first, which is why the app builds.

**Fix:** `cfg.tsconfig` points at `frontend/.ds-tsconfig.json`, which repeats the
app's three aliases and adds one exact-match `paths` rule per ambiguous specifier. The
converter's paths plugin resolves those itself, extension-first. It does **not** `extends`
the real tsconfig — the plugin `JSON.parse`s the file and reads only `compilerOptions.paths`
and `baseUrl`, following no extends chain — so the aliases are duplicated by necessity.

**The five pairs today** are `Spark`/`spark`, `FolderWorkbench`/`folderWorkbench`,
`MethodCall`/`methodCall`, `WorkflowContract`/`workflowContract`, and `SideDock`/`sideDock`.
Adding another `Component.tsx` beside a `component.ts` breaks the bundle again with the same
confusing error — add its specifiers to `.ds-tsconfig.json`. Note the rules are keyed by the
SPECIFIER as written, so `./spark`, `../components/spark` and `../../components/spark` each need
a line.

**OPEN, and it is that trap half-sprung (2026-08-18).** The build prints:

```
▲ [WARNING] Use ".../chrome/SideDock.tsx" instead of ".../chrome/sideDock.tsx"
   .ds-entry.ts:98 } from './src/panels/chrome/sideDock';
```

`.ds-tsconfig.json` maps `"./sideDock"`, but the barrel writes the FULL specifier
`./src/panels/chrome/sideDock`, which no rule matches — so the paths plugin never fires and
esbuild resolves it by its own case-insensitive lookup. It currently lands on the right module
(the build succeeds and `SIDE_BOUNDS {min:180,max:640}` from `sideDock.ts` is in the bundle), so
this is a warning and not yet a break. **The fix is one line** in `.ds-tsconfig.json`:
`"./src/panels/chrome/sideDock": ["./src/panels/chrome/sideDock.ts"]`. Not applied here only
because the session's final build must be the one that was uploaded, and this would have meant
another full driver run for zero functional change. Apply it on the next sync.

## Shape and entry

- This package is a **private Vite SPA**, not a published component library. There is no
  library `dist/`, no `main`/`module`/`exports`. The converter therefore runs from a
  **hand-written barrel**, `frontend/.ds-entry.ts`, passed via `--entry`.
- **Why the barrel is mandatory:** the converter's no-dist fallback synthesizes an entry of
  `export * from '<file>'` per source file, and `export *` does **not** re-export defaults.
  Almost every chrome component here is `export default`, so without the barrel they are
  discovered by name (ts-morph recovers the declared name) and then are *absent* from
  `window.KontraDashboard` — every card fails with "Element type is invalid".
- **The barrel must live inside the package.** `PKG_DIR` is derived by walking up from the
  entry file looking for a `package.json` with a `name`. A barrel in `.design-sync/` walks
  up to `/` and finds nothing, so PKG_DIR resolves wrong and the build dies on
  `ENOENT /package.json`. Hence `frontend/.ds-entry.ts`, not `.design-sync/`.
- Adding a component needs BOTH: an export in `.ds-entry.ts` and an entry in
  `componentSrcMap`. The barrel controls what is in the bundle; `componentSrcMap` controls
  what gets a card.

## Tailwind v4 — the compile step is not optional

- `src/styles.css` is **source** Tailwind (`@import 'tailwindcss'`, `@theme inline`,
  `@apply`). It is not usable as `cssEntry` — previews would render unstyled.
- `cfg.buildCmd` compiles it: `.ds-tailwind.css` (a generated wrapper declaring the extra
  `@source`) → `.ds-styles.css`, which is what `cssEntry` points at. Both files sit under
  `frontend/` because **`cssEntry` is bounded to PKG_DIR** by the converter
  (`tsconfig`/`extraFonts` are bounded to the git root instead, which is looser).
- **ORDER MATTERS — this cost a full rebuild cycle.** Tailwind only generates utilities it
  finds by scanning. `.ds-tailwind.css` declares `@source "../../.design-sync/previews"`,
  but the scan happens *when the CLI runs*. Authoring a preview that uses a utility not
  already present in the app source (`grid-cols-2`, `grid-cols-[auto_1fr]`, `text-2xl`,
  `tabular-nums`) and rebuilding **without recompiling Tailwind first** silently drops those
  utilities: grids collapse to one column and nothing errors.
  **Always: author previews → run `buildCmd` → `package-build.mjs` → capture.**

## React Flow is part of the design system now

The Scratch canvas is real (ADR 0026) and its four node components are the reason:

- The node components are not exported by name — `scratchNodeTypes` is, and the barrel pulls
  `ScratchNodeActor`/`Workflow`/`Dataset`/`Note` out of it. That keeps the registry the single
  source of the kind→component mapping instead of a second list drifting beside it.
- **The barrel imports `@xyflow/react/dist/style.css`**, exactly as `main.tsx` does. Without
  it every `Handle` stacks at the origin: the nodes look plausible and their ports are wrong,
  in every design the agent ever builds with them.
- **`ReactFlow`, `ReactFlowProvider`, `Background`, `Controls`, `MiniMap` are exported and
  excluded from the card index** (`componentSrcMap: null`). They are in the bundle so a
  preview and the bundled nodes share ONE module instance — `Handle` reads the flow store
  through context, so a preview importing its own copy renders nodes whose handles throw.
- A node preview is therefore a real (static) canvas: see `previews/ScratchNodeActor.tsx`.
  `fitViewOptions` padding is a fraction of the VIEWPORT, so 0.25 spends half the card on
  margin and shrinks the node to a sixth of its size; `{ padding: 0.08, maxZoom: 1,
  minZoom: 0.9 }` with a ~128px canvas (one node) or ~210px (two) is calibrated.

## Component-level findings

- **`DetailDrawer` is deliberately not synced.** It imports `sharedQueue` from
  `@core/panels/pollers`, which lazy-loads Temporal via `require('@temporalio/client')`
  *inside a function* — deliberately, so the module's type graph stays browser-safe. Vite
  and rollup leave that call alone (the real app is fine), but esbuild's bundler follows
  `require()` statically and pulls in `@grpc/grpc-js`, `node:crypto` and `node:async_hooks`,
  none of which resolve for the browser. The converter exposes no `external` knob and the
  skill forbids forking `lib/bundle.mjs`. To sync it later, the repo would need that call
  behind a dynamic `import()` guarded at runtime, or `sharedQueue` moved off the browser path.
- **`font: inherit` beats Tailwind.** The hand-rolled section of `styles.css` has an
  **unlayered** rule `input[type="text"], input[type="number"], select, textarea { font: inherit }`.
  Unlayered CSS outranks anything in `@layer utilities`, so `font-mono`/`text-xs` applied
  directly to a `<Textarea>` do nothing. Set the family on a **parent** and let it inherit.
  `Input` escapes this only because it renders with no `type` attribute, so the attribute
  selector does not match.
- **`MermaidBlock` drops NODE labels on flowcharts and state diagrams — a real product bug.**
  `scrubSvg` strips `<foreignObject>` (correctly: it is how HTML re-enters an SVG). But
  mermaid v11 renders flowchart and state-diagram *node* labels inside a `foreignObject`
  even at `securityLevel: 'strict'` with `flowchart.htmlLabels: false`, so those diagrams
  render as correctly-shaped but EMPTY boxes. Edge labels survive (they are `<text>`),
  which is what makes it easy to miss — the diagram looks half-right. Sequence diagrams
  label with `<text>` throughout and are unaffected, so both shipped preview cells are
  sequence diagrams. Worth fixing upstream: either stop scrubbing `foreignObject` and
  sanitise properly, or force `htmlLabels: false` per-diagram-type (mermaid v11 honours
  it under `flowchart`, `state`, `class`, … separately, not globally).
- **Three synced components read the zustand store, not props**: `SideNav`, `WorkerPane` and
  `FolderWorkbench`. The store is exported from the barrel so previews can seed it. It is a
  module SINGLETON: sibling cells in one card cannot hold different state, because a later
  `setState` re-renders every mounted consumer with the final value. Seed once at module
  scope and fold the interesting states into one dataset.
  (`Toolbar` and `CatalogPanel`, which this note used to name, no longer exist.)
- **The dead graph-editor CSS still ships** — 19 rules of it. `styles.css` retains the whole
  v1 canvas vocabulary (`.canvas`, `.actor-node`, `.io-*`, `.mapping-table`, `.link-panel`,
  the seed table, `.panel.inspector`) from the execution editor ADR 0023 §12 removed.
  **Do not confuse it with Scratch.** Scratch is a real canvas and a current one (ADR 0026),
  but it is drawn entirely with React Flow plus the `ScratchNode*` components and Tailwind —
  it touches none of those classes. The distinction the conventions header has to carry is
  not "no canvas" but: a Scratch is a DRAWING that produces code, and nothing on it starts a
  run. A play button on a node is the exact mistake ADR 0026 exists to prevent.

## Authoring previews for THIS package — what the 2026-08-17 wave learned

- **The review sheet is a downscale, and the raws are not.** Capture screenshots the whole
  900×700 story viewport (`fullPage: false`), then the sheet scales it to ~520px tall, so a
  216px node reads at ~120px. Grade from the sheet, but open
  `_screenshots/review/raw/<group>__<Name>__<Cell>.png` whenever a detail decides the verdict.
- **A cell that overflows the viewport is CLIPPED, not scrolled**, and the published card
  clips identically — `emit.mjs` gives the product card the same declared viewport. Measure
  the tall cells. `MethodCallPanel` needed `{"cardMode": "column", "viewport": "900x1100"}`
  because its generated caller is ~30 lines by construction and its source `<pre>` is
  `max-h-[46vh]`, so it GROWS with the viewport and the override has to overshoot.
  Note `viewport` is keyed into the grade contract: changing it re-opens that component's
  already-`good` cells for a re-read.
- **`preview-rebuild.mjs` does NOT run `cfg.buildCmd`.** Only `package-build.mjs` does. A
  preview reaching for a utility no app file uses compiles fine and collapses silently. Stay
  inside the app's vocabulary, or tell the orchestrator to recompile Tailwind.
- **A component that fetches on mount can still be previewed honestly.** `MethodCall` reads
  `GET /api/sources/workflow`; unstubbed the card shows the panel under a 404 banner the
  product never puts an operator in. `previews/MethodCall.tsx` installs a module-scope
  `globalThis.fetch` wrapper answering ONLY that route and delegating everything else. It is
  contained because each component gets its own card page loading only its own preview.
- **`previews/PaneThroughput.tsx` compresses the clock, on purpose.** The component owns its
  sampler (`SAMPLE_MS = 1000`) and `spark.ts`'s `SPARK_MIN = 2` draws nothing under two
  samples — deliberately, a flat line is a measured zero — while capture screenshots at
  `networkidle`, well under 2s. The preview patches `setInterval` so 1000ms callbacks fire at
  6ms for 48 ticks and then freeze. **The series itself is untouched**; only the wait is
  skipped. If the harness ever grows a >2s settle, delete the patch and keep the data.
- **Each cell is its own page load under capture, but the product's grid renders them all on
  one page.** Never let cells differ by a module-level mutable — last write wins there. Keep
  per-cell variation in props.
- **Composing furniture:** a `PaneResizer` is `h-1.5 bg-border/40` and vanishes over `bg-card`.
  What makes it read is panes on `bg-background` inside a `bg-card` stack, and border-only pane
  headers — a `bg-muted` header directly under the handle merges with it into one strip.
- **A Scratch note's textarea is `rows={3}` at 188px and does not grow**: budget ~20 characters
  per line, three lines, no long words.
- **`ScratchInspector` has no height of its own**, so a cell renders at natural height and
  nothing scrolls; ~634px is the measured ceiling before clipping. Frame it at ~372px rather
  than its own 304px so its right border reads as the seam against the canvas.

- **Measured capture geometry**: viewport 900×700, body padding 24, a `p-4` frame 16 ⇒ **820px of
  usable width and ~640px of usable height**. Compose to that, or ask for a `viewport` override.
- **A component that is `flex min-h-0 flex-1` all the way down needs a height-constrained FLEX
  parent**, not a padded block — inside an ordinary block frame every `min-h-0` goes inert and the
  card shows a layout the product never has. `FolderWorkbench`'s shell is
  `style={{ height: 632, display: 'flex' }}`.
- **`TileWall` does not mount terminals** — `renderTile` is a render prop, so a card composes the
  real `TileHeader` + `HealthChips` over a terminal-ground `div` and fakes nothing. It does need a
  DEFINITE height from its parent: its canvas is `flex-1` and has nothing to be one of otherwise.
- **`useTerminalStyle` reads `.dark` off `<html>`, and a preview's `.dark` is on its own wrapper**,
  so anything mounting `TerminalTile` first renders a bright white xterm inside a dark card. Seed
  `localStorage['kontra-dashboard-terminal-theme']` with
  `{"version":1,"palette":"night","fontSize":12,"sidebar":false}` — the same key `StyleControls`
  writes. Do NOT put `.dark` on `documentElement`: that is a shared write on the product's
  one-page grid.

- **A piece of chrome is only legible NEXT TO what it sizes.** A 6px `SideResizer` and a 28px
  `SideRail` both rendered as blank cards alone. The fix is composition, not a bigger swatch: panes
  on `bg-background` inside a `bg-card` shell, **border-only pane headers** (`border-b border-border`,
  never `bg-muted` — a muted header against `bg-border/40` merges with the handle into one strip),
  and the handle between two columns of real content. Those components' cards are page shells, which
  is why they all carry `cardMode: "column"`.
- **A state only a GESTURE reaches is seeded through the hook's own storage key.** `PaneStack`'s
  order and a folded pane, a collapsed or right-docked sidecar: write
  `localStorage['kontra.stack.<stack>']` / `['kontra.side.<pane>']` before render, in a try/catch,
  with a UNIQUE key per cell — the product's grid renders every cell on one page, so two cells
  sharing a key overwrite each other. `paneOrderKey`/`sideDockKey` are not on the barrel, so the
  string is written out in the preview.
- **Stack arithmetic**: every unfolded pane is `flex-1`, so a shell must be tall enough for
  `panes × (header + body lines)` — ~324px for three panes of four lines, ~300 for two. Three panes
  in 280px clips the first body mid-line.

## Product observations the 2026-08-17 sync surfaced (not preview problems)

Each was found by composing a card and looking at it, and each is real:

1. ~~**`languageOf` has no case for `.go`.**~~ **FIXED 2026-08-18** — `@codemirror/lang-go` is a
   dependency now, `languageOf` returns `{id:'go'}` for `.go`, and its indent unit is a TAB (gofmt
   uses tabs; four spaces produce a file gofmt rewrites on the next save). `go.mod`/`go.sum` stay
   `Text` deliberately — they are their own grammars and borrowing Go's lexer paints them wrong.
2. **The editor sets no `EditorView.lineWrapping`.** Right for code — and it means `description.md`,
   which is prose, scrolls horizontally in the left half while its preview wraps on the right. The
   one file the workbench exists to get written is the one the editor is configured wrong for.
3. **`RegisterFolder` enables `add` on the prefilled root alone** (`path.trim() !== ''`, prefill
   `${defaultRoot}/`), so pressing add on open submits `~/.kontra/actors/` and is refused for a path
   the operator never chose.
4. **`PaneFilterBar`'s three `<select>`s lose their `font-mono`** to the unlayered
   `select { font: inherit }` rule, beside a query input that keeps it — two controls in one row
   disagreeing about their typeface.
5. **The workbench and the card disagree about an absent folder.** The card withholds `edit` when
   the directory is gone (every read 400s); `FolderWorkbench`, reached before it vanished, still
   draws a live Serve button. The serve's own refusal catches it, so nothing breaks.

## Bundle characteristics

- `_ds_bundle.js` is ~8.5 MB unminified. ~6 MB of that is **mermaid** and its ecosystem
  (mermaid, `@mermaid-js/parser`, cytoscape, katex, dagre-d3-es, d3-*), pulled in as a
  *static* import by `Markdown` → `MermaidBlock`. It cannot be dropped without dropping the
  markdown/widget layer. `lucide-react` contributes ~1.5 MB (the icon set does not
  tree-shake here).
- `_ds_bundle.css` is ~1.5 MB — CSS that JS modules import (katex etc.), separate from the
  53 KB compiled Tailwind in `cssEntry`.

## Environment

- Node v22, pnpm 10.33.2. Install with `pnpm i --frozen-lockfile` in `frontend`.
- Render check needs playwright matched to a **cached** chromium: `playwright@1.61.0`
  pins chromium-1228, which is in `~/.cache/ms-playwright`. `1.62.1` pins 1234 and is NOT
  cached — installing it fails with "Executable doesn't exist".
- Converter deps live in `.ds-sync/` (gitignored): `esbuild ts-morph @types/react
  @tailwindcss/cli@4.3.2 playwright@1.61.0`.

## Known render warns (triaged as legitimate)

- The three `[RENDER_BLANK]` warns on the first build (Badge, Card, Textarea) were unauthored
  floor cards and disappeared once previews were authored.
- **`[RENDER] WidgetView: root empty` is a FLAKE, not a defect — do not chase it.** Seen once on
  2026-08-18, gone on an unchanged re-run. Its screenshot rendered all three cells perfectly while
  `.render-check.json` said `rootEmpty:true, maxHeight:0, texts:["","",""]` with **zero** page
  errors. `package-validate.mjs` navigates with `waitUntil:'networkidle'` and then immediately
  `page.evaluate`s the roots; the screenshot is taken later. WidgetView is the heaviest card
  (`Markdown` → `MermaidBlock` pulls ~6 MB of mermaid at module scope) and the bundle is now
  **10.8 MB**, so its mount can finish after the probe. If it fires: look at
  `_screenshots/widgets__WidgetView.png` FIRST — a good screenshot with an empty `texts` array is
  this race, and a re-run clears it. It gets worse as the bundle grows.

## A product change can invalidate a preview while the diff says nothing changed

**Learned 2026-08-18, and it is the most useful thing this sync found.** The verdict read
`0 changed, 0 new, 0 removed — 45 verified-by-upload`, and one card was nevertheless broken.

Grades key on `sourceKeys` — the authored `.tsx`, the `.d.ts`, the `.prompt.md` — and **bundle churn
deliberately never invalidates them**. That is right for styling and pipeline noise, and it is blind
to exactly one thing: a change to a component's own code that makes an existing preview illegal.
Here `ActorCard.folder` went from optional to REQUIRED (the "no folder, no Actor" rule), three
preview cells were still passing `folder={undefined}`, and nothing in the diff could know.

**The render check is the only thing that catches this**, and it did — 9 × `TypeError: Cannot read
properties of undefined (reading 'path')`. So: on a re-sync where the product changed but the diff
reports `0 changed`, the render check is not a formality, it is the whole gate. Read
`.render-check.json` before believing a clean-looking verdict.

The fix is always to make the preview tell the truth, never to paper over it: the cell demonstrating
"an Actor with no registered folder" was DELETED, because the product no longer has that state —
the Actors page is one row per registered folder now. `NoCodeOnThisDisk` keeps only the
folder-is-gone case, and its comment records why the other half went.

## Re-sync risks — what can silently go stale

- **The barrel is maintained by hand.** A component renamed, moved or deleted in
  `src/panels/**` will break `.ds-entry.ts` at bundle time (loud), but a *newly added*
  component is silently absent from the design system until someone adds it to both the
  barrel and `componentSrcMap`. There is no drift check.

  **Two are absent right now (2026-08-18), both added the same day:** `RunTail`
  (`src/panels/RunTail.tsx` — a run's event tail, heartbeats and materialization, folded and
  remembered; it is what made "press Run" stop navigating away) and `ActorFilterBar`
  (`src/panels/ActorFilterBar.tsx`). Adding either needs an export in `.ds-entry.ts` AND an entry
  in `componentSrcMap`, then a preview in `.design-sync/previews/` or it ships a floor card.
  Deliberately not added in this sync — the user was mid-merge and it needs a full rebuild plus
  re-upload. A one-liner that finds the rest:

  ```sh
  node -e "const fs=require('fs'),c=require('./.design-sync/config.json');const m=new Set(Object.keys(c.componentSrcMap));
  for(const d of ['src/panels','src/panels/chrome','src/panels/grid','src/panels/widgets','src/components','src/components/ui'])
    for(const f of fs.readdirSync('frontend/'+d)) { const n=f.replace(/\.tsx$/,'');
      if(f.endsWith('.tsx')&&!f.includes('.test.')&&/^[A-Z]/.test(n)&&!m.has(n)) console.log(d+'/'+f); }"
  ```

  It reports ~16 names; most are PAGES (`ActorsPage`, `WorkflowsPage`, …) or multi-export files whose
  named exports are already mapped (`ScratchNodes` → `ScratchNode*`, `SideDock` → `SideRail` /
  `SideResizer` / `SideDockControls`, `RegisteredFolders` → `FolderActions` / `FolderAbsent`,
  `WorkbenchPanes` → `WorkbenchFiles` / `ServeConsole`, `MethodContract` → `MethodRow`,
  `MethodCallPanes` → `MethodCallPanel`). Read it as a candidate list, never as a TODO.
- **The Tailwind wrapper duplicates nothing but assumes paths.** `.ds-tailwind.css`
  hardcodes `./src` and `../../.design-sync/previews`. Moving either breaks styling
  *silently* — utilities just stop being generated.
- **`font: inherit` and the dead canvas CSS are documented in `conventions.md`.** If
  `styles.css` is ever cleaned up, re-validate those claims — a conventions file naming
  classes that no longer exist is worse than none.
- **Groups are derived from directory names.** The five shadcn primitives land in `general`
  because `components/ui` is entirely generic-dir names. If a nicer grouping is wanted,
  give them real per-component docs with `category:` frontmatter (a frontmatter-only stub
  would trip `[PROMPT_EMPTY]`).
- Preview content embeds **illustrative** kontra data (actor names, run ids, queue names,
  counts). It is realistic, not real, and will not track the product.
