/**
 * Who wrote this Dataset, and which of its rows a number is about — the panel on the inside of
 * the Datasets surface.
 *
 * IT ANSWERS THE QUESTION THE RUN STATUS CANNOT. A four-Machine `nscheck` run finished
 * `completed` and correct, and nothing in the console could say whether all four Machines had
 * produced a row. "Four were asked, three produced rows" is the sentence, and the half this
 * panel owns is the second: a Dataset knows who wrote it, never who was asked, so the Machines
 * that produced nothing are ABSENT here rather than drawn as zero.
 *
 * THREE STATES, TOLD APART ON SIGHT. A measured hostname, a gap where nothing was recorded, and
 * the legacy `w`/`0` placeholder — which is most of the data written before provenance travelled
 * with the Batch. They differ on four independent channels at once (border style, fill, text
 * colour, glyph) plus the label itself, so the distinction survives greyscale and a skimming
 * operator. The rules for which is which live in `datasets/provenance.ts`; this file only draws.
 *
 * AND EVERY COUNT ON IT SAYS WHAT IT COUNTED. A Dataset name spans **Runs** — `lame` holds 1,246
 * rows from two Runs of one workflow while the listing showed 623 for one of them — so the header
 * states its scope in words, and the Runs dimension lets an operator scope the whole console to
 * one of them and read "623 of 1,246 rows · this run". Both numbers in that sentence come out of
 * ONE statement, which is the only way it is honest: two counts issued seconds apart against a
 * Dataset a Run is still appending to are two moments, not a ratio (`datasets/scope.ts`).
 *
 * IT TAKES `provenance` AND RENDERS. The fetch is the console's, issued once when a Dataset is
 * opened — never on the listing poll, which already runs every 2 s against a catalog scan.
 */

import { ExternalLink } from 'lucide-react';

import {
  drawsDistribution,
  entriesOf,
  headline,
  provenanceBadge,
  runScope,
  type DatasetProvenance as Provenance,
  type ProvenanceDimension,
  type ProvenanceEntry,
} from '../datasets/provenance';
import { asOfText, countText } from '../datasets/scope';

export interface DatasetProvenanceProps {
  /** `null` while the request is in flight. A Dataset with nothing recorded is a VALUE, not null. */
  provenance: Provenance | null;
  /** The server's own sentence when the read failed. An empty panel would read as "no Machines". */
  error?: string | null;
  /**
   * The **Run** the console is currently scoped to, when it is scoped to one. `null` is the
   * Dataset's own scope — every Run — which is what the editor below reads by default.
   */
  scopedRun?: string | null;
  /**
   * Scope the console to one Run, or back to every Run with `null`.
   *
   * Absent means the panel is read-only: it still NAMES every Run that wrote the Dataset, because
   * naming them is what stops a count being read as the whole when it is one Run's share.
   */
  onScopeRun?: (run: string | null) => void;
  /**
   * GO TO one Run — the Workflows surface, that Run open.
   *
   * A SECOND VERB ON THE SAME ROW, and deliberately not a replacement for the first: scoping keeps
   * you here and narrows every count to that Run's rows; opening leaves for the Run itself. Both are
   * offered because provenance that only NAMES a Run makes the operator copy an id out and hunt for
   * it, which is the inference this panel exists to replace with a link.
   */
  onOpenRun?: (run: string) => void;
}

/** The three dimensions, and the noun each is counted in. */
const DIMENSIONS: ReadonlyArray<{ id: ProvenanceDimension; noun: string; caption: string }> = [
  // Runs first: it is the dimension the header's count is scoped in, and the one an operator
  // clicks to answer "is this number this run's, or every run's?".
  { id: 'run', noun: 'Run', caption: 'appended to it' },
  { id: 'machine', noun: 'Machine', caption: 'wrote these rows' },
  { id: 'version', noun: 'Actor version', caption: 'produced them' },
];

