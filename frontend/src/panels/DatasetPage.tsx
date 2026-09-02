/**
 * Datasets: every one ever written, and the SQL console on the inside of each.
 *
 * EVERYTHING IS A DATASET. An actor's output for one dispatch and an operator-loaded list are
 * listed side by side and addressed the same way — by NAME, plus version and dispatch time when
 * they exist. No run ids, no graph node ids, no table hashes: a five-shard dispatch of one actor is
 * one dataset here, exactly as it is in `kontra dataset list`.
 *
 * THE QUERY WORKBENCH USED TO BE ITS OWN SURFACE, and folding it in here is a correction. It had a
 * schema tree down the left listing the same datasets this page listed, so an operator picked a
 * dataset twice: once to look at it, and again — by typing its name — to query it. Opening one now
 * IS opening its console. The schema shown is that dataset's, `SELECT * FROM <it>` is already in
 * the editor, and the engine, the export routes and `run/query.ts` are all unchanged; only the way
 * in moved.
 *
 * A LISTING ROW IS A TABLE ROW, NOT A CARD. Cards were pretty and unreadable at scale: a real
 * run has dozens of datasets, and the questions asked of the list — which is biggest, which is
 * newest, which is still open — are comparisons, and comparisons want columns.
 *
 * THE ENGINE IS ON THE SERVER. This page holds a text editor and a grid; the SQL travels to
 * `/api/datasets/query` and rows come back as JSON. It used to run DuckDB *in the browser* — 76 MB
 * of WebAssembly, re-downloaded per visit — so that it could range-read presigned parquet.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
  Check,
  Copy,
  Download,
  ExternalLink,
  Pencil,
  Play,
  Plus,
  RefreshCw,
  Rows2,
  Rows3,
  Tag,
  Trash2,
  X,
} from 'lucide-react';
import { AgGridReact } from 'ag-grid-react';
import type { ColDef, ICellRendererParams, ValueGetterParams } from 'ag-grid-community';
import CodeMirror from '@uiw/react-codemirror';
import { sql as sqlLang, type SQLNamespace } from '@codemirror/lang-sql';
import type { DatasetInfo } from '../run/api';
import {
  addDatasetTag,
  deleteDataset,
  removeDatasetTag,
  renameDataset,
  resetDatasetName,
  type DatasetDeviation,
} from '../run/api';
import { Spark } from '../components/Spark';
import {
  EXPORT_FORMATS,
  downloadExport,
  fetchSchema,
  runQuery,
  type ExportFormat,
  type QueryResult,
  type SchemaEntry,
} from '../run/query';
import { queryCommand } from '../run/dataset';
import {
  DATASET_STATES,
  datasetBadge,
  datasetState,
  type DatasetBadgeState,
} from '../datasets/state';
import { fetchProvenance, type DatasetProvenance } from '../datasets/provenance';
import { datasetExpiry } from '../datasets/expiry';
import { scopeWords } from '../datasets/scope';
import { duckdbText, inspect } from '../datasets/cells';
import {
  groupDatasetName,
  groupDatasets,
  runText,
  runTitle,
  wholeDataset,
  type DatasetGroup,
} from '../datasets/grouped';
import {
  dispatchCell,
  listingRowId,
  listingRows,
  tagMeaning,
  type DatasetListingRow,
} from '../datasets/listing';
import DatasetProvenancePanel from './DatasetProvenance';
import DatasetAccrual from './DatasetAccrual';
import { LiveRowTail } from './LiveRowTail';
import { gridTheme } from '../lib/agGrid';
import { useAppStore } from '../state/store';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';

/** Rows one Run fetches. The server clamps to its own ceiling regardless. */
const PAGE_ROWS = 1000;

/** How tall a row gets in `tall` mode. Four lines of a markdown body is enough to tell what it is
 *  and to decide whether to open it; more than that and the grid stops being a grid. */
const TALL_ROW_HEIGHT = 84;

function when(ms?: number): string {
  return ms ? new Date(ms).toLocaleString() : '';
}

/** Byte sizes as an operator reads them — the catalog reports exact bytes. */
export function fmtBytes(n: number): string {
  if (!n) return '';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  let v = n;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i += 1;
  }
  return `${v < 10 && i > 0 ? v.toFixed(1) : Math.round(v)} ${units[i]}`;
}

/** Stable identity for a dataset — the same triple that addresses it. */
export function keyOf(d: Pick<DatasetInfo, 'kind' | 'name' | 'version' | 'dt'>): string {
  return `${d.kind}:${d.name}:${d.version ?? ''}:${d.dt ?? ''}`;
}

/** A dataset name that needs quoting to be a legal bare identifier. */
export function quoteIfNeeded(name: string): string {
  return /^[A-Za-z_][A-Za-z0-9_]*$/.test(name) ? name : `"${name.replace(/"/g, '""')}"`;
}

/**
 * The query an opened dataset starts with. It is the one every operator types first, and having
 * it already run is the difference between a console and a blank box.
 *
 * SCOPED TO ONE RUN WHEN THERE IS ONE. A Dataset name spans **Runs**, so `SELECT * FROM lame`
 * reads all 1,246 rows of it while the panel above may be scoped to the 623 one Run wrote. The
 * `WHERE run_id = …` is what keeps the grid and the counts describing the same rows — and it is
 * the query an operator would otherwise have to know the column name to write.
 */
export function openingQuery(name: string, run: string | null = null): string {
  const t = quoteIfNeeded(name);
  if (!run) return `SELECT *\nFROM ${t}\nLIMIT 100;`;
  return `SELECT *\nFROM ${t}\nWHERE run_id = '${run.replace(/'/g, "''")}'\nLIMIT 100;`;
}

/**
 * The queries an operator types after the first one.
 *
 * Built from the dataset's OWN first column rather than from a fixed name, because a snippet that
 * references a column this table does not have is a Binder Error dressed up as a shortcut. Three,
 * not ten: a snippet list long enough to need reading is slower than typing.
 */
export function snippets(name: string, firstColumn: string): Array<{ label: string; sql: string }> {
  const t = quoteIfNeeded(name);
  const c = quoteIfNeeded(firstColumn);
  return [
    { label: 'count the rows', sql: `SELECT count(*) AS rows\nFROM ${t};` },
    {
      label: `group by ${firstColumn}`,
      sql: `SELECT ${c}, count(*) AS n\nFROM ${t}\nGROUP BY 1\nORDER BY n DESC\nLIMIT 100;`,
    },
    { label: `filter on ${firstColumn}`, sql: `SELECT *\nFROM ${t}\nWHERE ${c} LIKE '%'\nLIMIT 500;` },
  ];
}

// --- saved SQL presets --------------------------------------------------------------------------

/**
 * A query an operator wrote and KEPT. The built-in `snippets` are throwaway starting points; a
 * preset is the join / filter / group-by they actually composed and want back after closing the
 * dataset. Scoped to a dataset by default so a name means the same thing every time it is applied;
 * a preset with no `dataset` is global and shows on every console.
 */
export interface SqlPreset {
  name: string;
  sql: string;
  dataset?: string;
}

/** Where presets live. localStorage is enough: they are one operator's own shortcuts, not shared
 *  state, and losing them costs a retype, not a run. */
export const PRESETS_KEY = 'kontra.dataset.presets';

/** The presets to offer on one dataset's console: its own, plus the global (dataset-less) ones. A
 *  preset built for `lame` must not appear on `domains`, where its column names are a Binder Error
 *  dressed up as a shortcut — the same reason `snippets` are built from the dataset's own columns. */
export function visiblePresets(all: SqlPreset[], datasetName: string): SqlPreset[] {
  return all.filter((p) => p.dataset === undefined || p.dataset === datasetName);
}

/** Add or replace by identity, which is the (name, dataset) pair. Re-saving under a name that
 *  already exists on THIS dataset overwrites it rather than growing a second one — the operator
 *  asked to keep "the domains query", not to accumulate every version of it. */
export function upsertPreset(all: SqlPreset[], preset: SqlPreset): SqlPreset[] {
  const rest = all.filter((p) => !(p.name === preset.name && p.dataset === preset.dataset));
  return [...rest, preset];
}

/** Drop one preset by identity, leaving every other one untouched — applying or deleting one must
 *  never disturb the rest of the saved set. */
export function removePreset(all: SqlPreset[], preset: Pick<SqlPreset, 'name' | 'dataset'>): SqlPreset[] {
  return all.filter((p) => !(p.name === preset.name && p.dataset === preset.dataset));
}

/** Read the saved presets, tolerating a browser with no storage and a corrupt or foreign value — a
 *  garbled key must degrade to "no presets", never throw on the way to opening a console. */
export function loadPresets(): SqlPreset[] {
  try {
    const raw = globalThis.localStorage?.getItem(PRESETS_KEY);
    if (!raw) return [];
    const parsed: unknown = JSON.parse(raw);
    if (!Array.isArray(parsed)) return [];
    return parsed.filter(
      (p): p is SqlPreset =>
        !!p &&
        typeof (p as SqlPreset).name === 'string' &&
        typeof (p as SqlPreset).sql === 'string'
    );
  } catch {
    return [];
  }
}

export function savePresets(all: SqlPreset[]): void {
  try {
    globalThis.localStorage?.setItem(PRESETS_KEY, JSON.stringify(all));
  } catch {
    /* storage full or blocked — the presets stay in memory for this session */
  }
}

// --- deleting a temporary -----------------------------------------------------------------------

/**
 * The confirmation an operator reads before a temporary Dataset is dropped.
 *
 * Deletion is a real destructive act on data, so it NAMES what goes — the Dataset, its rows and
 * size, and the Run that owns it — rather than asking a bare "are you sure?". A person deciding
 * whether a temp can go needs to see, at the moment of the decision, exactly what they are freeing.
 *
 * THE `open` CASE IS THE DANGEROUS ONE, and gets an extra sentence. An open temp may belong to a Run
 * still in flight — its producer may be appending to it right now — so dropping it pulls the
 * destination out from under a live run. The orphan policy is explicit-only (slice 03): a
 * person decides, and the route allows it, so the UI does not REFUSE an open temp — it makes the
 * consequence explicit here, which is the whole difference between a single silent click and a
 * deliberate one.
 *
 * A TAGGED TEMP GETS ITS OWN SENTENCE, and it is the one place the two things that can remove a
 * Dataset are reconciled in front of the person deciding. A tag means KEEP (ADR 0029 §3) and the
 * page says so on the chip — but a tag does NOT protect a temp from this button: the record is
 * keyed by the **Run** and the owner marker is what makes a temp deletable (ADR 0028), so the
 * explicit verb still removes it. Silently deleting something the page has been calling "kept" is
 * exactly the disagreement §5 exists to prevent, so the tags are NAMED here instead.
 */
