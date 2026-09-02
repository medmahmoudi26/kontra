import { Badge, Spark, SPARK_COLORS } from '@kontra/frontend';

function Frame({ children }: { children: React.ReactNode }) {
  return (
    <div className="dark">
      <div className="rounded-lg bg-background p-4 text-foreground">{children}</div>
    </div>
  );
}

/**
 * EVERY NUMBER BELOW IS A READING, NOT A CURVE.
 *
 * `spark.ts` says it in the file header: a mock fills its charts from `Math.sin(tick)` and a
 * control plane must not. So each series here is 48 samples of something the product actually
 * measures — rows per second between two `/api/datasets` catalog polls, at the 2 s cadence the
 * surfaces use, which is 96 seconds of history per line. The shapes are the shapes a lake really
 * makes: a materialization commits a Batch at a time, so a Dataset being written is a floor of
 * trickle with a spike whenever a node's batch lands, never a smooth wave.
 */

/**
 * The whole fleet, summed — ten droplets running `subfinder@0.3.1` over 119 apexes. The floor is
 * a few dozen rows a second of found hosts; each spike is one Machine's Batch committing.
 */
const FLEET = [
  0, 0, 12, 48, 96, 132, 210, 604, 188, 74, 52, 96,
  140, 512, 366, 120, 88, 64, 41, 33, 58, 96, 187, 742,
  310, 120, 84, 62, 55, 90, 148, 226, 588, 204, 96, 71,
  60, 44, 38, 52, 88, 166, 430, 172, 90, 63, 47, 55,
];

/** One Dataset a Run is still appending to: `subdomains.subfinder`, mid-sweep. */
const OPEN = [
  24, 61, 88, 142, 96, 55, 318, 121, 74, 62, 88, 130,
  402, 156, 91, 68, 52, 44, 71, 118, 264, 512, 187, 96,
  70, 58, 49, 63, 104, 232, 88, 61, 47, 55, 82, 148,
  356, 132, 78, 60, 51, 44, 66, 97, 190, 84, 58, 47,
];

/**
 * `http.probe`, sealed halfway through the window: probe finished its targets and the Dataset
 * stopped growing. The tail really is zero — that is a MEASUREMENT, and it is why the line runs
 * flat along the bottom instead of stopping.
 */
const SEALED = [
  208, 174, 191, 165, 152, 178, 143, 121, 96, 84, 77, 61,
  54, 40, 33, 21, 14, 9, 6, 3, 1, 0, 0, 0,
  0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
  0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
];

/** A standalone list nothing writes to: `scope_paid` was loaded once by `kontra dataset create`
 *  and has had no producer since. Ninety-six seconds of measured nothing. */
const IDLE = new Array(48).fill(0) as number[];

/**
 * A Dataset that first appeared on the LAST poll — one sample, and `SPARK_MIN` is 2, so there is
 * no line. Two points is the minimum that can express a direction, and a direction is the only
 * thing a sparkline says.
 */
const ONE_SAMPLE = [96];

/** The rail's own footer markup around the chart — `SideNav`'s `Count`, which is not exported. */
function Count({ label, value, className }: { label: string; value: string; className?: string }) {
  return (
    <div className="flex justify-between font-mono text-[10px] text-muted-foreground">
      <span>{label}</span>
      <span className={`tabular-nums ${className ?? 'text-foreground'}`}>{value}</span>
    </div>
  );
}

/**
 * WHERE IT LIVES — the bottom of the persistent left rail (`w-60`, its real width), which is the
 * only place on the console a chart is on screen at all times. Three rails, because this is where
 * the empty-series rule is load-bearing rather than tidy.
 *
 * LEFT — a sweep in progress. A caption naming the measurement, 160 × 26 of FILLED sparkline, and
 * the reading below it in `tabular-nums`. The chart says the shape of the last minute and a half
 * and nothing else; the number is what an operator quotes.
 *
 * MIDDLE — a page opened two seconds ago. One sample exists, so there is no line and the count is
 * `—`. This is the case `SideNav` has a comment about: a flat line here would say the fleet is
 * running and committing nothing, which is the single most alarming thing this rail can state, and
 * it would be stating it about a page that has not finished loading.
 *
 * RIGHT — a fleet that really is committing nothing: 48 samples, every one zero. The line is
 * drawn, flat along the bottom, and the count is `0`. Same rail, same minute and a half, opposite
 * meaning — and the two are told apart without reading a word.
 */
