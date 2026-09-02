// design-sync entry point for @kontra/orchestrator-web.
//
// WHY THIS FILE EXISTS: the web package is a private Vite SPA with no library
// build and no barrel — its components are reached by deep path from `App.tsx`.
// The converter's fallback entry emits `export * from '<file>'` per source file,
// and `export *` does NOT re-export defaults; nearly every chrome component here
// is `export default`. Without this file they would be discovered by name and
// then be absent from `window.KontraDashboard`, so every preview card would fail
// with "Element type is invalid".
//
// This re-exports the real shipped components under stable named exports. It
// adds no markup and reimplements nothing. Scope is the five surfaces' reusable
// vocabulary — the shadcn primitives, the control-plane chrome, and the pieces
// the Actors and Workflows pages (including the sketch tab) are built out of.
//
// The PAGE components (ActorsPage, WorkflowsPage, DashboardPage, DatasetPage,
// SettingsPage) are deliberately excluded: each fetches on mount and owns live
// sockets, xterm or ag-grid, so a card would render an empty shell. The design
// agent composes pages; it does not import one.
//
// Kept in sync by hand. Adding a component here plus an entry in
// `.design-sync/config.json` `componentSrcMap` is what puts a card in the
// Claude Design project.

// React Flow's own stylesheet, which `main.tsx` imports for the same reason: it
// positions every `Handle` on its node's border. Without it the scratch nodes
// render with their ports stacked at the origin — so it has to reach a design
// through `_ds_bundle.css`, and a JS-side import is what puts it there.
import '@xyflow/react/dist/style.css';

// ── shadcn/ui primitives ────────────────────────────────────────────────────
//
// THREE, NOT FIVE. `Card` and `Textarea` were exported here for eight months after their
// implementations were deleted, and this file is why nobody noticed: `298c8d6` removed
// `src/components/ui/card.tsx` and `src/components/ui/textarea.tsx` on the evidence "no
// `from '…/card'` anywhere", having — its own message says so — "scanned every module under
// web/src". This barrel sat one directory ABOVE `web/src` and imported both. It is the second
// entry point of this package, and a sweep that walks only from `src/main.tsx` cannot see it.
//
// Nothing rendered a `<Card>` or a `<Textarea>` then and nothing does now, so the deletion was
// right and only its evidence was short. What is restored here is the barrel's own contract,
// stated at the top of this file: it re-exports the REAL SHIPPED components and reimplements
// nothing. A primitive that exists only for the design system is markup this app does not have.
//
// The break was invisible to `pnpm run typecheck` because `tsconfig.json`'s `include` listed
// `src`, `e2e` and `playwright.config.ts` — not this file. It lists this file now, so the next
// deletion of something the barrel names fails at the compiler instead of at a sync.
export { Badge, badgeVariants } from './src/components/ui/badge';
export { Button, buttonVariants } from './src/components/ui/button';
export { Input } from './src/components/ui/input';

// ── Chrome: status, headers, navigation, pane furniture ─────────────────────
export { default as StatusBar, STALE_SNAPSHOT_MS } from './src/panels/chrome/StatusBar';
export type { StatusBarProps, WallStats } from './src/panels/chrome/StatusBar';

export { default as TileHeader } from './src/panels/chrome/TileHeader';
export type { TileHeaderProps } from './src/panels/chrome/TileHeader';

export { default as TileMenu } from './src/panels/chrome/TileMenu';
export type { TileMenuItem, TileMenuProps } from './src/panels/chrome/TileMenu';

export { default as SidebarTree } from './src/panels/chrome/SidebarTree';
export type { SidebarTreeProps } from './src/panels/chrome/SidebarTree';

export { default as StyleControls } from './src/panels/chrome/StyleControls';

// The drag handle every stacked pane is separated by. `usePaneHeight` is the
// hook that remembers a pane's height across mounts — a resizer without it
// snaps back to the default the next time the pane is drawn.
export { PaneResizer, usePaneHeight } from './src/panels/chrome/PaneResizer';
export type { PaneResizerProps } from './src/panels/chrome/PaneResizer';

export { default as PaneFilterBar } from './src/panels/chrome/PaneFilterBar';
export type { PaneFilterBarProps } from './src/panels/chrome/PaneFilterBar';