export default function DatasetProvenance({
  provenance,
  error,
  scopedRun = null,
  onScopeRun,
  onOpenRun,
}: DatasetProvenanceProps): JSX.Element {
  // Which dimensions this Dataset can be asked about at all. `run_id` and the Machine/version
  // pair are gated separately because they arrived separately: a table may carry one and not the
  // other, and answering about the one it has is not the same as answering about both.
  const dimensions = DIMENSIONS.filter((d) =>
    d.id === 'run' ? provenance?.carriesRun : provenance?.carriesProvenance
  );

  const state = error
    ? 'error'
    : provenance === null
      ? 'reading'
      : dimensions.length === 0
        ? 'none'
        : provenance.groups.length === 0
          ? 'empty'
          : 'measured';

  const scope = state === 'measured' && scopedRun ? runScope(provenance!, scopedRun) : null;

  return (
    <section
      data-testid="dataset-provenance"
      data-state={state}
      data-scoped-run={scopedRun ?? ''}
      className="shrink-0 rounded-md border border-border px-3 py-2"
    >
      <div className="flex flex-wrap items-baseline gap-2">
        <span className="text-[10px] uppercase tracking-wide text-muted-foreground">Provenance</span>
        {/* WHAT THESE COUNTS COUNTED, said in the caption rather than left to be inferred. The
            header above shows the dispatch the operator opened; this covers every Run that ever
            wrote the name, because that is what the editor below reads — `SELECT * FROM <name>`,
            unpartitioned. Two correct numbers with nothing to tell them apart is the incident
            this plane exists to prevent.

            AND AS OF WHEN. A Dataset a Run is still appending to grows, so this total is a fact
            about the moment the statement answered and says so. */}
        {state === 'measured' && (
          <span
            data-testid="provenance-scope"
            data-rows={provenance!.rows}
            className="font-mono text-[10.5px] text-muted-foreground"
            title="Every Run that ever wrote this name — the same rows the query below reads. The row count in the header above is the one dispatch you opened."
          >
            {countText(provenance!.rows, 'dataset', provenance!.kind)} ·{' '}
            {asOfText(provenance!.measuredAt)}
          </span>
        )}
        {/* SCOPED TO ONE RUN: the part and the whole, in one sentence, out of one statement. Not a
            second query — `runScope` sums the buckets the same scan produced, so the ratio is a
            fact about one snapshot rather than about two moments. */}
        {scope && (
          <span
            data-testid="provenance-run-scope"
            data-run={scope.run}
            data-rows={scope.part.rows}
            data-share={scope.share === null ? '' : scope.share.toFixed(4)}
            className="flex items-baseline gap-1.5 rounded border border-primary/50 bg-primary/10 px-1.5 py-0.5 font-mono text-[10.5px]"
            title={`Rows this Run appended, out of every Run's — both counted by the one statement that answered at ${new Date(scope.whole.measuredAt).toLocaleString()}.`}
          >
            <span className="truncate">{scope.run}</span>
            <span className="tabular-nums">{scope.sentence}</span>
            {onScopeRun && (
              <button
                type="button"
                data-testid="provenance-clear-scope"
                className="text-muted-foreground hover:text-foreground"
                onClick={() => onScopeRun(null)}
                title="Back to every Run"
              >
                ✕
              </button>
            )}
          </span>
        )}
      </div>

      {state === 'error' && <p className="m-0 mt-1 text-[11px] text-destructive">{error}</p>}
      {state === 'reading' && <p className="m-0 mt-1 text-[11px] text-muted-foreground">reading…</p>}
      {state === 'none' && (
        <p className="m-0 mt-1 text-[11px] text-muted-foreground">
          This Dataset carries no Machine, Actor version or Run: an operator-loaded list has none of
          those columns, so there was never anything to record.
        </p>
      )}
      {state === 'empty' && (
        <p className="m-0 mt-1 text-[11px] text-muted-foreground">
          No rows yet, so nothing has written a Machine.
        </p>
      )}

      {state === 'measured' && (
        <div className="mt-1.5 grid gap-x-6 gap-y-2 [grid-template-columns:repeat(auto-fit,minmax(190px,1fr))]">
          {dimensions.map((d) => (
            <Dimension
              key={d.id}
              provenance={provenance!}
              scopedRun={scopedRun}
              onScopeRun={onScopeRun}
              onOpenRun={onOpenRun}
              {...d}
            />
          ))}
        </div>
      )}
    </section>
  );
}

