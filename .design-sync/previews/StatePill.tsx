import { StatePill } from '@kontra/frontend';

function Frame({ children }: { children: React.ReactNode }) {
  return (
    <div className="dark">
      <div className="rounded-lg bg-background p-4 text-foreground">{children}</div>
    </div>
  );
}

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex items-baseline gap-2">
      <span className="w-[136px] shrink-0 text-[10px] uppercase tracking-wider text-muted-foreground">
        {label}
      </span>
      <div className="flex flex-wrap items-baseline gap-1.5">{children}</div>
    </div>
  );
}

/**
 * Execution, whose authority is Temporal. These are the five words a workflow can be in,
 * and the pill is the only place the colour for each is decided.
 */
export function Execution() {
  return (
    <Frame>
      <Row label="execution">
        <StatePill state="starting" testid="s1" />
        <StatePill state="running" testid="s2" />
        <StatePill state="completed" testid="s3" />
        <StatePill state="failed" testid="s4" />
        <StatePill state="cancelled" testid="s5" />
      </Row>
    </Frame>
  );
}

/**
 * Materialization, whose authority is the ledger — a different question about the same run.
 * `writing` and `running` rhyme on purpose (both in flight) without matching: they are
 * answers to two questions and a reader must not read one for the other (ADR 0017).
 */
export function Materialization() {
  return (
    <Frame>
      <div className="flex flex-col gap-2">
        <Row label="execution">
          <StatePill state="running" testid="m1" />
          <StatePill state="completed" testid="m2" />
        </Row>
        <Row label="materialization">
          <StatePill state="writing" testid="m3" />
          <StatePill state="complete" testid="m4" />
        </Row>
      </div>
    </Frame>
  );
}

/**
 * The projection over both dimensions, which is what a list row shows when it has room
 * for one pill. `output_failed` is the state that exists because a run can finish and
 * still have written nothing.
 */
export function Projected() {
  return (
    <Frame>
      <Row label="projection">
        <StatePill state="executing" testid="p1" />
        <StatePill state="finalizing" testid="p2" />
        <StatePill state="output_failed" testid="p3" />
      </Row>
    </Frame>
  );
}

/**
 * AN UNKNOWN NEVER GETS THE COLOUR OF AN ANSWER. `unknown` and `unrecorded` fall through
 * to the muted default, because a pill that looked settled would be the confident green
 * label over lost work that ADR 0017 exists to prevent.
 */
export function NothingIsKnown() {
  return (
    <Frame>
      <div className="flex flex-col gap-2">
        <Row label="not measured">
          <StatePill state="unknown" testid="u1" />
          <StatePill state="unrecorded" testid="u2" />
          <StatePill state="pending" testid="u3" />
        </Row>
        <p className="m-0 text-[10.5px] italic text-muted-foreground">
          muted, not grey-green: nobody looked, and the pill says so
        </p>
      </div>
    </Frame>
  );
}

/** Two sizes. `small` is the one a dense table row uses; the default is a header's. */
export function Sizes() {
  return (
    <Frame>
      <div className="flex flex-col gap-2">
        <Row label="default">
          <StatePill state="running" testid="z1" title="workflow-run 5b2c… started 41s ago" />
          <StatePill state="output_failed" testid="z2" />
        </Row>
        <Row label="small">
          <StatePill state="running" testid="z3" small />
          <StatePill state="output_failed" testid="z4" small />
        </Row>
      </div>
    </Frame>
  );
}