// The vocabulary both of those are steered by. Without it a caller has to spell the
// "do not narrow" sentinel as the bare string `'*'`, and the whole point of the
// sentinel is that it is NOT the empty string: a filter built with `''` silently
// drops every Terminal whose actor is unregistered. `paneOptions` is how the menus
// are meant to be built — from the live inventory, never from a fixed list — and
// PANE_BOUNDS/PANE_STEP are the floor and the keyboard increment a resizer works to.
export {
  ANY,
  EMPTY_FILTER,
  filterActive,
  paneOptions,
} from './src/panels/paneFilter';
export type { PaneFilter } from './src/panels/paneFilter';
export { PANE_BOUNDS, PANE_STEP, clampPaneHeight } from './src/panels/chrome/paneHeight';
export type { PaneBounds, PaneGrow } from './src/panels/chrome/paneHeight';

export { default as PaneThroughput } from './src/panels/chrome/PaneThroughput';

// A sidecar that resizes, folds to a rail, and moves to the other edge — the horizontal
// twin of PaneResizer. Every sidecar on the app was a pinned width until this; a design
// built with a fixed-width panel is one an operator cannot fit their own screen.
export { SideResizer, SideRail, SideDockControls, useSideDock } from './src/panels/chrome/SideDock';
export type { SideResizerProps, SideDock } from './src/panels/chrome/SideDock';
export {
  SIDE_BOUNDS,
  SIDE_STEP,
  clampSideWidth,
  dragWidth,
  flipSide,
  keyWidth,
} from './src/panels/chrome/sideDock';
export type { DockSide, SideBounds, SideDockState } from './src/panels/chrome/sideDock';

// A column of panes an operator can drag into a different order and fold individually.
// The order is stored BY KEY, never by index — a stored `[2,0,1]` silently permutes a
// different set of panes the moment one is added or renamed.
export { PaneStack, usePaneOrder } from './src/panels/chrome/PaneStack';
export type { StackedPane } from './src/panels/chrome/PaneStack';
export { moveBefore, nudge, reconcileOrder } from './src/panels/chrome/paneOrder';
export type { PaneOrder } from './src/panels/chrome/paneOrder';

// ── Grid: the tile wall ─────────────────────────────────────────────────────
export { default as TileWall, WallEmpty } from './src/panels/grid/TileWall';
export type { TileWallProps } from './src/panels/grid/TileWall';

// ── Widgets: rendered content ───────────────────────────────────────────────
export { default as WidgetView } from './src/panels/widgets/WidgetView';
export type { WidgetViewProps } from './src/panels/widgets/WidgetView';

export { default as Markdown, MARKDOWN_TEXT_CAP } from './src/panels/widgets/Markdown';
export type { MarkdownProps } from './src/panels/widgets/Markdown';

export { default as MermaidBlock } from './src/panels/widgets/MermaidBlock';
export type { MermaidBlockProps } from './src/panels/widgets/MermaidBlock';

// ── Catalog: an Actor, its Methods, and calling one ─────────────────────────
export { ActorCard } from './src/panels/ActorCard';

// The Actors grid's filter row — the Monitor's bar with this page's nouns, kept
// deliberately alike so an operator who learns one filter does not meet a second
// grammar on the next surface. Its count is never conditional: a filter that
// hides eight cards must not read as a page that lost eight.
export { ActorFilterBar } from './src/panels/ActorFilterBar';
export type { ActorFilterBarProps } from './src/panels/ActorFilterBar';
export { EMPTY_ACTOR_FILTER, actorNames, matchActor } from './src/panels/actorFilter';
export type { ActorFilter, ActorStanding, FilterableActor } from './src/panels/actorFilter';

// One Method row: name, description, and the `takes → emits` summary, with the
// field tables under a disclosure. `schemaFields`/`schemaType`/`summarise` are
// the SINGLE flattener — the Scratch canvas renders its typed ports through
// them too, so a field is named the same wherever it is drawn.
export {
  MethodRow,
  summarise,
  schemaFields,
  schemaType,
} from './src/panels/MethodContract';
export type { SchemaField } from './src/panels/MethodContract';

export { MethodCall } from './src/panels/MethodCall';
export { MethodCallPanel } from './src/panels/MethodCallPanes';

export { WorkflowContract } from './src/panels/WorkflowContract';