export function deletionConfirm(args: {
  name: string;
  rows: number;
  bytes: number;
  owner?: string;
  state: DatasetBadgeState;
  /** The tags on the owning Run's record, if any — named because a tag reads as "kept". */
  tags?: readonly string[];
}): string {
  const size = fmtBytes(args.bytes) || 'no data yet';
  const owned = args.owner ? `, owned by run ${args.owner}` : '';
  const lines = [
    `Delete temporary Dataset "${args.name}"?`,
    `${args.rows.toLocaleString()} row${args.rows === 1 ? '' : 's'} · ${size}${owned}.`,
    'This frees its rows and its catalog entry, and cannot be undone.',
  ];
  const tags = args.tags ?? [];
  if (tags.length > 0) {
    lines.push(
      `This temporary is TAGGED ${tags.map((t) => `"${t}"`).join(', ')} — a tag means KEEP, and it ` +
        'does not stop this delete: the tag is on the owning Run, and a temporary Dataset is ' +
        'removed by asking, never by the retention sweep. The Run’s durable output keeps the tag.'
    );
  }
  if (args.state === 'open') {
    lines.push(
      'This temporary is still OPEN — a Run may be appending to it right now. Deleting it pulls the ' +
        'destination out from under that run; its remaining output will have nowhere to land.'
    );
  }
  return lines.join('\n\n');
}

// --- the results grid ---------------------------------------------------------------------------

/** The per-column defaults, shared as ONE frozen object for the grid's whole life. This was an
 *  inline `{ flex: 1 }` rebuilt on every render; `flex` re-shares column widths, so the app-level
 *  catalog poll (every 2 s while any Run is live re-renders this tree) silently dropped the
 *  operator's resize, sort, filter and scroll on each tick. A stable reference is what stops AG Grid
 *  re-applying it — a new object, even with identical contents, counts as a changed prop. */
export const GRID_DEFAULT_COL_DEF: ColDef = Object.freeze({ flex: 1 });

const EMPTY_COLUMNS: QueryResult['columns'] = [];
const EMPTY_DATASETS: DatasetInfo[] = [];

/** A result's SHAPE, independent of its rows: the column names and their types. The grid's column
 *  definitions are a function of this and nothing else, so a re-query returning the same shape — and
 *  every catalog tick, which does not touch the result at all — must hand AG Grid the SAME defs, and
 *  it keeps the operator's column state. Keying the rebuild on `result` identity was the bug. */
export function schemaSignature(columns: ReadonlyArray<{ name: string; type: string }>): string {
  return columns.map((c) => `${c.name}\0${c.type}`).join('\x01');
}

/**
 * EVERYTHING the results grid's column definitions depend on, as one value — the memo's whole key.
 *
 * IT IS A FUNCTION, AND ITS PARAMETERS ARE THE CONTRACT. The rebuild used to key on the `result`
 * object, so every re-query and every 2 s catalog tick handed AG Grid new defs and took the
 * operator's widths, sort, filter and scroll with them (issue #14). Keying on the schema SIGNATURE
 * fixed that; making the key a NAMED FUNCTION is what keeps the fix legible now that this page has a
 * LIVE ROW COUNT beside the grid — the count is not a parameter here, cannot become one without
 * changing this signature, and a suite asserts that a count landing leaves this value identical.
 *
 * The row-height mode is in it because it genuinely changes the defs (`wrapText`), and it changes
 * only when an operator presses the button.
 */
export function resultsGridKey(
  columns: ReadonlyArray<{ name: string; type: string }>,
  tall: boolean
): string {
  return `${schemaSignature(columns)}\u0002${tall ? 'tall' : 'dense'}`;
}

/** Build the grid's column definitions for one schema. Extracted so the memo can key on the schema
 *  signature rather than the result object; see `GRID_DEFAULT_COL_DEF` for why identity matters. */
export function buildColumnDefs(
  columns: ReadonlyArray<{ name: string; type: string }>,
  tall: boolean
): ColDef[] {
  return columns.map((c) => ({
    field: c.name,
    headerTooltip: c.type,
    // DUCKDB'S OWN NOTATION. This was `JSON.stringify`, which is the right value in the wrong
    // spelling: `kontra dataset query` and the duckdb CLI print `{'ns': a.example.com}` for the
    // same row, and two spellings across two windows onto one Dataset is a thing to translate
    // rather than read. See `datasets/cells.ts`.
    valueFormatter: (p) => duckdbText(p.value),
    // The whole value on hover, before deciding whether to open it.
    tooltipValueGetter: (p) => duckdbText(p.value),
    // NO TYPE INFERENCE, which is what was overriding the formatter above. AG Grid infers a
    // column's type from its first value and gives a boolean one a CHECKBOX renderer — so
    // `lame.ok` drew tick boxes where DuckDB prints `true` and `false`, and a `NULL` boolean
    // drew an unticked box indistinguishable from a measured `false`. That last part is the
    // reason this is not a matter of taste.
    cellDataType: false,
    sortable: true,
    filter: true,
    resizable: true,
    minWidth: 120,
    // In `tall` mode the text wraps instead of being clipped. `autoHeight` is deliberately NOT
    // used: it measures every rendered cell, and on a thousand rows of crawl output that is a
    // layout pass per cell — a fixed taller row shows four lines for free.
    ...(tall ? { wrapText: true, cellClass: 'kontra-cell-wrap' } : {}),
  }));
}

export default function DatasetPage() {
  const [selected, setSelected] = useState<DatasetInfo | null>(null);
  /** The Run the console should OPEN scoped to. The console owns its scope from there on. */
  const [focusRun, setFocusRun] = useState<string | null>(null);

  // ARRIVED FROM A RUN. The Runs surface links a Dataset it wrote to this page, carrying the run
  // it was showing — so the console opens on the Dataset's TOTAL with that run's contribution
  // named, which is the round trip the count labelling exists for. The focus is consumed once and
  // cleared, like the Monitor's: it is a navigation, not a selection.
  //
  // It waits for the listing rather than fabricating a row: a Dataset is opened by its listing
  // row (rows, bytes, dispatch, lifecycle all come from it), and the app-level poll is what has
  // one. Until then nothing happens and the list renders as usual.
  // Subscribe to the catalog ONLY while a focus is pending. With no focus — which is the whole time
  // a console is open — this returns a stable empty array, so the app-level poll (every 2 s while a
  // Run is live) no longer re-renders this parent and cascades a twitch into the open DatasetConsole
  // below. The listing keeps its own live subscription; this one exists solely to resolve a focus.
  const focus = useAppStore((s) => s.datasetFocus);
  const datasets = useAppStore((s) => (s.datasetFocus ? s.datasets : EMPTY_DATASETS));
  const clearFocus = useAppStore((s) => s.clearDatasetFocus);
  useEffect(() => {
    if (!focus) return;
    const row = datasets.find(
      (d) => d.name === focus.name && (focus.kind === undefined || d.kind === focus.kind)
    );
    if (!row) return;
    setSelected(row);
    setFocusRun(focus.run ?? null);
    clearFocus();
  }, [clearFocus, datasets, focus]);

  return selected ? (
    <DatasetConsole
      // The scope belongs to the Dataset that was opened, so a fresh arrival remounts: a run
      // scope must never survive into a Dataset it says nothing about.
      key={`${keyOf(selected)}:${focusRun ?? ''}`}
      dataset={selected}
      focusRun={focusRun}
      onClose={() => {
        setSelected(null);
        setFocusRun(null);
      }}
    />
  ) : (
    // Opened from the LIST, so no Run scopes it: a scope carried in from a previous arrival would
    // silently filter a Dataset the operator picked for itself.
    <DatasetList
      onOpen={(d) => {
        setSelected(d);
        setFocusRun(null);
      }}
    />
  );
}

// --- the listing ------------------------------------------------------------------------------

/**
 * THE LISTING IS A DATAGRID, and the two rules issue #14 wrote for the RESULTS grid apply here for
 * the same reason — the app-level catalog poll re-renders this tree every ~2 s while any Run is
 * live, and a grid that resets on each tick is worse than the hand-rolled table it replaced:
 *
 *   1. THE COLUMN DEFINITIONS ARE ONE FROZEN MODULE CONSTANT, built once for the process. The
 *      results grid has to memoise on a schema signature because its columns ARE the query's
 *      columns; the listing's columns are fixed by this file, so they can be stronger than a memo —
 *      the poll cannot rebuild them because nothing rebuilds them. A new array, even with identical
 *      contents, is a changed prop and re-applies widths.
 *   2. THE DEFAULTS ARE ONE FROZEN SHARED OBJECT, never a per-render literal, for exactly the
 *      reason {@link GRID_DEFAULT_COL_DEF} records.
 *
 * And one more the results grid does not need: `getRowId`. The ROW DATA legitimately changes on
 * every poll — rows and bytes move while a Run writes — so without a stable id AG Grid replaces the
 * whole row set and takes the operator's scroll and selection with it. Keyed on `kind:name`
 * ({@link listingRowId}) the same Dataset stays the same row and only its cells update.
 *
 * WHAT A ROW IS: one **Dataset**, not one dispatch. That grain is `datasets/grouped.ts`'s decision
 * and it survives the port — three Runs appending to `lame` are one row, and `version`, `dispatch`
 * and `run` state the plurality inside it. What the port DOES give up is clicking one dispatch open:
 * the row opens the Dataset, and the console's own scope selector narrows it to a single **Run**,
 * which is the same rows by a better route (the run id is the thing that scopes `WHERE run_id = …`,
 * and a dispatch is a partition of it).
 */