function Dimension({
  provenance,
  id,
  noun,
  caption,
  scopedRun,
  onScopeRun,
  onOpenRun,
}: {
  provenance: Provenance;
  id: ProvenanceDimension;
  noun: string;
  caption: string;
  scopedRun: string | null;
  onScopeRun?: (run: string | null) => void;
  onOpenRun?: (run: string) => void;
}): JSX.Element {
  const entries = entriesOf(provenance, id);
  const distribution = drawsDistribution(entries);
  return (
    <div data-testid={`provenance-${id}s`}>
      <div className="flex items-baseline gap-1.5 text-[11px]">
        <span data-testid={`provenance-${id}s-headline`} className="font-medium">
          {headline(entries, noun)}
        </span>
        <span className="text-[10.5px] text-muted-foreground">{caption}</span>
      </div>
      {/* Nothing is hidden and nothing is rolled up into "+N more" — a Machine that produced
          rows is a Machine that must be nameable, and a Run that wrote rows is a Run whose share
          of the total must be readable. A long list scrolls inside the panel instead of pushing
          the console's own grid off the screen. */}
      <ul className="m-0 mt-1 max-h-32 list-none space-y-1 overflow-auto p-0">
        {entries.map((e) => (
          <Row
            key={e.key}
            entry={e}
            id={id}
            distribution={distribution}
            // ONLY A RUN IS AN ADDRESS, and only a recorded one: a gap has no value to filter on,
            // and `node`/`version` are partitions of the rows rather than a thing to scope to —
            // the console's editor reads the whole name, which is the point of the panel.
            onSelect={
              id === 'run' && onScopeRun && e.state === 'recorded' && e.value !== null
                ? () => onScopeRun(e.value === scopedRun ? null : e.value)
                : undefined
            }
            // ONLY A RECORDED RUN IS SOMEWHERE TO GO. A gap and the legacy placeholder name no Run,
            // so they get no link — a link that lands on `w` would be an address invented out of
            // the absence of one.
            onOpen={
              id === 'run' && onOpenRun && e.state === 'recorded' && e.value !== null
                ? () => onOpenRun(e.value!)
                : undefined
            }
            run={id === 'run' ? (e.value ?? '') : ''}
            selected={id === 'run' && e.value !== null && e.value === scopedRun}
          />
        ))}
      </ul>
    </div>
  );
}

function Row({
  entry,
  id,
  distribution,
  onSelect,
  onOpen,
  run,
  selected,
}: {
  entry: ProvenanceEntry;
  id: ProvenanceDimension;
  distribution: boolean;
  /** Present only where the value addresses something — see the call site. */
  onSelect?: () => void;
  /** Present only on a RECORDED Run — the one value that is somewhere to go. */
  onOpen?: () => void;
  /** The Run this row names, for the link's own label. Empty on every other dimension. */
  run?: string;
  selected?: boolean;
}): JSX.Element {
  const badge = provenanceBadge(entry.state);
  const body = (
    <>
      <span aria-hidden className="shrink-0 text-[8px] leading-none">
        {badge.glyph}
      </span>
      <span className={`truncate font-mono ${badge.valueClassName}`}>{entry.label}</span>
      <span className="ml-auto shrink-0 tabular-nums">{entry.rows.toLocaleString()}</span>
      {/* MEASURED, and drawn only when there is something to distribute: a lone bucket at 100%
          would draw a distribution over a Dataset that has none. `bg-current` keeps the bar the
          row's own colour, so the placeholder's share never reads as a Machine's. */}
      {distribution && (
        <span
          aria-hidden
          className="h-1 w-10 shrink-0 overflow-hidden rounded-full bg-muted"
          data-share={entry.share.toFixed(4)}
        >
          <span className="block h-full bg-current" style={{ width: `${entry.share * 100}%` }} />
        </span>
      )}
    </>
  );
  return (
    <li
      // One element per value, and the key is unique within the list — `recorded-kf-dns-01`,
      // `legacy-w`, `unrecorded`. A test hook that matches a dozen elements has broken this
      // suite before.
      data-testid={`provenance-${id}-${entry.key}`}
      data-state={entry.state}
      data-value={entry.value ?? ''}
      data-rows={entry.rows}
      data-selected={selected ? 'true' : 'false'}
      title={badge.title}
      className={`flex items-center gap-1.5 rounded border px-1.5 py-0.5 text-[10.5px] ${badge.className} ${
        selected ? 'ring-1 ring-primary' : ''
      }`}
    >
      {onSelect ? (
        <button
          type="button"
          aria-pressed={selected ? 'true' : 'false'}
          className="flex min-w-0 flex-1 items-center gap-1.5 text-left"
          onClick={onSelect}
          title={`Scope every count and the query below to this Run — ${entry.rows.toLocaleString()} of these rows are its.`}
        >
          {body}
        </button>
      ) : (
        body
      )}
      {/* THE SECOND VERB: leave for the Run itself. Distinct from the row's own click, which scopes
          this console to it — two different places, so two different targets rather than one that
          has to be guessed at. */}
      {onOpen && (
        <button
          type="button"
          data-testid={`provenance-open-${entry.key}`}
          className="shrink-0 rounded p-0.5 text-muted-foreground hover:bg-accent hover:text-foreground"
          title={`Open run ${run} — the Workflows surface, this Run's thread`}
          onClick={onOpen}
        >
          <ExternalLink size={9} />
        </button>
      )}
    </li>
  );
}
