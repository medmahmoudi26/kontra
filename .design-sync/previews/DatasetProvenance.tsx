import { Badge, DatasetProvenance, type DatasetProvenanceProps } from '@kontra/frontend';

function Frame({ children }: { children: React.ReactNode }) {
  return (
    <div className="dark">
      <div className="rounded-lg bg-background p-4 text-foreground">{children}</div>
    </div>
  );
}

type Provenance = NonNullable<DatasetProvenanceProps['provenance']>;

/** The moment the ONE statement behind every number on this panel answered — 02:15:07 local. */
const MEASURED_AT = Date.UTC(2026, 7, 16, 2, 15, 7);

/**
 * One `/api/datasets/:name/provenance` answer.
 *
 * `rows` is DuckDB's own sum from the same scan as the buckets, never a second `count(*)` — a
 * Dataset a Run is still appending to grows between two statements, and a share taken across them
 * is wrong by whatever landed in between. The fixture adds the buckets for the same reason the
 * server does.
 */
function answer(
  name: string,
  groups: Array<[machine: string | null, version: string | null, run: string | null, rows: number]>,
  over: Partial<Provenance> = {}
): Provenance {
  return {
    name,
    kind: 'output',
    rows: groups.reduce((n, g) => n + g[3], 0),
    groups: groups.map(([machine, version, run, rows]) => ({ machine, version, run, rows })),
    carriesProvenance: true,
    carriesRun: true,
    measuredAt: MEASURED_AT,
    ...over,
  };
}

const noop = () => {};

/**
 * THE SENTENCE THE RUN STATUS CANNOT FORM: five Machines were asked, four wrote rows.
 *
 * This sweep reported `completed` and was correct. The half a Dataset can answer is the second one
 * — it knows who WROTE it and never who was ASKED — so `kontra-subfinder-8` is simply not here.
 * That absence is the finding: a Machine drawn as `0` would claim the lake measured it at zero,
 * and the reason this panel was built is a four-Machine run in which one produced nothing and no
 * surface in the console could say so.
 *
 * One Actor version and one Run, so neither of those columns draws a share bar. A lone bucket at
 * 100% is a distribution over something that is not distributed; the Machines column, which really
 * is distributed, draws four.
 */
export function FourOfFiveMachinesWrote() {
  return (
    <Frame>
      <DatasetProvenance
        provenance={answer('subdomains.subfinder', [
          ['kontra-subfinder-4', '0.3.1', 'subfinder-sweep-1786831339', 11_842],
          ['kontra-subfinder-6', '0.3.1', 'subfinder-sweep-1786831339', 9_517],
          ['kontra-subfinder-9', '0.3.1', 'subfinder-sweep-1786831339', 8_960],
          ['kontra-subfinder-7', '0.3.1', 'subfinder-sweep-1786831339', 6_895],
        ])}
      />
    </Frame>
  );
}

/** `lame`, as it stands: 1,246 rows under one name from two nightly Runs of one workflow. */
const TWO_RUNS = answer('lame', [
  ['kontra-nscheck-3', '0.1.0', 'nightly-2026-08-14', 340],
  ['kontra-nscheck-4', '0.1.0', 'nightly-2026-08-14', 283],
  ['kontra-nscheck-3', '0.1.0', 'nightly-2026-08-15', 411],
  ['kontra-nscheck-5', '0.1.0', 'nightly-2026-08-15', 212],
]);

/**
 * A DATASET NAME SPANS RUNS, and this is the panel scoped to one of them.
 *
 * `623 of 1,246 rows · this run` is the sentence the console could not previously form. It is
 * sayable at all only because both numbers come out of ONE scan: `runScope` sums the buckets that
 * same statement produced, so the ratio is a fact about one moment rather than about two. The
 * refusal is mechanical — `scope.ts:share` returns `null` unless the two counts carry the same
 * statement token, so a percentage can never be assembled from a catalog listing and a `count(*)`
 * issued seconds apart.
 *
 * Only a Run is clickable, and only a recorded one: a Machine is a partition of these rows rather
 * than an address, and a gap has no value to filter on. The scoped Run is ringed in the list and
 * the chip carries the way back to every Run.
 */
export function ScopedToOneRun() {
  return (
    <Frame>
      <DatasetProvenance
        provenance={TWO_RUNS}
        scopedRun="nightly-2026-08-14"
        onScopeRun={noop}
      />
    </Frame>
  );
}

/**
 * THE THREE VALUES, TOLD APART ON SIGHT — and this is the card the whole panel is for.
 *
 * `kontra-nscheck-3` is MEASURED: solid border, plain text, `●`. It ran the Method.
 *
 * `w (placeholder)` is the dead pair the publish activity substituted before provenance travelled
 * with the Batch — amber, struck through, `⚠`. It is a value the rows really carry, so it is shown
 * rather than dropped, and it names no Machine, so it is never counted as one: the headline says
 * `1 Machine` over three rows. It is claimed on the PAIR (`w` AND version `0`), which is what
 * keeps an Actor whose version really is `0` from being relabelled.
 *
 * `not recorded` is a GAP: dashed, italic, muted, `?`. Nothing knew — a Batch paged out of a
 * Dataset was produced by the lake and not by a Machine. No producer can emit NULL, which is
 * exactly why NULL is the representation.
 *
 * Four independent channels at once (border style, fill, text colour, glyph) plus the label, so
 * the three survive greyscale and a skimming operator. And note the Runs column: all three are
 * recorded there, including the Run whose rows carry the dead pair — `run_id` was never the
 * substituted column, and striking it through would delete a fact the lake holds.
 */