/**
 * The per-column defaults for the LISTING grid — one frozen object shared for the grid's whole life,
 * for the reason {@link GRID_DEFAULT_COL_DEF} spells out.
 *
 * NO `flex` HERE. The listing's columns carry their own widths (a `state` badge and a tag strip do
 * not want the same room), and `flex` in the shared default is precisely what re-shared every
 * column's width on each poll in #14. The two elastic columns declare `flex` themselves, inside the
 * frozen defs, so it is applied once rather than re-applied per render.
 *
 * NO TYPE INFERENCE, for the same reason as the results grid: AG Grid types a column from its first
 * value, and a boolean column draws a CHECKBOX — which would render `temporary` as a tick box and an
 * untagged Dataset as an unticked one indistinguishable from a durable one.
 */
export const LISTING_DEFAULT_COL_DEF: ColDef<DatasetListingRow> = Object.freeze({
  sortable: true,
  filter: true,
  resizable: true,
  minWidth: 72,
  cellDataType: false,
});

/** What a listing cell can DO — handed to the grid as `context` so the column definitions stay
 *  values, with no closure over this render's props. Cells read it through {@link listingActions}. */
export interface DatasetListingActions {
  onOpen(g: DatasetGroup): void;
  onDelete(g: DatasetGroup): void;
  onAddTag(g: DatasetGroup): void;
  onRemoveTag(g: DatasetGroup, tag: string): void;
  onRename(g: DatasetGroup): void;
  /** The temp whose delete is in flight: its button is disabled so a second click cannot re-fire. */
  deleting: string | null;
}

function listingActions(p: { context?: unknown }): DatasetListingActions | undefined {
  return p.context as DatasetListingActions | undefined;
}

/**
 * Whether a click inside a row should OPEN the Dataset's console.
 *
 * THE ROW-ACTION BUTTONS ARE NOT OPENERS, and `stopPropagation` cannot say so here. AG Grid listens
 * for the click on the ROW element, which is an ancestor of the button and a descendant of React's
 * delegated root — so the grid's handler runs BEFORE any React handler could stop the event. The
 * decision therefore has to be made inside the grid's own handler, by looking at what was clicked.
 */
export function opensConsole(target: unknown): boolean {
  const el = target as { closest?(selector: string): unknown } | null;
  if (!el || typeof el.closest !== 'function') return true;
  return el.closest('[data-row-action]') == null;
}

/** The Dataset's storage name — struck through when abandoned, because §11's whole point is that a
 *  partial Dataset must never read as a finished one and a badge alone is what a scanning eye
 *  skips. */
function DatasetNameCell(p: ICellRendererParams<DatasetListingRow>): JSX.Element | null {
  const row = p.data;
  if (!row) return null;
  return (
    <span
      data-testid={`dataset-row-${row.dataset}`}
      data-dispatches={row.dispatches}
      className={`font-mono ${row.state === 'abandoned' ? 'text-muted-foreground line-through' : ''}`}
      title={row.dataset}
    >
      {row.dataset}
    </span>
  );
}

/**
 * The run-grain name (ADR 0029), drawn in the VIEWER'S zone and sorted on the server's UTC string.
 *
 * The pencil renames it; an empty answer resets to the derived default. Present only with a Run to
 * key the record on — a standalone list has none, so the affordance is absent rather than failing.
 */
function DatasetDerivedNameCell(p: ICellRendererParams<DatasetListingRow>): JSX.Element | null {
  const row = p.data;
  const actions = listingActions(p);
  if (!row) return null;
  if (!row.name) return <span className="text-muted-foreground">—</span>;
  return (
    <span className="group/name flex items-center gap-1">
      <span
        data-testid={`dataset-name-${row.dataset}`}
        className={`truncate font-mono text-[11px] ${row.renamed ? 'text-foreground' : 'text-muted-foreground'}`}
        title={
          row.renamed
            ? 'the operator RENAME (ADR 0029 §4) — overrides the derived name'
            : row.localized
              ? `the derived, run-grain name (ADR 0029 §2), shown in YOUR time zone.\nkontra dataset ls prints it UTC:\n${row.name}`
              : 'the derived, run-grain name (ADR 0029 §2) — the same string kontra dataset ls prints'
        }
      >
        {row.nameLocal}
      </span>
      {row.runId && (
        <button
          type="button"
          data-row-action
          data-testid={`dataset-rename-${row.dataset}`}
          className="rounded p-0.5 text-muted-foreground opacity-0 hover:bg-accent hover:text-foreground group-hover/name:opacity-100"
          title="Rename this Dataset (empty resets to the derived name)"
          onClick={() => actions?.onRename(row.group)}
        >
          <Pencil size={10} />
        </button>
      )}
    </span>
  );
}

/** The §11 lifecycle badge — three states and the absence of one, each its own colour. */
function DatasetStateCell(p: ICellRendererParams<DatasetListingRow>): JSX.Element | null {
  const row = p.data;
  if (!row) return null;
  const badge = datasetBadge(row.state);
  return (
    <Badge variant="outline" className={`text-[9.5px] ${badge.className}`} title={badge.title}>
      {badge.label}
    </Badge>
  );
}

/** TEMPORARY vs durable — a different axis from the lifecycle beside it, so `tmp_a7f3` never reads
 *  as a durable `lame` an operator meant to keep. */
function DatasetTempCell(p: ICellRendererParams<DatasetListingRow>): JSX.Element | null {
  const row = p.data;
  if (!row) return null;
  if (!row.temporary) return <span className="text-[10px] text-muted-foreground">durable</span>;
  return (
    <Badge
      variant="outline"
      data-testid={`dataset-temp-${row.dataset}`}
      className="border-violet-500/40 bg-violet-500/15 text-[9.5px] text-violet-700 dark:text-violet-300"
      title={`A Run's temporary Dataset — framework-named and short-lived, owned by run ${row.owner || 'unknown'}. Delete it when its triage is done; it is never swept for you.`}
    >
      temporary
    </Badge>
  );
}

/** MEASURED between two catalog samples — empty until there are two, and empty forever for a Dataset
 *  nothing is writing to. A flat line would say "actively writing zero rows", which is a different
 *  and alarming claim. */
function DatasetRateCell(p: ICellRendererParams<DatasetListingRow>): JSX.Element | null {
  const row = p.data;
  if (!row) return null;
  return (
    <Spark
      series={row.rate}
      color={row.state === 'open' ? '#fcd34d' : row.state === 'sealed' ? '#34d399' : 'currentColor'}
      width={72}
      height={14}
      title={`rows per second written to ${row.dataset}, across every dispatch`}
    />
  );
}

/**
 * WHICH **RUN** MADE THIS. One Run is NAMED, several are COUNTED, and the difference is the whole
 * design (ADR 0029 §2): a durable Dataset accumulates across Runs — `lame` holds five on this box —
 * so naming the newest of five under a heading that reads "run" is exactly the lie the plural exists
 * to prevent. The set is one hover away, and so is the Run a tag would actually be keyed on, which
 * is a THIRD fact and never silently substituted for either of the other two.
 */
function DatasetRunCell(p: ICellRendererParams<DatasetListingRow>): JSX.Element | null {
  const row = p.data;
  if (!row) return null;
  const plural = { contributingRuns: row.runs, contributingRunsPartial: row.runsPartial };
  const key = row.runId ? `\n\nTags and the rename are keyed on the newest of them: ${row.runId}` : '';
  if (row.soleRun) {
    return (
      <span
        data-testid={`dataset-runs-${row.dataset}`}
        className="truncate font-mono text-[10.5px] text-muted-foreground"
        title={`the one Run that wrote this Dataset — and the key its tags and rename are stored against:\n${row.soleRun}`}
      >
        {row.soleRun}
      </span>
    );
  }
  return (
    <span
      data-testid={`dataset-runs-${row.dataset}`}
      className="truncate text-[10.5px] text-muted-foreground"
      title={runTitle(plural) + key}
    >
      {runText(plural) || '—'}
    </span>
  );
}

/**
 * THE TAG SET (ADR 0029 §1) — a chip each with a remove, and a `+` to add.
 *
 * Present only where there is a **Run** to key the record on, because that is the record's key: a
 * standalone list has none, so the affordance is absent rather than failing. A tag is added and
 * removed, never assigned, so two operators tagging one Dataset converge instead of clobbering.
 *
 * WHAT IT MEANS DIFFERS FOR A TEMP, and the tooltip says which — see {@link tagMeaning}. A tag on a
 * durable Dataset is what keeps it past the TTL; a tag on a TEMPORARY one keeps the same Run's
 * durable output, changes nothing about the temp's own lifetime, and does not stop the delete
 * button beside it.
 */
function DatasetTagsCell(p: ICellRendererParams<DatasetListingRow>): JSX.Element | null {
  const row = p.data;
  const actions = listingActions(p);
  if (!row) return null;
  return (
    <TagStrip
      dataset={row.dataset}
      tags={row.tags}
      runId={row.runId}
      temporary={row.temporary}
      onAdd={() => actions?.onAddTag(row.group)}
      onRemove={(tag) => actions?.onRemoveTag(row.group, tag)}
    />
  );
}

/**
 * WHAT RETENTION WOULD DO WITH THIS DATASET — the sweep's reading, on the listing row.
 *
 * BESIDE THE TAGS, DELIBERATELY. A tag is what changes this cell, and until now the consequence of
 * NOT tagging lived only on the Dataset's own page: an operator scanning a hundred rows for the ones
 * about to age out had to open a hundred pages. The sweep is armed and hourly now, so the reading has
 * to be where the decision is made.
 *
 * AMBER ONLY WHEN A SWEEP WOULD COLLECT IT TODAY. Every other reading is a keep, and a listing where
 * most rows glow is a listing nobody reads.
 */
function DatasetExpiryCell(p: ICellRendererParams<DatasetListingRow>): JSX.Element | null {
  const row = p.data;
  if (!row) return null;
  return (
    <span
      data-testid="dataset-row-expiry"
      data-dataset={row.dataset}
      data-disposition={row.expiry.disposition}
      className={`rounded border px-1.5 py-0.5 font-mono text-[10px] ${
        row.expiry.due
          ? 'border-amber-500/40 bg-amber-500/10 text-amber-700 dark:text-amber-400'
          : 'border-transparent text-muted-foreground'
      }`}
      title={row.expiry.title}
    >
      {row.expiry.label}
    </span>
  );
}