// ── Workbench: a registered folder, its files, and serving it ───────────────
export { FolderWorkbench } from './src/panels/FolderWorkbench';
export { RegisterFolder } from './src/panels/RegisterFolder';
// `FolderList` is GONE — it was a shelf of folder rows drawn above the Actors grid and
// below the Workflows list, which on an ordinary installation is the folders those
// surfaces already name, printed a second time in a second vocabulary. The path is a line
// on the row that names it now, and these are the affordances that had to stay one
// implementation: a DISCOVERED folder has no registration to remove, and a second copy
// offering the button anyway would be an affordance that exists only to 400.
export { FolderActions, FolderAbsent, useRegisteredFolders } from './src/panels/RegisteredFolders';
export type { FolderShelf } from './src/panels/RegisteredFolders';
export { WorkbenchFiles, ServeConsole } from './src/panels/WorkbenchPanes';

// ── Scratch: the canvas ─────────────────────────────────────────────────────
// The node components are reached through the registry React Flow looks a
// node's `type` up in, because that registry is the export — pulling them out
// by name here keeps `scratchNodeTypes` the single source of the mapping.
import { scratchNodeTypes } from './src/panels/ScratchNodes';
export const ScratchNodeActor = scratchNodeTypes.actor;
export const ScratchNodeWorkflow = scratchNodeTypes.workflow;
export const ScratchNodeDataset = scratchNodeTypes.dataset;
export const ScratchNodeNote = scratchNodeTypes.note;

export {
  scratchNodeTypes,
  ScratchActionsProvider,
  ScratchPortsProvider,
} from './src/panels/ScratchNodes';
export type { ScratchNodeActions } from './src/panels/ScratchNodes';

export { ScratchInspector } from './src/panels/ScratchInspector';

// React Flow itself, so a preview and the bundled nodes share ONE module
// instance. `Handle` reads the React Flow store through context: a preview that
// imported its own copy would render nodes whose handles throw. Excluded from
// the card index in config.json — these are third-party, not kontra's design.
export {
  ReactFlow,
  ReactFlowProvider,
  Background,
  Controls,
  MiniMap,
} from '@xyflow/react';

// ── Panels: navigation, health, state, provenance ───────────────────────────
export { SideNav } from './src/panels/SideNav';
export { StatePill } from './src/panels/StatePill';
export { WorkerPane } from './src/panels/WorkerPane';

export {
  default as HealthChips,
  CHIP_SIGNALS,
  DETAIL_SEPARATOR,
  chipState,
  detailLines,
} from './src/panels/HealthChips';
export type { ChipSignal, ChipState, HealthChipsProps } from './src/panels/HealthChips';

export { default as DatasetProvenance } from './src/panels/DatasetProvenance';
export type { DatasetProvenanceProps } from './src/panels/DatasetProvenance';

// DetailDrawer is DELIBERATELY ABSENT. It imports `sharedQueue` from
// `@core/panels/pollers`, which lazy-loads Temporal via `require('@temporalio/client')`
// inside a function — deliberately, so the module's TYPE graph stays browser-safe.
// Vite/rollup leave that call alone, so the real app is unaffected; esbuild's
// bundler follows `require()` statically and drags in @grpc/grpc-js, node:crypto
// and node:async_hooks, which cannot resolve for the browser. The converter
// exposes no `external` knob. See .design-sync/NOTES.md.

// ── Sparklines ──────────────────────────────────────────────────────────────
// A MEASURED series or nothing: every sparkline here draws points that were
// observed. A slot with no producer is drawn as absent, never as plausible.
export { Spark, Streak, SPARK_COLORS } from './src/components/Spark';

// The app store. Anything that reads its data from the store rather than from
// props has to be seeded by whatever renders it, including a preview card:
//   useAppStore.setState({ catalog: [...], view: 'actors' })
export { useAppStore } from './src/state/store';

// ── Design vocabulary (not components — the contracts the chrome reads) ─────
// The Dataset lifecycle is a tracked contract: open / sealed / abandoned, plus
// the absence of one. Three states, three visual treatments, never collapsed
// into two — `datasetBadge` is the single place that mapping lives.
export {
  DATASET_STATES,
  datasetBadge,
  datasetState,
} from './src/datasets/state';
export type { DatasetState } from './src/datasets/state';