export function MeasuredPlaceholderAndGap() {
  return (
    <Frame>
      <DatasetProvenance
        provenance={answer('lame', [
          ['w', '0', 'nightly-2026-08-14', 623],
          ['kontra-nscheck-3', '0.1.0', 'nightly-2026-08-16', 443],
          [null, null, 'nightly-2026-08-15', 180],
        ])}
        onScopeRun={noop}
      />
    </Frame>
  );
}

/**
 * FOUR WAYS THERE IS NOTHING TO ANSWER WITH, and not one of them is an empty grid.
 *
 * 1 — NEVER RECORDED. An operator-loaded list has no `node`, no `version` and no `run_id` columns
 * at all, so there was never anything to record. This is a different fact from "every row is
 * unrecorded", and drawing it as a Dataset whose Machines were lost would send somebody looking
 * for a producer that never existed.
 *
 * 2 — EMPTY. The columns are there and no rows are. A Run has started and committed nothing yet.
 *
 * 3 — READING. `provenance` is `null` only while the request is in flight. A Dataset with nothing
 * recorded is a VALUE, not `null`, so this state is never reached by an answer.
 *
 * 4 — FAILED, in the SERVER's own sentence. An empty panel here would read as "no Machines", which
 * is a claim about the data instead of about the read that did not happen.
 */
export function WhenThereIsNothingToAnswer() {
  const rows: Array<{ note: string; props: DatasetProvenanceProps }> = [
    {
      note: 'scope_paid — a list loaded by `kontra dataset create`',
      props: {
        provenance: {
          name: 'scope_paid',
          kind: 'standalone',
          rows: 119,
          groups: [],
          carriesProvenance: false,
          carriesRun: false,
          measuredAt: MEASURED_AT,
        },
      },
    },
    {
      note: 'http.probe — dispatched four minutes ago, nothing committed yet',
      props: { provenance: answer('http.probe', []) },
    },
    { note: 'nameservers.nscheck — the scan is in flight', props: { provenance: null } },
    {
      note: 'lame — the group-by failed',
      props: {
        provenance: null,
        error: 'provenance: 502 could not read dataset provenance: DuckDB is not attached to the lake',
      },
    },
  ];
  return (
    <Frame>
      <div className="flex flex-col gap-2">
        {rows.map((r) => (
          <div key={r.note} className="flex flex-col gap-1">
            <span className="font-mono text-[10px] text-muted-foreground">{r.note}</span>
            <DatasetProvenance {...r.props} />
          </div>
        ))}
      </div>
    </Frame>
  );
}

/**
 * IT ASKS ONLY WHAT THE TABLE CARRIES, because the two halves arrived separately.
 *
 * TOP — a Dataset seeded into the lake with `run_id` and nothing else. It can be asked which Run
 * appended these rows, and the Machine and Actor version columns are not drawn at all rather than
 * drawn full of gaps: "this table has no such column" and "every row's value is missing" are
 * different sentences, and only the second is a problem to chase.
 *
 * BOTTOM — the mirror image, and the older half of the fleet: rows written before `run_id`
 * travelled with the Batch. Three Machines and two Actor versions are nameable, and the panel
 * stays silent about Runs — so nothing on this Dataset can be scoped to one, which is precisely
 * why the counts that ARE shown say what they counted.
 */
export function OnlyTheColumnsItHas() {
  return (
    <Frame>
      <div className="flex flex-col gap-2">
        <DatasetProvenance
          provenance={answer(
            'seeds.crawl',
            [[null, null, 'crawl-1786744800', 4_204]],
            { kind: 'standalone', carriesProvenance: false }
          )}
          onScopeRun={noop}
        />
        <DatasetProvenance
          provenance={answer(
            'headers.probe',
            [
              ['kontra-probe-1', '0.1.0', null, 2_610],
              ['kontra-probe-4', '0.2.0', null, 1_988],
              ['kontra-probe-7', '0.2.0', null, 1_204],
            ],
            { carriesRun: false }
          )}
          onScopeRun={noop}
        />
      </div>
    </Frame>
  );
}

/**
 * WHERE IT LIVES, and the two correct numbers it exists to keep apart.
 *
 * The header above is the dispatch an operator opened — one `version=…/dt=…` partition, 623 rows.
 * The panel underneath counts every Run that ever wrote the name — 1,246 — because that is what
 * the editor below it reads: `SELECT * FROM lame`, unpartitioned. Both were always correct and
 * neither was labelled, and an operator comparing them concluded the lake had lost half the rows.
 *
 * So each number states its scope in words, in the surface and never only in a tooltip, and the
 * panel adds `as of 02:15:07` — a Dataset a Run is still appending to grows, so a total is a fact
 * about the moment its statement answered.
 */
export function InTheDatasetConsole() {
  return (
    <Frame>
      <div className="flex flex-col gap-2 rounded-md border border-border bg-card p-3">
        <div className="flex flex-wrap items-baseline gap-2">
          <span className="font-mono text-[13px] font-semibold">lame</span>
          <Badge
            variant="outline"
            className="border-amber-500/40 bg-amber-500/15 text-[9.5px] text-amber-700 dark:text-amber-400"
          >
            open
          </Badge>
          <span className="font-mono text-[10.5px] tabular-nums text-muted-foreground">
            623 rows · this dispatch
          </span>
          <span className="font-mono text-[10px] text-muted-foreground">
            version=0.1.0/dt=2026-08-15
          </span>
        </div>
        <DatasetProvenance provenance={TWO_RUNS} scopedRun={null} onScopeRun={noop} />
        <div className="rounded-md border border-border bg-muted/60 p-2 font-mono text-[10.5px] text-muted-foreground">
          SELECT * FROM lame LIMIT 200
        </div>
      </div>
    </Frame>
  );
}