/**
 * THE TAG SET, DRAWN AND EDITED — the one implementation, used by the listing cell above and by the
 * Dataset's own page below.
 *
 * IT IS SHARED BECAUSE THE TWO PLACES MUST NOT DRIFT. Tagging is what keeps output past the TTL, and
 * a `+` that appears on the listing but not on the page (or that says something different about what
 * it keys on) is an operator learning two rules for one act. What differs between the callers is
 * only what a mutation does AFTER the route answers — the listing reloads the whole catalog, the
 * page updates the record it is holding — which is why those are callbacks and not baked in here.
 *
 * NO RUN, NO AFFORDANCE. The record is keyed by the **Run** (ADR 0029 §4), so a Dataset no single
 * Run resolved for has nothing to address; it says so rather than offering a button that can only
 * fail.
 */
export function TagStrip({
  dataset,
  tags,
  runId,
  temporary,
  onAdd,
  onRemove,
}: {
  /** The Dataset's storage name — the test hook each element is keyed on. */
  dataset: string;
  tags: readonly string[];
  /** The record's key, or empty when nothing resolved one. */
  runId: string;
  temporary: boolean;
  onAdd(): void;
  onRemove(tag: string): void;
}): JSX.Element {
  if (!runId) {
    return (
      <span className="text-[10px] text-muted-foreground" title="No Run resolved for this Dataset, and the tag record is keyed by the Run (ADR 0029 §4) — there is nothing to address.">
        —
      </span>
    );
  }
  return (
    <span data-testid={`dataset-tags-${dataset}`} className="flex items-center gap-1 overflow-hidden">
      {/* THE CHIPS GIVE WAY, THE `+` DOES NOT. Two tags at this column's width pushed the add button
          out of an `overflow-hidden` cell — the affordance disappearing exactly when the Dataset is
          interesting enough to have been tagged twice. The set truncates instead; the whole of it is
          on the cell's own hover, and the column is resizable like every other. */}
      <span className="flex min-w-0 items-center gap-1 overflow-hidden" title={tags.join(', ')}>
        {tags.map((tag) => (
          <Badge
            key={tag}
            variant="outline"
            data-testid={`dataset-tag-${dataset}-${tag}`}
            className="gap-0.5 border-sky-500/40 bg-sky-500/10 py-0 pl-1.5 pr-1 text-[9.5px] text-sky-700 dark:text-sky-300"
            title={`tagged "${tag}" — kept output (ADR 0029 §3)`}
          >
            <Tag size={9} />
            {tag}
            <button
              type="button"
              data-row-action
              data-testid={`dataset-untag-${dataset}-${tag}`}
              className="rounded hover:text-destructive"
              title={`Remove the tag "${tag}"`}
              onClick={() => onRemove(tag)}
            >
              <X size={9} />
            </button>
          </Badge>
        ))}
      </span>
      <button
        type="button"
        data-row-action
        data-testid={`dataset-addtag-${dataset}`}
        className="flex shrink-0 items-center gap-0.5 rounded border border-dashed border-border px-1 py-0 text-[9.5px] text-muted-foreground hover:border-foreground/40 hover:text-foreground"
        title={tagMeaning({ temporary })}
        onClick={onAdd}
      >
        <Plus size={9} />
        tag
      </button>
    </span>
  );
}

/**
 * THE LIVE ROW TAIL (live-datasets slice 05). While a Dataset is `open` its producer is still
 * appending and the catalog total four columns over does not move until the caller publishes, so
 * this streams the durable count of the Run's blobs as they land, from its own SSE endpoint.
 *
 * Its column's VALUE is the Run id, never a count — see {@link DatasetListingRow.live}: a value that
 * moved every two seconds would have the grid refresh this cell every two seconds, tearing the
 * stream down and re-opening it on each catalog poll.
 */
function DatasetLiveCell(p: ICellRendererParams<DatasetListingRow>): JSX.Element | null {
  const row = p.data;
  if (!row?.live) return null;
  return <LiveRowTail runId={row.live} enabled />;
}

/** Deletion lives ONLY on a temporary row: the route refuses a durable Dataset (409), so offering
 *  the button there would be an action that can only fail. */
function DatasetActionsCell(p: ICellRendererParams<DatasetListingRow>): JSX.Element | null {
  const row = p.data;
  const actions = listingActions(p);
  if (!row?.temporary) return null;
  return (
    <button
      type="button"
      data-row-action
      data-testid={`dataset-delete-${row.dataset}`}
      disabled={actions?.deleting === row.dataset}
      className="rounded p-1 text-muted-foreground hover:bg-destructive/10 hover:text-destructive disabled:opacity-50"
      title={`Delete this temporary Dataset (owned by run ${row.owner || 'unknown'})`}
      onClick={() => actions?.onDelete(row.group)}
    >
      <Trash2 size={13} />
    </button>
  );
}

/**
 * The listing's columns — every fact the listing knows, each with a VALUE to sort and filter on.
 *
 * ONE FROZEN CONSTANT, built once for the process: see the section header. The array is frozen
 * (shallow) so a future edit cannot start pushing per-render columns into it, which is how the
 * stable reference would be lost.
 *
 * TWO HONESTY RULES ARE ENCODED IN THE VALUE GETTERS, not in the renderers, which is what makes
 * them survive a sort:
 *   - `name` sorts on the CANONICAL, UTC string the server rendered while the cell draws the
 *     viewer's local one (ADR 0029 §2). Sorting on the drawn string would order one operator's list
 *     differently from another's, for the same Datasets.
 *   - `run` holds the SINGULAR Run only where a singular answer exists. A partition several Runs
 *     wrote sorts as empty and reads "from 5 runs" — never the first of five.
 */
export const DATASET_LISTING_COLUMNS = Object.freeze([
  {
    colId: 'dataset',
    headerName: 'dataset',
    field: 'dataset',
    pinned: 'left',
    width: 176,
    cellRenderer: DatasetNameCell,
    headerTooltip:
      'The Dataset’s storage name — its identity, and the table `SELECT * FROM <it>` names.',
  },
  {
    colId: 'name',
    headerName: 'name',
    // The canonical (UTC) string is the sort and filter value; the cell draws it local.
    valueGetter: (p: ValueGetterParams<DatasetListingRow>) => p.data?.name ?? '',
    cellRenderer: DatasetDerivedNameCell,
    flex: 2,
    minWidth: 330,
    headerTooltip:
      'The run-grain name (ADR 0029): the operator’s rename when there is one, else the derived ' +
      'default. Stored UTC, drawn in your time zone — and sorted on the stored string, so every ' +
      'viewer gets the same order.',
  },
  {
    colId: 'tags',
    headerName: 'tags',
    // Joined so the column's text filter finds a tag — the set is what the cell draws.
    valueGetter: (p: ValueGetterParams<DatasetListingRow>) => (p.data?.tags ?? []).join(' '),
    cellRenderer: DatasetTagsCell,
    flex: 1.3,
    minWidth: 186,
    headerTooltip:
      'Tags an operator or an author put on this Run’s output (ADR 0029 §1). Untagged output ' +
      'expires after the retention TTL; tagged output is kept. Filter this column to find one.',
  },
  {
    colId: 'expires',
    headerName: 'expires',
    // The LABEL is the value, so the text filter finds `would be collected` — the rows about to age
    // out — and the sort groups the keeps together. Not a timestamp: the sweep is a schedule, so
    // there is no deletion instant to sort on, only a reading.
    valueGetter: (p: ValueGetterParams<DatasetListingRow>) => p.data?.expiry.label ?? '',
    cellRenderer: DatasetExpiryCell,
    width: 168,
    headerTooltip:
      'What the retention sweep would do with this Dataset (ADR 0029 §3): untagged output is ' +
      'collected once its LAST WRITE is older than the TTL plus a grace window; a tagged, open or ' +
      'temporary Dataset is kept. Filter for `would be collected` to find what is about to age out. ' +
      '“Would”, not “will”: collection happens when a sweep runs.',
  },
  {
    colId: 'state',
    headerName: 'state',
    field: 'state',
    cellRenderer: DatasetStateCell,
    width: 104,
    headerTooltip:
      'The §11 lifecycle: open while a producer may still be appending, sealed when the caller ' +
      'declared it complete, abandoned when it gave up. `none` means no writer ever recorded one.',
  },
  {
    colId: 'temporary',
    headerName: 'temporary',
    valueGetter: (p: ValueGetterParams<DatasetListingRow>) =>
      p.data?.temporary ? 'temporary' : 'durable',
    cellRenderer: DatasetTempCell,
    width: 100,
    headerTooltip:
      'A temporary Dataset is a Run’s staging table (ADR 0028): framework-named, owned by that Run, ' +
      'and removed by asking — never by the retention sweep.',
  },
  {
    colId: 'kind',
    headerName: 'kind',
    field: 'kind',
    width: 80,
    headerTooltip:
      '`output` was written by a Run; `standalone` was loaded by an operator with kontra dataset create.',
  },
  {
    colId: 'rows',
    headerName: `rows · ${scopeWords('dataset').label}`,
    field: 'rows',
    type: 'rightAligned',
    width: 118,
    filter: 'agNumberColumnFilter',
    valueFormatter: (p: { value: unknown }) => Number(p.value ?? 0).toLocaleString(),
    headerTooltip: scopeWords('dataset').title,
  },
  {
    colId: 'bytes',
    headerName: 'size',
    field: 'bytes',
    type: 'rightAligned',
    width: 88,
    filter: 'agNumberColumnFilter',
    // The NUMBER is the value, so 512 KB sorts below 4.5 MB instead of above it as text.
    valueFormatter: (p: { value: unknown }) => fmtBytes(Number(p.value ?? 0)) || '—',
    headerTooltip: 'What this Dataset’s data files occupy, exactly as the catalog reports it.',
  },
  {
    colId: 'rate',
    headerName: 'write rate',
    field: 'rateNow',
    cellRenderer: DatasetRateCell,
    width: 92,
    filter: 'agNumberColumnFilter',
    headerTooltip:
      'Rows per second, measured between two catalog samples. Sorts on the newest sample, so the ' +
      'Dataset being written to fastest comes to the top.',
  },
  {
    colId: 'live',
    headerName: 'live rows',
    field: 'live',
    cellRenderer: DatasetLiveCell,
    width: 110,
    headerTooltip:
      'Rows landing right now, streamed from the Run’s own endpoint while the Dataset is open. The ' +
      'catalog total beside it does not move until the caller publishes.',
  },
  {
    colId: 'run',
    headerName: 'run',
    // The SINGULAR run, and only where a singular answer exists (ADR 0029 §2, ADR 0017). NOT
    // `runId`, which is the record's KEY and is filled even for a Dataset five Runs wrote.
    valueGetter: (p: ValueGetterParams<DatasetListingRow>) => p.data?.soleRun ?? '',
    cellRenderer: DatasetRunCell,
    width: 152,
    headerTooltip:
      'The one Run that wrote this Dataset. A Dataset several Runs appended to has no singular ' +
      'answer, so the cell counts them and lists them on hover — never the newest of five dressed ' +
      'up as the one. The Run a tag would be keyed on is on that hover too.',
  },
  {
    colId: 'version',
    headerName: 'version',
    field: 'version',
    width: 88,
    valueFormatter: (p: { value: unknown }) => String(p.value ?? '') || '—',
    headerTooltip:
      'Every Actor version that wrote this name. More than one is a real state a total hides.',
  },
  {
    colId: 'dt',
    headerName: 'dispatch',
    valueGetter: (p: ValueGetterParams<DatasetListingRow>) => p.data?.dt ?? '',
    valueFormatter: (p: { data?: DatasetListingRow }) => (p.data ? dispatchCell(p.data) : ''),
    width: 160,
    headerTooltip:
      'The newest `version=…/dt=…` partition, and how many more are under this name. It is a ' +
      'directory on disk and UTC by construction, so — unlike the name — it is not re-zoned.',
  },
  {
    colId: 'columns',
    headerName: 'columns',
    valueGetter: (p: ValueGetterParams<DatasetListingRow>) => (p.data?.columns ?? []).join(', '),
    width: 140,
    valueFormatter: (p: { value: unknown }) => String(p.value ?? '') || '—',
    headerTooltip: 'The table’s columns, from the catalog — what is in this thing, without opening it.',
  },
  {
    colId: 'updatedAt',
    headerName: 'written',
    field: 'updatedAt',
    width: 150,
    filter: 'agNumberColumnFilter',
    // Sorted on the instant, drawn in the viewer's zone — the same split the name column makes.
    valueFormatter: (p: { value: unknown }) => when(Number(p.value ?? 0)) || '—',
    headerTooltip: 'When this Dataset’s newest file was committed, in your time zone.',
  },
  {
    colId: 'actions',
    headerName: '',
    pinned: 'right',
    width: 56,
    sortable: false,
    filter: false,
    resizable: false,
    cellRenderer: DatasetActionsCell,
    headerTooltip: 'Delete a temporary Dataset. A durable one is refused by the route, so it has none.',
  },
] as ColDef<DatasetListingRow>[]) as ColDef<DatasetListingRow>[];