export function InTheRail() {
  const rails = [
    { key: 'sweeping', series: FLEET, units: '55', inFlight: '1' },
    { key: 'just opened', series: ONE_SAMPLE, units: '—', inFlight: '0' },
    { key: 'nothing running', series: IDLE, units: '0', inFlight: '0' },
  ];
  return (
    <Frame>
      <div className="flex gap-2">
        {rails.map((r) => (
          <div
            key={r.key}
            className="flex w-60 flex-col gap-2 rounded-md border border-border bg-card px-3.5 py-3"
          >
            <div className="text-[9px] uppercase tracking-wide text-muted-foreground">
              Fleet throughput
            </div>
            <div className="h-[26px] text-muted-foreground">
              <Spark
                series={r.series}
                color={SPARK_COLORS.throughput}
                width={160}
                height={26}
                fill
                title="rows committed per second across every Dataset, sampled from the catalog"
              />
            </div>
            <Count label="units/s" value={r.units} />
            <Count label="live panes" value="0" />
            <Count label="in flight" value={r.inFlight} />
          </div>
        ))}
      </div>
    </Frame>
  );
}

/**
 * One line per Dataset, which is how the Datasets console draws it — and why every line is
 * normalised to ITS OWN maximum rather than to a shared axis.
 *
 * `subdomains.subfinder` peaks at 512 rows/s and `nameservers.nscheck` at 9. On one axis the
 * second would be a flat line, i.e. indistinguishable from a Dataset nobody is writing to — which
 * is the one thing this column exists to tell apart. The heights are therefore NOT comparable
 * between rows and the shapes are; the comparable number is the count beside it.
 */
export function PerDataset() {
  const rows = [
    { name: 'subdomains.subfinder', state: 'open' as const, rows: '37,214', series: OPEN, color: '#fcd34d' },
    { name: 'http.probe', state: 'sealed' as const, rows: '8,902', series: SEALED, color: SPARK_COLORS.throughput },
    {
      name: 'nameservers.nscheck',
      state: 'open' as const,
      rows: '1,246',
      color: '#fcd34d',
      series: [2, 4, 3, 6, 9, 5, 3, 2, 4, 7, 8, 4, 2, 1, 3, 5, 9, 6, 3, 2, 2, 4, 6, 8,
        5, 3, 2, 3, 5, 7, 9, 4, 2, 1, 2, 4, 6, 5, 3, 2, 3, 5, 8, 6, 4, 2, 2, 3],
    },
  ];
  return (
    <Frame>
      <div className="flex flex-col gap-1.5">
        {rows.map((r) => (
          <div
            key={r.name}
            className="flex items-center gap-2 rounded-md border border-border bg-card px-2 py-1.5"
          >
            <span className="min-w-0 flex-1 truncate font-mono text-[11px]">{r.name}</span>
            <Badge
              variant="outline"
              className={`text-[9.5px] ${
                r.state === 'open'
                  ? 'border-amber-500/40 bg-amber-500/15 text-amber-700 dark:text-amber-400'
                  : 'border-transparent bg-emerald-500/15 text-emerald-600 dark:text-emerald-400'
              }`}
            >
              {r.state}
            </Badge>
            <span className="shrink-0 font-mono text-[11px] tabular-nums text-muted-foreground">
              {r.rows}
            </span>
            <span className="w-24 shrink-0 text-muted-foreground">
              <Spark
                series={r.series}
                color={r.color}
                width={80}
                height={16}
                title={`rows per second written to ${r.name}, across every dispatch`}
              />
            </span>
          </div>
        ))}
      </div>
    </Frame>
  );
}

/**
 * THE RULE THIS COMPONENT EXISTS TO ENFORCE, in three rows that must never look alike.
 *
 * TOP — a Dataset being written: a real shape, and the number beside it is a rate.
 *
 * MIDDLE — a MEASURED ZERO. Forty-eight samples were taken and every one was 0, so the line is
 * drawn, flat along the bottom. "Nothing is landing in this Dataset" is a finding, and an
 * operator who is waiting for a sweep to produce rows needs to see it stated.
 *
 * BOTTOM — NOT MEASURED. One sample exists (`SPARK_MIN` is 2), so nothing is drawn and the slot
 * is empty. A flat line here would claim the middle row's finding about a Dataset nobody has
 * sampled twice yet — the same lie as a green health light over a machine nobody probed. The
 * empty slot keeps its width so the column still lines up; absence is a shape, not a gap in the
 * layout.
 */
export function AbsentIsNotZero() {
  const rows = [
    { name: 'subdomains.subfinder', note: '48 samples · 96 s', series: OPEN, color: '#fcd34d' },
    { name: 'scope_paid', note: '48 samples · all zero', series: IDLE, color: SPARK_COLORS.idle },
    { name: 'lame.nscheck', note: '1 sample · first seen on this poll', series: ONE_SAMPLE, color: '#fcd34d' },
  ];
  return (
    <Frame>
      <div className="flex flex-col gap-1.5">
        {rows.map((r) => (
          <div
            key={r.name}
            className="flex items-center gap-2 rounded-md border border-border bg-card px-2 py-1.5"
          >
            <span className="min-w-0 flex-1">
              <span className="block truncate font-mono text-[11px]">{r.name}</span>
              <span className="block text-[9.5px] text-muted-foreground">{r.note}</span>
            </span>
            <span className="w-24 shrink-0 text-muted-foreground">
              <Spark
                series={r.series}
                color={r.color}
                width={80}
                height={16}
                title={`rows per second written to ${r.name}`}
              />
            </span>
          </div>
        ))}
      </div>
    </Frame>
  );
}

