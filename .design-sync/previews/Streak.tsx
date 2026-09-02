import { StatePill, Streak } from '@kontra/frontend';

function Frame({ children }: { children: React.ReactNode }) {
  return (
    <div className="dark">
      <div className="rounded-lg bg-background p-4 text-foreground">{children}</div>
    </div>
  );
}

/**
 * The same frame with the card's own padding cut back.
 *
 * MEASURED, not cosmetic: the run header's grid is `auto-fit, minmax(148px, 1fr)`, so four tiles
 * split whatever the container has. At a 900 px capture with `p-4` each tile gets 209 px, and
 * `UNITS COMMITTED` plus its note needs about 220 — so the product's own `truncate` fires and the
 * label reads `UNITS COMMITT…`. The console is wider than a card, so this is the card's problem
 * rather than the component's; the padding buys the 16 px that keeps the label whole.
 */
function WideFrame({ children }: { children: React.ReactNode }) {
  return (
    <div className="dark">
      <div className="rounded-lg bg-background p-2 text-foreground">{children}</div>
    </div>
  );
}

type Status = 'completed' | 'failed' | 'cancelled' | 'running' | 'pending';

/**
 * `run/api.ts:fmtDuration`, which is not on the barrel. The label is the CALLER's to compose —
 * `runState.ts:streakOf` builds `${runId} · ${status} · ${fmtDuration(ms)}` — so it is composed
 * here the same way rather than invented.
 */