function DatasetList({ onOpen }: { onOpen(d: DatasetInfo): void }): JSX.Element {
  // THE LISTING COMES FROM THE APP-LEVEL POLL, not from a fetch of its own. That is what makes the
  // write-rate column possible at all — a rate needs two samples taken by the same clock — and it
  // is why this page's row counts never disagree with the ones the Workflows surface is showing
  // for the same run.
  const datasets = useAppStore((s) => s.datasets);
  const datasetsAt = useAppStore((s) => s.datasetsAt);
  const listError = useAppStore((s) => s.datasetsError);
  const series = useAppStore((s) => s.datasetSeries);
  const reload = useAppStore((s) => s.loadDatasets);
  const theme = useAppStore((s) => s.theme);
  const [schema, setSchema] = useState<SchemaEntry[] | null>(null);
  const [loaded, setLoaded] = useState(false);
  /** The temp currently being deleted — its row shows progress and cannot double-fire the drop. */
  const [deleting, setDeleting] = useState<string | null>(null);
  const [deleteError, setDeleteError] = useState<string | null>(null);
  /** A tag or rename that the route refused (a 400 empty tag) or that failed on the wire. The
   *  server's own sentence, surfaced rather than swallowed — the same treatment `deleteError` gets. */
  const [recordError, setRecordError] = useState<string | null>(null);

  useEffect(() => {
    void reload().finally(() => setLoaded(true));
  }, [reload]);

  // Delete a temporary Dataset, having NAMED what goes (rows, size, owner, its tags — and, for an
  // open temp, the run it may still belong to) in a confirmation the operator reads first. On
  // success the app-level listing is reloaded rather than the page: the dropped temp is gone and the
  // operator stays put, one row lighter. A refusal (a durable Dataset reached this by any path)
  // surfaces the server's own sentence rather than vanishing.
  const handleDelete = useCallback(
    async (group: DatasetGroup) => {
      const ok = window.confirm(
        deletionConfirm({
          name: group.name,
          rows: group.total.total.rows,
          bytes: group.bytes,
          owner: group.owner,
          state: datasetState(wholeDataset(group)),
          tags: group.tags,
        })
      );
      if (!ok) return;
      setDeleting(group.name);
      setDeleteError(null);
      try {
        await deleteDataset(group.name);
        await reload();
      } catch (e) {
        setDeleteError(e instanceof Error ? e.message : String(e));
      } finally {
        setDeleting(null);
      }
    },
    [reload]
  );

  // Tag / untag / rename a Dataset — over the record keyed by the group's newest resolved Run (ADR
  // 0029 §4). Each mutation reloads the app-level listing so the row reflects the new record, the
  // same way a delete does. A refusal (an empty tag is a 400) surfaces the server's sentence. Guarded
  // on `runId`: the affordance is absent without one, so these never fire on an unaddressable row.
  const withRecordAction = useCallback(
    async (action: () => Promise<unknown>) => {
      setRecordError(null);
      try {
        await action();
        await reload();
      } catch (e) {
        setRecordError(e instanceof Error ? e.message : String(e));
      }
    },
    [reload]
  );
  const handleAddTag = useCallback(
    (group: DatasetGroup) => {
      if (!group.runId) return;
      // A TEMP IS TAGGED THROUGH THE SAME KEY AS EVERYTHING ELSE THE RUN WROTE, and the prompt says
      // so: the record is keyed by the Run (ADR 0029 §4), a temp has exactly one (its owner), and
      // that Run is usually also the one that promoted rows into a durable Dataset. Tagging the
      // staging table therefore keeps the durable output too — which is what an operator means —
      // but it is not what "tag this table" looks like, so it is stated before the tag is typed.
      const tag = window.prompt(`${tagPrompt(group)}\n\nAdd a tag`)?.trim();
      if (!tag) return;
      void withRecordAction(() => addDatasetTag(group.runId!, tag));
    },
    [withRecordAction]
  );
  const handleRemoveTag = useCallback(
    (group: DatasetGroup, tag: string) => {
      if (!group.runId) return;
      void withRecordAction(() => removeDatasetTag(group.runId!, tag));
    },
    [withRecordAction]
  );
  // Rename, or reset to the derived default when the field is cleared. The prompt is seeded with the
  // current effective name so an edit starts from what is on screen, and an empty answer means "drop
  // the rename" rather than "store a blank" — which the route would refuse anyway.
  const handleRename = useCallback(
    (group: DatasetGroup) => {
      if (!group.runId) return;
      const current = group.renamedTo ?? groupDatasetName(group) ?? '';
      const next = window.prompt(`Rename "${group.name}" (empty resets to the derived name)`, current);
      if (next === null) return; // cancelled
      const trimmed = next.trim();
      void withRecordAction(() =>
        trimmed === '' ? resetDatasetName(group.runId!) : renameDataset(group.runId!, trimmed)
      );
    },
    [withRecordAction]
  );

  // The column lists, in one call for every dataset. Cheap (catalog metadata only) and it turns
  // "what is in this thing" into something answerable from the listing rather than from opening it.
  useEffect(() => {
    fetchSchema()
      .then(setSchema)
      .catch(() => setSchema([]));
  }, []);

  /**
   * ONE ROW PER DATASET. The response is one row per `version=…/dt=…` partition, so three Runs of
   * one workflow put three rows called `lame` on this page — each reading `623 rows`, none of them
   * saying they were one Dataset. See `datasets/grouped.ts`; the dispatch grain is still stated, in
   * the `dispatch`, `version` and `run` columns.
   */
  const groups = useMemo(() => groupDatasets(datasets, datasetsAt), [datasets, datasetsAt]);
  /** NULL while the schema call is still out — "reading…" and "no columns" are different answers. */
  const columnsByName = useMemo(
    () =>
      schema === null
        ? null
        : Object.fromEntries(schema.map((s) => [s.name, s.columns.map((c) => c.name)])),
    [schema]
  );
  const rows = useMemo(
    () => listingRows(groups, { series, columns: columnsByName }),
    [groups, series, columnsByName]
  );

  // The row actions, as the grid's `context`. Stable while the callbacks are — a changed context is
  // not a changed column definition, so the widths and the sort survive it either way.
  const actions = useMemo<DatasetListingActions>(
    () => ({
      onOpen: (g) => onOpen(wholeDataset(g)),
      onDelete: handleDelete,
      onAddTag: handleAddTag,
      onRemoveTag: handleRemoveTag,
      onRename: handleRename,
      deleting,
    }),
    [deleting, handleAddTag, handleDelete, handleRemoveTag, handleRename, onOpen]
  );

  return (
    <div className="dataset-page">
      <div className="flex flex-wrap items-baseline gap-3">
        <h1 className="m-0 text-lg font-semibold">Datasets</h1>
        <span className="text-[11.5px] text-muted-foreground">
          Every Dataset ever written — sort, filter and resize any column. Open one for a SQL console
          over its raw rows.
        </span>
        <Button
          variant="ghost"
          size="icon"
          className="ml-auto"
          onClick={() => void reload()}
          title="Refresh"
        >
          <RefreshCw />
        </Button>
      </div>

      {/* The four lifecycle states, once, at the top — rather than a tooltip per row. §11's whole
          point is that a partial Dataset must never read as a finished one, and a colour an
          operator has to hover to decode is a colour they will stop decoding. */}
      <div className="flex flex-wrap gap-1.5">
        {[...DATASET_STATES, 'none' as const].map((s) => {
          const badge = datasetBadge(s);
          return (
            <Badge key={s} variant="outline" className={`text-[9.5px] ${badge.className}`} title={badge.title}>
              {badge.label}
            </Badge>
          );
        })}
      </div>

      {listError && <p className="run-status run-failed">{listError}</p>}
      {/* A delete that the ROUTE refused — a durable Dataset reached by any path is a 409 — or that
          failed on the wire. The server's own sentence, surfaced rather than swallowed. */}
      {deleteError && <p className="run-status run-failed">{deleteError}</p>}
      {/* A tag or rename the route refused (an empty tag is a 400) or that failed on the wire. */}
      {recordError && <p className="run-status run-failed">{recordError}</p>}
      {loaded && datasets.length === 0 && !listError && (
        <p className="text-muted-foreground">
          No datasets yet. Load a list with <code>kontra dataset create</code>, or run a workflow —
          each actor's output is mirrored here as <code>name · version · dispatch</code>.
        </p>
      )}

      {rows.length > 0 && (
        <div className="min-h-[320px] flex-1" data-testid="dataset-listing-grid">
          <AgGridReact<DatasetListingRow>
            theme={gridTheme(theme)}
            columnDefs={DATASET_LISTING_COLUMNS}
            defaultColDef={LISTING_DEFAULT_COL_DEF}
            rowData={rows}
            // The SAME id every poll, so a moving row count updates cells in place instead of
            // replacing the row set and taking the scroll with it.
            getRowId={(p) => listingRowId(p.data)}
            context={actions}
            rowHeight={38}
            tooltipShowDelay={400}
            enableCellTextSelection
            suppressFieldDotNotation
            // A row opens its console — unless the click was on one of the row's own actions, which
            // AG Grid's handler sees before React's could stop it. See `opensConsole`.
            onRowClicked={(e) => {
              if (e.data && opensConsole(e.event?.target)) actions.onOpen(e.data.group);
            }}
          />
        </div>
      )}
    </div>
  );
}