/**
 * `SPARK_COLORS` names a MEANING, never a hex value, so a caller says what the line is a reading
 * of and the palette stays one decision. Each row below is the measurement that colour is for.
 *
 * `idle` is deliberately `currentColor` — the muted foreground — so a series with no movement
 * recedes instead of competing with a Dataset that is filling. It is the one colour that is not a
 * hue, and that is the statement: nothing here needs attention.
 */
export function ByMeaning() {
  const rows = [
    { key: 'throughput', color: SPARK_COLORS.throughput, what: 'rows committed per second, fleet-wide', series: FLEET },
    { key: 'rate', color: SPARK_COLORS.rate, what: 'Batches dispatched per second to kontra-subfinder', series: [1, 2, 2, 3, 5, 4, 2, 1, 1, 2, 4, 6, 5, 3, 2, 2, 1, 3, 5, 7, 4, 2, 1, 1, 2, 3, 6, 8, 5, 3, 2, 1, 2, 4, 5, 3, 2, 1, 1, 2, 3, 5, 4, 2, 1, 1, 2, 3] },
    { key: 'activity', color: SPARK_COLORS.activity, what: 'workflow history events per second', series: [6, 14, 22, 9, 7, 31, 12, 8, 6, 5, 18, 44, 15, 9, 7, 6, 11, 26, 38, 13, 8, 6, 5, 9, 21, 52, 17, 10, 7, 6, 8, 15, 29, 11, 7, 6, 5, 8, 19, 34, 12, 8, 6, 5, 7, 13, 24, 9] },
    { key: 'failure', color: SPARK_COLORS.failure, what: 'activity retries per second on kontra-probe-7', series: [0, 0, 0, 0, 0, 1, 0, 0, 2, 5, 9, 14, 11, 8, 12, 17, 21, 16, 9, 6, 11, 19, 24, 18, 12, 7, 4, 6, 10, 15, 9, 5, 3, 2, 4, 7, 5, 3, 1, 2, 3, 2, 1, 0, 1, 0, 0, 0] },
    { key: 'idle', color: SPARK_COLORS.idle, what: 'rows per second into scope_paid — nobody writes it', series: IDLE },
  ];
  return (
    <Frame>
      <div className="flex flex-col gap-1.5">
        {rows.map((r) => (
          <div
            key={r.key}
            className="flex items-center gap-2 rounded-md border border-border bg-card px-2 py-1.5"
          >
            <span className="w-24 shrink-0 font-mono text-[11px]">{r.key}</span>
            <span className="min-w-0 flex-1 truncate text-[10.5px] text-muted-foreground">
              {r.what}
            </span>
            <span className="w-24 shrink-0 text-muted-foreground">
              <Spark series={r.series} color={r.color} width={100} height={16} title={r.what} />
            </span>
          </div>
        ))}
      </div>
    </Frame>
  );
}

/**
 * ONE measurement — the fleet's last 96 seconds — at the four sizes the console draws it, because
 * the SVG is always 100% wide and `preserveAspectRatio="none"` stretches it. That is what lets one
 * component sit in a 46 px tile banner and a 160 px nav footer without a second implementation,
 * and it is also the constraint on the shape: a series that needs a legible x-axis would be
 * unreadable at three of these four.
 *
 * The filled variant is the rail's alone. Fill reads as volume, which is right for a total across
 * every Dataset and wrong for one row of a table, where six filled areas stacked would be a chart
 * nobody asked for.
 */
export function AtEverySize() {
  const sizes = [
    { where: 'left rail footer', w: 160, h: 26, fill: true, box: 160 },
    { where: 'default', w: 100, h: 16, fill: false, box: 100 },
    { where: 'Datasets row', w: 80, h: 16, fill: false, box: 80 },
    { where: 'dispatch row', w: 80, h: 12, fill: false, box: 80 },
    { where: 'tile banner', w: 46, h: 13, fill: false, box: 46 },
  ];
  return (
    <Frame>
      <div className="flex flex-col gap-1.5">
        {sizes.map((s) => (
          <div
            key={s.where}
            className="flex items-center gap-2 rounded-md border border-border bg-card px-2 py-1.5"
          >
            <span className="min-w-0 flex-1 truncate text-[10.5px] text-muted-foreground">
              {s.where}
            </span>
            <span className="shrink-0 font-mono text-[10px] tabular-nums text-muted-foreground">
              {s.w}×{s.h}
            </span>
            <span className="shrink-0 text-muted-foreground" style={{ width: s.box }}>
              <Spark
                series={FLEET}
                color={SPARK_COLORS.throughput}
                width={s.w}
                height={s.h}
                fill={s.fill}
                title="rows committed per second across every Dataset"
              />
            </span>
          </div>
        ))}
      </div>
    </Frame>
  );
}