function fmt(ms: number): string {
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${String(s % 60).padStart(2, '0')}s`;
  return `${Math.floor(m / 60)}h ${String(m % 60).padStart(2, '0')}m`;
}

const MIN = 60_000;

/**
 * Twenty nightly runs of one caller workflow, oldest at the left — `STREAK_RUNS` is 20 and
 * `streakOf` reverses the list, so the newest bar is the rightmost.
 *
 * THE DURATIONS ARE THE SHAPE OF THE WORK, not a curve: a full `subfinder@0.3.1` sweep of 119
 * apexes across ten droplets takes eighteen to forty-six minutes depending on how many resolvers
 * time out. The two that are not green are the two ways a sweep really ends early — a run
 * cancelled three minutes in, and one that died in twelve seconds because the worker raised a
 * `TabError` at import and the queue was served by nothing.
 */
const RUNS: ReadonlyArray<[Status, number]> = [
  ['completed', 19 * MIN],
  ['completed', 21 * MIN],
  ['cancelled', 3 * MIN + 41_000],
  ['completed', 24 * MIN],
  ['completed', 22 * MIN],
  ['failed', 12_000],
  ['completed', 26 * MIN],
  ['completed', 31 * MIN],
  ['completed', 28 * MIN],
  ['completed', 25 * MIN],
  ['completed', 46 * MIN],
  ['completed', 33 * MIN],
  ['completed', 27 * MIN],
  ['completed', 24 * MIN],
  ['completed', 29 * MIN],
  ['completed', 38 * MIN],
  ['completed', 30 * MIN],
  ['completed', 26 * MIN],
  ['completed', 23 * MIN],
  ['completed', 25 * MIN],
];

/** The bars `streakOf` hands `Streak`: wall time, how it ended, and the run it is. The label is
 *  the hover text, and a run id is `<workflow>-<epoch>` — `history.test.ts`'s `nscheck-1786831339`. */
function bars(runs: ReadonlyArray<[Status, number]> = RUNS, workflow = 'subfinder-sweep') {
  return runs.map(([status, ms], i) => ({
    ms,
    status,
    label: `${workflow}-${1_786_400_000 + i * 86_400} · ${status} · ${fmt(ms)}`,
  }));
}

/** How many of the NEWEST runs completed before the first one that did not — `greenStreak`, which
 *  reads the list newest-first and stops at the first bar that is not `completed`. */
function green(runs: ReadonlyArray<[Status, number]> = RUNS): number {
  let n = 0;
  for (let i = runs.length - 1; i >= 0; i--) {
    if (runs[i]![0] !== 'completed') break;
    n += 1;
  }
  return n;
}

/** The stat tile a Streak is read inside — `RunStats.tsx`'s `ReadingView`, which is not exported. */
function Stat({
  label,
  value,
  note,
  alarming,
  streak,
}: {
  label: string;
  value: string;
  note: string;
  alarming?: boolean;
  streak?: ReturnType<typeof bars>;
}) {
  return (
    <div className="min-w-0 rounded-xl border border-border bg-card px-3.5 py-2.5">
      <div className="flex items-end justify-between gap-2">
        <span
          className={`font-mono text-[23px] font-bold leading-none tracking-tight ${
            alarming ? 'text-rose-400' : ''
          }`}
        >
          {value}
        </span>
        <span className="h-[17px] w-[58px] shrink-0">
          {streak ? <Streak bars={streak} width={100} height={17} /> : null}
        </span>
      </div>
      <div className="mt-1.5 flex min-w-0 items-baseline gap-1.5">
        <span className="truncate font-mono text-[10.5px] uppercase tracking-wider text-muted-foreground">
          {label}
        </span>
        <span className="shrink-0 font-mono text-[10px] text-muted-foreground">{note}</span>
      </div>
    </div>
  );
}

/**
 * A younger workflow — `nscheck_zones`, run ten times since it was written.
 *
 * TEN AND NOT TWENTY, because 58 px is 58 px: twenty bars there are under three pixels each, and a
 * red bar three pixels wide is a colour an operator cannot see. `STREAK_RUNS` caps the window at
 * twenty and most workflows are well under it, so the tiles below draw the case where the chart
 * actually carries its second channel. The enlarged card is where twenty is legible.
 */
const SHORT: ReadonlyArray<[Status, number]> = [
  ['completed', 8 * MIN],
  ['failed', 26_000],
  ['completed', 7 * MIN],
  ['completed', 9 * MIN],
  ['cancelled', 1 * MIN + 12_000],
  ['completed', 6 * MIN],
  ['completed', 11 * MIN],
  ['completed', 8 * MIN],
  ['completed', 7 * MIN],
  ['completed', 9 * MIN],
];

/** The run the header below is about: this one died seven minutes in, and broke the streak. */
const BROKEN: ReadonlyArray<[Status, number]> = [...SHORT.slice(0, 9), ['failed', 7 * MIN]];

/**
 * WHERE IT LIVES — the fourth tile of a Run's header, in 58 × 17 pixels beside a 23px number.
 *
 * The number is the answer; the chart is the CONTEXT the number cannot carry. `0` on its own says
 * this run broke the streak and nothing else. The bars say the five before it were green, that this
 * one ran seven minutes before it died rather than failing at import the way the second-oldest did,
 * and that a healthy run of this workflow is about eight minutes — three readings out of 58 pixels,
 * none of which is worth its own stat tile.
 *
 * The tiles are the run's OTHER measured numbers, and each says where it got them: `units
 * committed` reads this run's materialization ledger and prints `unrecorded` rather than `0`,
 * because a ledger holding nothing and a run that committed nothing are different statements.
 * Only `retries` turns red — a broken streak is a fact, not an alert.
 */
export function InTheRunHeader() {
  return (
    <WideFrame>
      <div className="grid gap-2.5 [grid-template-columns:repeat(auto-fit,minmax(148px,1fr))]">
        <Stat label="units committed" value="—" note="unrecorded" />
        <Stat label="activities" value="342" note="scheduled" />
        <Stat label="retries" value="27" note="9 failed" alarming />
        <Stat
          label="run streak"
          value={String(green(BROKEN))}
          note={`green · last ${BROKEN.length}`}
          streak={bars(BROKEN, 'nscheck-zones')}
        />
      </div>
    </WideFrame>
  );
}

/**
 * THE NUMBER IS COUNTED FROM THE NEWEST END, and the three tiles say so from one window of runs.
 *
 * LEFT — five completed since the last one that was not. The older failure and the cancelled run
 * are still drawn, because a streak of 5 that follows a crash is a different situation from a clean
 * ten.
 *
 * MIDDLE — the same ten runs with the NEWEST one failed, seven minutes in. The streak is 0 and the
 * tile is not red: `alarming` belongs to the retries tile, and a broken streak is a fact, not an
 * alert.
 *
 * RIGHT — the newest run is still going, so the streak is also 0. That is the rule `BAR_FILL`
 * encodes with amber rather than green: a run in flight has not succeeded yet, and counting it
 * would be a claim made before the evidence. Its bar is shorter than the tallest because height is
 * wall time SO FAR — five minutes into a run that usually takes eight — and it grows while you
 * watch it.
 */
export function CountedFromTheNewest() {
  const stillGoing: ReadonlyArray<[Status, number]> = [
    ...SHORT.slice(0, 9),
    ['running', 5 * MIN],
  ];
  return (
    <Frame>
      <div className="grid gap-2.5 [grid-template-columns:repeat(auto-fit,minmax(148px,1fr))]">
        <Stat
          label="run streak"
          value={String(green(SHORT))}
          note={`green · last ${SHORT.length}`}
          streak={bars(SHORT, 'nscheck-zones')}
        />
        <Stat
          label="run streak"
          value={String(green(BROKEN))}
          note={`green · last ${BROKEN.length}`}
          streak={bars(BROKEN, 'nscheck-zones')}
        />
        <Stat
          label="run streak"
          value={String(green(stillGoing))}
          note={`green · last ${stillGoing.length}`}
          streak={bars(stillGoing, 'nscheck-zones')}
        />
      </div>
    </Frame>
  );
}

/**
 * THE TWO CHANNELS, enlarged so the encoding is visible — the console draws this at 58 px in a
 * stat tile and 112 px in the workflow list, and `preserveAspectRatio="none"` is what makes one
 * component fit both, so this is the same geometry stretched rather than a different chart.
 *
 * HEIGHT IS WALL TIME, relative to the longest run in the window, for the reason `sparkPoints`
 * normalises to its own maximum: a workflow whose runs take thirty seconds and one whose runs take
 * forty minutes both need to show which of their own runs was the slow one.
 *
 * COLOUR IS THE OUTCOME, and it is why a 3-pixel floor exists. The failure below lasted twelve
 * seconds against a 46-minute maximum — 0.4% of the height, which rounds to nothing — and a run
 * that failed instantly is the single most important bar in the window. It is drawn at the floor
 * so it is visible and hoverable, rather than being a run that appears not to have happened.
 */
export function HeightIsTimeColourIsOutcome() {
  return (
    <Frame>
      <div className="flex flex-col gap-2 rounded-md border border-border bg-card px-3 py-2">
        <div className="text-[9px] uppercase tracking-wide text-muted-foreground">
          subfinder-sweep · last 20 runs
        </div>
        <div style={{ height: 44 }}>
          <Streak bars={bars()} width={200} height={44} />
        </div>
        <div className="flex flex-wrap items-baseline gap-3 font-mono text-[10px] text-muted-foreground">
          <span>tallest · completed · 46m 00s</span>
          <span>cancelled · 3m 41s</span>
          <span>failed · 12s — drawn at the 3px floor</span>
        </div>
      </div>
    </Frame>
  );
}

/**
 * THE OTHER PLACE IT IS DRAWN — the Workflows rail, at its real 264 px, because the geometry is
 * half the point: the streak takes whatever the row has left after the pill, about 170 px, and the
 * bars are as thin as twenty runs in 170 px makes them.
 *
 * Here it is doing a different job from the run header's: not "how is this run going" but WHICH OF
 * THESE FILES IS THE ONE THAT KEEPS BREAKING, answered without opening any of them. `nscheck_zones`
 * is three red bars in five, and no other column on this rail could have said so.
 *
 * AND TWO ROWS HAVE NO CHART AT ALL. `Streak` returns `null` for an empty window — the same refusal
 * `Spark` makes below two samples — so a workflow that has never run draws nothing, and the pill
 * says `never run` in words. There is deliberately no zero-height bar and no grey placeholder:
 * either would be a drawing of a history that does not exist.
 *
 * The right-hand column is the same six windows stretched wide, which is the only way to see what
 * the rail is compressing — and the two files with no history are blank there too.
 */
export function InTheWorkflowList() {
  const rows: Array<{
    name: string;
    description: string;
    state: string;
    date: string;
    runs: ReadonlyArray<[Status, number]>;
  }> = [
    {
      name: 'subfinder_sweep.py',
      description: 'Resolve every apex in scope_paid and append what answers to subdomains.subfinder.',
      state: 'completed',
      date: '15/08/2026',
      runs: RUNS,
    },
    {
      name: 'probe_new_hosts.py',
      description: 'HEAD every host found since the last sweep; keep the status line and headers.',
      state: 'running',
      date: '15/08/2026',
      runs: [...RUNS.slice(6, 19), ['running', 4 * MIN + 8_000]],
    },
    {
      name: 'nscheck_zones.py',
      description: 'Ask every nameserver for the zone it claims to serve, and record the lame ones.',
      state: 'failed',
      date: '14/08/2026',
      runs: [
        ['completed', 8 * MIN],
        ['failed', 22_000],
        ['failed', 19_000],
        ['completed', 7 * MIN + 30_000],
        ['failed', 14_000],
      ],
    },
    {
      name: 'dedupe_hosts.py',
      description: 'Fold the sweep’s output into one row per host and seal the Dataset.',
      state: 'completed',
      date: '13/08/2026',
      runs: [
        ['completed', 2 * MIN + 10_000],
        ['completed', 1 * MIN + 52_000],
        ['completed', 2 * MIN + 4_000],
      ],
    },
    {
      name: 'crlf_probe.py',
      description: 'Saved from the workbench four minutes ago and never served.',
      state: 'never run',
      date: '',
      runs: [],
    },
    {
      name: 'desync_charset.py',
      description: 'Low-byte charset smuggling against the hosts probe found answering.',
      state: 'never run',
      date: '',
      runs: [],
    },
  ];
  return (
    <Frame>
      <div className="flex gap-2.5">
        <aside className="flex shrink-0 flex-col gap-0.5 rounded-md border border-border p-1.5" style={{ width: 264 }}>
          {rows.map((r) => (
            <div key={r.name} className="rounded px-2 py-2">
              <div className="flex min-w-0 items-center gap-1.5">
                <span className="min-w-0 flex-1 truncate font-mono text-[12px]">{r.name}</span>
                <span className="shrink-0 font-mono text-[9.5px] tabular-nums text-muted-foreground">
                  {r.date}
                </span>
              </div>
              <p className="m-0 mt-1 text-[10.5px] leading-snug text-muted-foreground">
                {r.description}
              </p>
              <div className="mt-1.5 flex items-end gap-2">
                <StatePill state={r.state} testid={`file-state-${r.name}`} small />
                <span className="min-w-0 flex-1 text-muted-foreground">
                  <Streak bars={bars(r.runs, r.name.replace('.py', '').replace(/_/g, '-'))} width={112} height={16} />
                </span>
              </div>
            </div>
          ))}
        </aside>
        {/* The same six windows with the width the rail cannot give them. Nothing is added and
            nothing is recoloured — only the viewBox is stretched, which is what
            `preserveAspectRatio="none"` is for, and the two files with no history stay blank in
            both columns. */}
        <div className="flex min-w-0 flex-1 flex-col gap-1.5">
          {rows.map((r) => (
            <div
              key={r.name}
              className="flex flex-1 items-center gap-2 rounded-md border border-border bg-card px-2 py-1.5"
            >
              <span
                className="shrink-0 truncate font-mono text-[10.5px] text-muted-foreground"
                style={{ width: 116 }}
              >
                {r.name.replace('.py', '')}
              </span>
              <span className="min-w-0 flex-1" style={{ height: 22 }}>
                <Streak bars={bars(r.runs, r.name.replace('.py', '').replace(/_/g, '-'))} width={200} height={22} />
              </span>
              <span className="shrink-0 font-mono text-[10px] tabular-nums text-muted-foreground">
                {r.runs.length === 0 ? 'no runs' : `${r.runs.length} runs`}
              </span>
            </div>
          ))}
        </div>
      </div>
    </Frame>
  );
}