/** What tagging THIS Dataset will actually key on, said before the tag is typed. See
 *  {@link tagMeaning} for why a temp's answer differs from a durable Dataset's. */
export function tagPrompt(g: Pick<DatasetGroup, 'name' | 'runId' | 'temporary'>): string {
  const run = g.runId ?? 'unknown';
  return g.temporary
    ? `Tag the temporary Dataset "${g.name}".\n\nThe tag is stored against its owning run ${run}, ` +
        'so it also marks everything else that Run wrote — including the durable Dataset it ' +
        'promoted into, which is what the tag keeps past the retention TTL. The temp itself is ' +
        'never swept on a clock, and the delete button still removes it.'
    : `Tag "${g.name}".\n\nThe tag is stored against run ${run} (ADR 0029 §4) and keeps that Run's ` +
        'output past the retention TTL.';
}

// --- one cell, opened ---------------------------------------------------------------------------

/**
 * The full value of one cell, beside the grid rather than inside it.
 *
 * A GRID ROW CANNOT SHOW A DOCUMENT. A crawl's markdown body is thousands of characters in a
 * 120-pixel column, so the one column an operator opened the Dataset to read was the only one they
 * could not — and the workaround was to drag the row taller, which is a per-row fix for a
 * per-Dataset problem.
 *
 * IT SHOWS JSON FOR A NESTED VALUE AND TEXT VERBATIM, which is the opposite of the grid's DuckDB
 * notation on purpose. What is shown here is what gets copied, and a copied value has to
 * round-trip: DuckDB's form is unambiguous to read and lossy to parse. `cells.ts` carries the whole
 * reasoning and is where both are pinned.
 */
function CellInspector({
  column,
  value,
  onClose,
}: {
  column: string;
  value: unknown;
  onClose(): void;
}): JSX.Element {
  const detail = inspect(value);
  const [copied, setCopied] = useState(false);

  return (
    <aside
      className="flex w-80 shrink-0 flex-col overflow-hidden rounded-md border border-border"
      data-testid="cell-inspector"
      data-kind={detail.kind}
    >
      <div className="flex shrink-0 items-center gap-1.5 border-b border-border bg-muted/60 px-2 py-1">
        <span className="min-w-0 flex-1 truncate font-mono text-[11px] font-medium" title={column}>
          {column}
        </span>
        {/* WHICH FORM IS ON SCREEN. The grid one row away says `{'a': 1}` and this says `{"a": 1}`;
            unlabelled, that reads as one of them being wrong. */}
        <span
          className="shrink-0 rounded bg-background px-1 font-mono text-[9px] uppercase tracking-wider text-muted-foreground"
          title={
            detail.kind === 'json'
              ? 'JSON, not the grid’s DuckDB notation — this is the form that parses back'
              : 'the value exactly as it is stored, newlines and all'
          }
        >
          {detail.kind}
        </span>
        <span className="shrink-0 font-mono text-[9.5px] tabular-nums text-muted-foreground">
          {detail.size}
        </span>
        <Button
          variant="ghost"
          size="icon"
          className="size-5 shrink-0"
          title="Copy"
          onClick={() => {
            void navigator.clipboard
              .writeText(detail.body)
              .then(() => {
                setCopied(true);
                setTimeout(() => setCopied(false), 2000);
              })
              .catch(() => {
                /* a plain-HTTP origin has no secure clipboard — the value is selectable below */
              });
          }}
        >
          {copied ? <Check /> : <Copy />}
        </Button>
        <Button variant="ghost" size="icon" className="size-5 shrink-0" title="Close" onClick={onClose}>
          <X />
        </Button>
      </div>
      {/* `whitespace-pre-wrap`, so a markdown body keeps its blank lines and its indentation. A
          document reflowed to fit a panel is a different document. */}
      <pre className="m-0 min-h-0 flex-1 overflow-auto whitespace-pre-wrap break-words p-2 font-mono text-[11px] leading-relaxed">
        {detail.body}
      </pre>
      {/* The DuckDB spelling too, for the value an operator is about to paste into a WHERE clause.
          Only when it differs from what is above — on a plain string the two are the same text. */}
      {detail.kind === 'json' && (
        <div className="shrink-0 border-t border-border bg-muted/30 p-2">
          <div className="mb-0.5 font-mono text-[9px] uppercase tracking-wider text-muted-foreground">
            as duckdb prints it
          </div>
          <code className="block break-words font-mono text-[10.5px] text-muted-foreground">
            {duckdbText(value)}
          </code>
        </div>
      )}
    </aside>
  );
}

// --- one dataset, with its console --------------------------------------------------------------

function DatasetConsole({
  dataset,
  focusRun = null,
  onClose,
}: {
  dataset: DatasetInfo;
  /** The Run to open scoped to, when the operator arrived from one. */
  focusRun?: string | null;
  onClose(): void;
}): JSX.Element {
  const theme = useAppStore((s) => s.theme);
  /** Go to the Run that wrote this Dataset. A store action, so the address bar follows and the link
   *  is pasteable — provenance is a link, not a string to copy out and search for. */
  const openRun = useAppStore((s) => s.openRun);
  /**
   * The **Run** every count on this screen is about, or `null` for every Run that wrote the name.
   *
   * IT SCOPES THE WHOLE CONSOLE AT ONCE — the sentence in the provenance panel and the query in
   * the editor — because a panel counting one Run above a grid showing every Run's rows is two
   * scopes on one screen, which is the confusion this slice exists to delete rather than to
   * relocate.
   */
  const [scopedRun, setScopedRun] = useState<string | null>(focusRun);
  const [text, setText] = useState(() => openingQuery(dataset.name, focusRun));
  const [result, setResult] = useState<QueryResult | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [exported, setExported] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);
  const [schema, setSchema] = useState<SchemaEntry[] | null>(null);
  const [provenance, setProvenance] = useState<DatasetProvenance | null>(null);
  const [provenanceError, setProvenanceError] = useState<string | null>(null);
  /** Taller rows with wrapped text — off by default, because most Datasets are scalars and a grid
   *  of four-line rows shows a quarter as many of them. */
  const [tall, setTall] = useState(false);
  /** The cell an operator opened, if any. A grid row cannot show a markdown body; this can. */
  const [cell, setCell] = useState<{ column: string; value: unknown } | null>(null);
  /** The queries the operator kept, read from localStorage once on open and mirrored back on every
   *  change. Held in state, not read live, so a Save on one console does not re-render another. */
  const [presets, setPresets] = useState<SqlPreset[]>(() => loadPresets());
  /**
   * The Dataset RECORD as this page holds it (ADR 0029 §1) — seeded from the row that was opened and
   * then replaced by what each mutation ANSWERS with, because every tag route returns the Run's whole
   * post-state deviation.
   *
   * LOCAL, NOT RE-READ FROM THE CATALOG POLL. Subscribing this console to the app-level poll to
   * learn its own tag back is exactly the 2-second re-render #14 removed; the route's answer is the
   * same fact, sooner, and costs nothing.
   */
  const [tags, setTags] = useState<string[]>(() => [...(dataset.tags ?? [])]);
  /** A tag route's refusal (an empty tag is a 400) or a wire failure — the server's own sentence,
   *  surfaced rather than swallowed, the same treatment the listing gives it. */
  const [recordError, setRecordError] = useState<string | null>(null);

  // The editor's value is read by the ⌘↵ handler, which is registered once — a state read inside it
  // would close over the first render's text and run a stale query.
  const latest = useRef(text);
  latest.current = text;

  const run = useCallback(async () => {
    const sql = latest.current.trim();
    if (!sql) return;
    setBusy(true);
    setError(null);
    try {
      setResult(await runQuery(sql, { limit: PAGE_ROWS }));
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      setResult(null);
    } finally {
      setBusy(false);
    }
  }, []);

  // The schema of every dataset, because a query may JOIN — but only THIS one's columns are shown
  // in the sidebar. Autocomplete over the whole catalog costs nothing and refusing it would make a
  // join harder to type than it is to run.
  useEffect(() => {
    fetchSchema()
      .then(setSchema)
      .catch(() => setSchema([]));
  }, []);

  // Run the opening query on arrival. Opening a dataset to see a blank grid and a Run button is a
  // console; opening it to see its first hundred rows is a dataset.
  useEffect(() => {
    void run();
  }, [run]);

  // WHO WROTE THIS, asked ONCE, when the Dataset is opened. A group-by is a real column scan —
  // unlike the listing, which is catalog metadata — so it must never ride the app-level poll that
  // already refreshes `/api/datasets` every 2 s. `live` guards the setState: opening one Dataset
  // and closing it before the read lands would otherwise write provenance into an unmounted
  // console, or into the next one.
  const { kind: datasetKind, name: datasetName } = dataset;
  useEffect(() => {
    let live = true;
    setProvenance(null);
    setProvenanceError(null);
    fetchProvenance({ kind: datasetKind, name: datasetName })
      .then((p) => {
        if (live) setProvenance(p);
      })
      .catch((e: unknown) => {
        if (live) setProvenanceError(e instanceof Error ? e.message : String(e));
      });
    return () => {
      live = false;
    };
  }, [datasetKind, datasetName]);

  /**
   * Scope this console to one **Run**, or back to every Run with `null`.
   *
   * NO SECOND READ. The provenance answer already holds every Run's bucket, so the scoped counts
   * are summed out of the statement that is already on the page — issuing a `count(*)` per Run
   * would be a second statement about a second moment, which is exactly the division this whole
   * slice refuses. What DOES re-run is the editor's query, because the grid must show the rows the
   * counts are talking about.
   */
  const scopeTo = useCallback(
    (nextRun: string | null) => {
      setScopedRun(nextRun);
      const sql = openingQuery(datasetName, nextRun);
      setText(sql);
      latest.current = sql;
      void run();
    },
    [datasetName, run]
  );

  /**
   * TAGGING FROM THE DATASET'S OWN PAGE — the same act as the listing's `+ tag`, keyed the same way
   * (the record is the **Run**'s, ADR 0029 §4), and stated in the same prompt so an operator does not
   * learn two rules for one thing.
   *
   * Where it differs from the listing is only what happens after the route answers: the listing
   * reloads the whole catalog because a row on it changed; this page takes the post-state the route
   * returned. That is what keeps a tag from costing this console a re-render of its grid.
   */
  const recordRun = dataset.runId ?? '';
  const mutateRecord = useCallback(async (action: () => Promise<DatasetDeviation>) => {
    setRecordError(null);
    try {
      setTags((await action()).tags);
    } catch (e) {
      setRecordError(e instanceof Error ? e.message : String(e));
    }
  }, []);
  const addTag = useCallback(() => {
    if (!recordRun) return;
    const tag = window
      .prompt(
        `${tagPrompt({ name: datasetName, runId: recordRun, temporary: dataset.temporary === true })}\n\nAdd a tag`
      )
      ?.trim();
    if (!tag) return;
    void mutateRecord(() => addDatasetTag(recordRun, tag));
  }, [dataset.temporary, datasetName, mutateRecord, recordRun]);
  const removeTag = useCallback(
    (tag: string) => {
      if (!recordRun) return;
      void mutateRecord(() => removeDatasetTag(recordRun, tag));
    },
    [mutateRecord, recordRun]
  );

  const namespace = useMemo<SQLNamespace>(() => {
    const ns: SQLNamespace = {};
    for (const d of schema ?? []) ns[d.name] = d.columns.map((c) => c.name);
    return ns;
  }, [schema]);

  const extensions = useMemo(
    () => [sqlLang({ schema: namespace, upperCaseKeywords: true })],
    [namespace]
  );

  const columns = useMemo(
    () => schema?.find((s) => s.name === dataset.name)?.columns ?? [],
    [dataset.name, schema]
  );

  // Rebuild the column definitions ONLY when the schema (names + types) or the row-height mode
  // changes — never on a new `result` object with the same shape. A same-shape re-query, and every
  // 2 s catalog tick that re-renders this tree, hands AG Grid the SAME defs reference so it keeps
  // the operator's column widths, sort, filter and scroll. Keying on `result` identity was the bug.
  const resultColumns = result?.columns ?? EMPTY_COLUMNS;
  const gridKey = resultsGridKey(resultColumns, tall);
  const columnDefs = useMemo<ColDef[]>(
    () => buildColumnDefs(resultColumns, tall),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [gridKey]
  );
  const rowData = useMemo(
    () =>
      (result?.rows ?? []).map((row) =>
        Object.fromEntries(result!.columns.map((c, i) => [c.name, row[i]]))
      ),
    [result]
  );

  const doExport = async (format: ExportFormat) => {
    const sql = latest.current.trim();
    if (!sql) return;
    setError(null);
    try {
      await downloadExport(sql, format, `kontra-${dataset.name}`);
      setExported(format);
      setTimeout(() => setExported(null), 2500);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  };

  // Hand over the COMMAND, not a script or a URL: it names the dataset the way the operator does,
  // scopes to this version and dispatch, and runs against the same catalog with no row limit.
  const copyCommand = async () => {
    try {
      await navigator.clipboard.writeText(queryCommand(dataset));
      setCopied(true);
      setTimeout(() => setCopied(false), 2500);
    } catch {
      /* clipboard blocked — the command is in the caption below */
    }
  };

  // Insert at the cursor rather than replacing: clicking a column while composing a query should
  // extend it, not discard it.
  const insert = (fragment: string) =>
    setText((t) => (t.endsWith(' ') || t.endsWith('\n') ? t + fragment : `${t} ${fragment}`));

  // The presets to OFFER here: this dataset's own plus any global ones. A preset built on another
  // dataset's columns would be a Binder Error waiting to be clicked.
  const shownPresets = useMemo(() => visiblePresets(presets, dataset.name), [presets, dataset.name]);

  // Keep the current SQL under a name. Scoped to this dataset by default, so the name means the same
  // query every time it is applied. Re-saving under an existing name overwrites, never duplicates.
  const savePreset = () => {
    const sql = latest.current.trim();
    if (!sql) return;
    const name = window.prompt('Name this query preset')?.trim();
    if (!name) return;
    const next = upsertPreset(presets, { name, sql, dataset: dataset.name });
    setPresets(next);
    savePresets(next);
  };
  // Apply REPLACES the editor and runs, like a snippet — a saved query is a whole query, not a
  // fragment to splice. It touches only the editor, never the saved set, so the others survive.
  const applyPreset = (p: SqlPreset) => {
    setText(p.sql);
    latest.current = p.sql;
    void run();
  };
  const deletePreset = (p: SqlPreset) => {
    const next = removePreset(presets, p);
    setPresets(next);
    savePresets(next);
  };

  const badge = datasetBadge(datasetState(dataset));
  /** What retention would do with this Dataset, from the record this page is holding — so tagging it
   *  changes the sentence beside the tag, in the same click. */
  const expiry = datasetExpiry(
    {
      temporary: dataset.temporary,
      tags,
      state: datasetState(dataset),
      updatedAt: dataset.updatedAt,
      kind: dataset.kind,
      runId: dataset.runId,
      // The plural too: the sweep addresses every contributing Run, so a Dataset several Runs wrote
      // IS swept even though `runId` is absent for it. Reading the singular alone drew "not swept"
      // over exactly the largest, longest-lived Datasets.
      runs: dataset.contributingRuns,
    },
    Date.now()
  );
  /**
   * Every **Run** known to have written this Dataset: the lake's own `run_id` statistics, plus the
   * one the ledger resolved for the record. The UNION, for the reason `datasets/listing.ts` records —
   * counting only the first reports "no runs" for a Dataset whose Run is perfectly well known.
   */
  const knownRuns = [
    ...new Set([...(dataset.contributingRuns ?? []), ...(dataset.runId ? [dataset.runId] : [])]),
  ];
  /** The Run this Dataset can honestly be ATTRIBUTED to, or none. `contributingRunsPartial` means the
   *  set is a lower bound, and a bound is never a singular answer. */
  const producingRun =
    dataset.contributingRunsPartial !== true && knownRuns.length === 1 ? knownRuns[0]! : '';

  return (
    <div className="dataset-page">
      <div className="flex flex-wrap items-center gap-3">
        <Button variant="outline" size="sm" onClick={onClose}>
          ← Datasets
        </Button>
        <h1 className="m-0 font-mono text-[15px] font-semibold">{dataset.name}</h1>
        <Badge variant="outline" className={`text-[9.5px] ${badge.className}`} title={badge.title}>
          {badge.label}
        </Badge>
        {/* THE HEADER COUNTS WHAT WAS OPENED, and says so. Opened from a dispatch it is that
            `version=…/dt=…` partition; opened from the listing it is the whole Dataset — and the
            panel below counts every Run that wrote the name. That pair, unlabelled, was this
            surface's whole bug: 623 here and 1,246 there, both correct.

            AND IT MOVES WHILE A RUN WRITES. `DatasetAccrual` owns the live half — its own component
            precisely so a count landing every two seconds cannot re-render this console and hand the
            results grid, the editor or a query in flight anything new. See its header. */}
        <DatasetAccrual dataset={dataset} />
        <span className="font-mono text-[11px] tabular-nums text-muted-foreground" data-testid="dataset-header-identity">
          {[
            fmtBytes(dataset.bytes),
            dataset.version ? `v${dataset.version}` : '',
            dataset.dt ?? '',
          ]
            .filter(Boolean)
            .join(' · ')}
        </span>

        <div className="ml-auto flex items-center gap-1">
          <Button size="sm" variant="outline" onClick={() => void copyCommand()} title={queryCommand(dataset)}>
            {copied ? <Check /> : <Copy />}
            {copied ? 'Copied' : 'CLI command'}
          </Button>
          {EXPORT_FORMATS.map((f) => (
            <Button key={f} variant="outline" size="sm" onClick={() => void doExport(f)}>
              {exported === f ? <Check /> : <Download />}
              {f}
            </Button>
          ))}
        </div>
      </div>

      {/* THE RECORD AND THE CLOCK: what is on this Dataset, what that means for how long it lives,
          and the Run that produced it — one line, above the provenance panel that details the rest.

          THE THREE BELONG TOGETHER because they are one decision. A tag means KEEP (ADR 0029 §3),
          and until now the page only said so inside the tooltip of the `+ tag` button — the one
          place an operator who has not decided to tag will never read. Beside it the expiry reading
          says what happens if they do not, so the button and its consequence are one glance apart. */}
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5 text-[10.5px]">
        <TagStrip
          dataset={dataset.name}
          tags={tags}
          runId={recordRun}
          temporary={dataset.temporary === true}
          onAdd={addTag}
          onRemove={removeTag}
        />
        {/* MEASURED AT RENDER, and coarse on purpose — hours and days. A second-by-second countdown
            would imply a precision the sweep schedule does not have, and would need a clock ticking
            in this component, which is the thing the accrual split exists to avoid. */}
        <span
          data-testid="dataset-expiry"
          data-disposition={expiry.disposition}
          className={`rounded border px-1.5 py-0.5 font-mono ${
            expiry.due
              ? 'border-amber-500/40 bg-amber-500/10 text-amber-700 dark:text-amber-400'
              : 'border-border text-muted-foreground'
          }`}
          title={expiry.title}
        >
          {expiry.label}
        </span>
        {/* PROVENANCE IS A LINK, NOT AN INFERENCE. One click lands on the Run that wrote these rows —
            offered ONLY where a single Run honestly did. A Dataset several Runs appended to has no
            singular answer (ADR 0029 §2), so its Runs are linked one by one in the panel below
            instead of the newest of five being dressed up as "the" producer here. */}
        {producingRun ? (
          <button
            type="button"
            data-testid="dataset-open-run"
            className="flex items-center gap-1 font-mono text-muted-foreground hover:text-foreground"
            title={`Open the Run that wrote this Dataset — ${producingRun}`}
            onClick={() => openRun(producingRun)}
          >
            <ExternalLink size={10} />
            {producingRun}
          </button>
        ) : knownRuns.length > 1 || dataset.contributingRunsPartial ? (
          <span
            data-testid="dataset-runs-plural"
            className="text-muted-foreground"
            title="Several Runs appended to this Dataset, so no single one produced it. Each is named — and reachable — in the panel below."
          >
            {dataset.contributingRunsPartial ? 'at least ' : ''}
            {knownRuns.length} runs · see below
          </span>
        ) : null}
        {recordError && <span className="run-status run-failed">{recordError}</span>}
      </div>

      {/* WHICH MACHINES AND WHICH RUNS WROTE IT, above the console rather than inside a tooltip.
          It sits here, between the Dataset's identity and its rows, because it is a property of
          those rows: a reader who is about to compare across them needs to know first whether they
          came from one Machine or four, from one Run or two, and whether any of them says so at
          all. Clicking a Run scopes this whole screen to it — see `scopeTo`. */}
      <DatasetProvenancePanel
        provenance={provenance}
        error={provenanceError}
        scopedRun={scopedRun}
        onScopeRun={scopeTo}
        onOpenRun={openRun}
      />

      <div className="flex min-h-0 flex-1 gap-3">
        {/* This dataset's columns. Click one to put it in the query. */}
        <aside className="w-52 shrink-0 overflow-auto rounded-md border p-2 text-xs">
          <div className="mb-2 font-semibold uppercase tracking-wide text-muted-foreground">
            Schema
          </div>
          {columns.length === 0 ? (
            <p className="text-muted-foreground">
              {schema === null ? 'reading…' : 'the catalog reports no columns for this dataset'}
            </p>
          ) : (
            <ul className="m-0 list-none p-0">
              {columns.map((c) => (
                <li key={c.name} className="border-b border-border/60 py-1 last:border-0">
                  <button
                    type="button"
                    className="flex w-full items-baseline justify-between gap-2 text-left hover:text-primary"
                    onClick={() => insert(c.name)}
                    title={c.type}
                  >
                    <span className="truncate font-mono">{c.name}</span>
                    <span className="shrink-0 text-[9.5px] text-muted-foreground">
                      {c.type.toLowerCase()}
                    </span>
                  </button>
                </li>
              ))}
            </ul>
          )}

          {/* The three queries every operator types next. They REPLACE the editor rather than
              appending, because a snippet is a starting point and half of one glued to the end of
              a half-written query is neither. */}
          {columns.length > 0 && (
            <>
              <div className="mb-1.5 mt-3.5 font-semibold uppercase tracking-wide text-muted-foreground">
                Snippets
              </div>
              {snippets(dataset.name, columns[0]!.name).map((s) => (
                <button
                  key={s.label}
                  type="button"
                  className="mb-0.5 block w-full truncate rounded bg-muted px-1.5 py-1 text-left font-mono text-[10px] text-muted-foreground hover:text-foreground"
                  onClick={() => {
                    setText(s.sql);
                    latest.current = s.sql;
                    void run();
                  }}
                  title={s.sql}
                >
                  {s.label}
                </button>
              ))}
            </>
          )}

          {/* Presets: the queries the operator KEPT, beside the throwaway snippets. Surviving a
              reload is the point — a join or filter you composed is worth more than the blank box
              you would otherwise retype it into. Applying one leaves every other one in place. */}
          <div className="mb-1.5 mt-3.5 flex items-center justify-between">
            <span className="font-semibold uppercase tracking-wide text-muted-foreground">
              Presets
            </span>
            <button
              type="button"
              data-testid="preset-save"
              className="rounded px-1 text-[10px] text-muted-foreground hover:text-foreground"
              title="Save the current SQL as a named preset"
              onClick={savePreset}
            >
              + Save
            </button>
          </div>
          {shownPresets.length === 0 ? (
            <p className="text-[10px] text-muted-foreground">
              None yet — write a query and Save it to keep it across reloads.
            </p>
          ) : (
            shownPresets.map((p) => (
              <div key={`${p.dataset ?? ''}:${p.name}`} className="group mb-0.5 flex items-center gap-1">
                <button
                  type="button"
                  data-testid={`preset-apply-${p.name}`}
                  className="min-w-0 flex-1 truncate rounded bg-muted px-1.5 py-1 text-left font-mono text-[10px] text-muted-foreground hover:text-foreground"
                  onClick={() => applyPreset(p)}
                  title={p.sql}
                >
                  {p.name}
                </button>
                <button
                  type="button"
                  data-testid={`preset-delete-${p.name}`}
                  className="shrink-0 rounded p-0.5 text-muted-foreground opacity-0 hover:text-foreground group-hover:opacity-100"
                  title="Delete this preset"
                  onClick={() => deletePreset(p)}
                >
                  <X size={11} />
                </button>
              </div>
            ))
          )}
        </aside>

        <div className="flex min-h-0 min-w-0 flex-1 flex-col gap-2">
          <div className="flex items-center gap-2">
            <span className="text-[10px] uppercase tracking-wide text-muted-foreground">SQL</span>
            <span className="text-[10px] text-muted-foreground">⌘↵ to run</span>
            {/* ROW HEIGHT, as a control rather than as something to drag per row. A Dataset of
                scalars wants the dense grid; one carrying a crawl's markdown wants four lines of
                every cell, and neither is the right default for the other. */}
            <Button
              size="sm"
              variant={tall ? 'secondary' : 'ghost'}
              className="ml-auto"
              data-testid="tall-rows"
              aria-pressed={tall}
              onClick={() => setTall((v) => !v)}
              title={
                tall
                  ? 'back to one line per row'
                  : 'wrap long values over four lines — for Datasets carrying documents'
              }
            >
              {/* One label, whatever the state. "Dense rows" on a button while the rows ARE dense
                  is ambiguous in the worst way — it reads as both a description and an action, and
                  the two point opposite ways. The pressed state says which it is. */}
              {tall ? <Rows2 /> : <Rows3 />}
              Tall rows
            </Button>
            <Button size="sm" onClick={() => void run()} disabled={busy}>
              <Play />
              {busy ? 'Running…' : 'Run query'}
            </Button>
          </div>

          <div className="overflow-hidden rounded-md border">
            <CodeMirror
              value={text}
              height="120px"
              theme={theme === 'dark' ? 'dark' : 'light'}
              extensions={extensions}
              onChange={setText}
              onKeyDown={(e) => {
                if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
                  e.preventDefault();
                  void run();
                }
              }}
            />
          </div>

          {error && <span className="run-status run-failed whitespace-pre-wrap">{error}</span>}

          <div className="flex min-h-0 flex-1 gap-2">
            <div className="min-h-0 min-w-0 flex-1">
              <AgGridReact
                theme={gridTheme(theme)}
                columnDefs={columnDefs}
                rowData={rowData}
                enableCellTextSelection
                suppressFieldDotNotation
                tooltipShowDelay={400}
                rowHeight={tall ? TALL_ROW_HEIGHT : undefined}
                defaultColDef={GRID_DEFAULT_COL_DEF}
                // A CELL IS OPENABLE. `enableCellTextSelection` means a click is also a selection,
                // which is why this is the whole affordance and not a double-click: selecting text
                // in a cell still works, and one click also says what the cell holds.
                onCellClicked={(e) =>
                  setCell({ column: e.colDef.field ?? '', value: e.value })
                }
              />
            </div>
            {cell && (
              <CellInspector
                column={cell.column}
                value={cell.value}
                onClose={() => setCell(null)}
              />
            )}
          </div>

          <div className="flex items-center gap-3.5 overflow-hidden text-[10.5px]">
            <span
              className={`truncate font-mono ${error ? 'text-destructive' : result ? 'text-emerald-500' : 'text-muted-foreground'}`}
              data-testid="query-status"
            >
              {/* `returned` is the scope of THIS number: rows this query answered with, which is
                  neither the Dataset's total nor one Run's contribution — and, when it is capped,
                  not even all of its own result. A bare "1,000 rows" beside a panel counting
                  1,246 is the same unlabelled pair one screen up. */}
              {error
                ? error.split('\n')[0]
                : result
                  ? `Query OK · ${result.rows.length.toLocaleString()} row${result.rows.length === 1 ? '' : 's'} returned${result.truncated ? ` · capped at ${PAGE_ROWS.toLocaleString()} — export for all` : ''}`
                  : busy
                    ? 'running…'
                    : 'Ready — ⌘↵ or Run query'}
            </span>
            <span className="flex-1" />
            <span className="shrink-0 whitespace-nowrap text-muted-foreground">
              a query that works here works verbatim in <code>kontra dataset query</code>
            </span>
            <span className="shrink-0 whitespace-nowrap font-mono tabular-nums text-muted-foreground">
              {result ? `${result.elapsedMs} ms` : ''}
            </span>
          </div>
        </div>
      </div>
    </div>
  );
}
