import { SideDockControls } from '@kontra/frontend';

function Frame({ children }: { children: React.ReactNode }) {
  return (
    <div className="dark">
      <div className="rounded-lg bg-background p-4 text-foreground">{children}</div>
    </div>
  );
}

const noop = () => {};

/**
 * A sidecar's whole layout, as the header's controls see it. In the product this is `useSideDock`,
 * which reads and writes `localStorage`; a card passes the state it wants to show and no-op
 * handlers, because a control that has already been pressed is not a state a capture can hold.
 */
function dock(side: 'left' | 'right', width = 264) {
  return {
    width,
    side,
    collapsed: false,
    setWidth: noop,
    setSide: noop,
    flip: noop,
    setCollapsed: noop,
    toggle: noop,
  };
}

/**
 * The page a sidecar is docked to. The controls are about the panel's relationship to THIS — which
 * edge it holds and whether it holds any width at all — so the card draws both.
 */
function Shell({ children }: { children: React.ReactNode }) {
  return (
    <div
      className="flex overflow-hidden rounded-md border border-border bg-card"
      style={{ height: 232 }}
    >
      {children}
    </div>
  );
}

function Side({
  width,
  side,
  grow,
  children,
}: {
  width: number;
  side: 'left' | 'right';
  /** Share the shell equally instead of holding a pinned width — for the card that draws three
   *  panels and nothing else, where a leftover strip of page would be the only thing not a panel. */
  grow?: boolean;
  children: React.ReactNode;
}) {
  return (
    <div
      className={`flex min-w-0 flex-col overflow-hidden bg-background ${
        side === 'left' ? 'border-r' : 'border-l'
      } border-border`}
      style={grow ? { flex: '1 1 0' } : { flex: `0 0 ${width}px`, width }}
    >
      {children}
    </div>
  );
}

function Main({ children }: { children: React.ReactNode }) {
  return <div className="flex min-w-0 flex-1 flex-col overflow-hidden bg-background">{children}</div>;
}

/**
 * The header, as `WorkflowsPage` and `ScratchInspector` both draw it: the panel's name, what it
 * holds, then the control group pushed to the right edge by its own `ml-auto`.
 */
function Header({ title, count, children }: { title: string; count?: string; children?: React.ReactNode }) {
  return (
    <div className="flex shrink-0 items-baseline gap-2 border-b border-border px-2 py-1.5">
      <span className="text-[9.5px] uppercase tracking-wide text-muted-foreground">{title}</span>
      {count ? (
        <span className="font-mono text-[10px] tabular-nums text-muted-foreground">{count}</span>
      ) : null}
      {children}
    </div>
  );
}

function Body({ lines }: { lines: string }) {
  return (
    <pre className="m-0 overflow-hidden px-2 py-1 font-mono text-[10px] leading-4 text-muted-foreground">
      {lines}
    </pre>
  );
}

function Rows({ rows }: { rows: readonly (readonly [string, string])[] }) {
  return (
    <div className="flex flex-col gap-0.5 overflow-hidden p-1.5">
      {rows.map(([name, path]) => (
        <div key={name} className="rounded px-2 py-1.5">
          <div className="font-mono text-[11.5px]">{name}</div>
          <p className="m-0 mt-0.5 break-all font-mono text-[9.5px] leading-snug text-muted-foreground">
            {path}
          </p>
        </div>
      ))}
    </div>
  );
}

const WORKFLOWS = [
  ['nscheck', '/root/kontra-local/.kontra/workflows/nscheck'],
  ['probe-sweep', '/srv/checkout/examples/python/probe'],
  ['subfinder-scan', '~/.kontra/workflows/subfinder-scan'],
] as const;

const MACHINES = [
  ['kontra-probe-1', '10.124.0.4 · probe@0.2.0'],
  ['kontra-probe-2', '10.124.0.5 · probe@0.2.0'],
  ['kontra-subf-1', '10.124.0.6 · subfinder'],
] as const;

const EDITOR = `@actor.method(takes=Target, emits=Probe)
async def head(self, batch: Batch[Target]) -> Batch[Probe]:
    async with httpx.AsyncClient(timeout=10) as c:
        return [await probe(c, t.url) for t in batch]

@actor.method(takes=Target, emits=Body)
async def fetch(self, batch: Batch[Target]) -> Batch[Body]:
    async with httpx.AsyncClient(timeout=30) as c:
        return [await body(c, t.url, cap=512_000) for t in batch]

if __name__ == "__main__":
    actor.serve(queue="kontra-probe")`;

/**
 * The pair in the Workflows list's header, docked LEFT, beside the editor it takes its width from.
 *
 * The flip glyph points where the panel WOULD GO, not where it is: `⇥` on a left-docked panel means
 * "move this to the right of the page". Reading it as a statement of the current side is the
 * mistake the tooltip exists to stop — it says the destination in words.
 */
export function InTheWorkflowsHeader() {
  return (
    <Frame>
      <Shell>
        <Side width={264} side="left">
          <Header title="Workflows" count="7 workflows">
            <SideDockControls dock={dock('left')} label="the workflow list" testid="workflows-dock" />
          </Header>
          <Rows rows={WORKFLOWS} />
        </Side>
        <Main>
          <Header title="probe.py" count="48 lines · Python" />
          <Body lines={EDITOR} />
        </Main>
      </Shell>
    </Frame>
  );
}

/**
 * The same pair on a RIGHT-docked panel, and the only thing that changed is the arrow: `⇤`, back
 * to the left. One glyph for the destination rather than a pair of "dock left"/"dock right"
 * buttons, because a control whose two halves are always half-disabled teaches nothing.
 *
 * `✕` beside it does not close the panel, it COLLAPSES it — the rail that is left carries the name
 * and brings it back at this width, which is why the two controls belong to one group.
 */
export function InTheInspectorHeader() {
  return (
    <Frame>
      <Shell>
        <Main>
          <Header title="Scratch" count="6 nodes · 5 edges" />
          <Body
            lines={`targets.seed ──▶ probe.head ──▶ probes.head
                          └─▶ nscheck.lame
probes.head  ──▶ report.render

saved 12:04 · kontra scratch open sweep-aug
probe.head    takes Target, emits Probe
nscheck.lame  takes Probe,  emits Finding
report.render reads probes.head, findings.lame

run probe-sweep · 34 batches · 12 completed`}
          />
        </Main>
        <Side width={304} side="right">
          <Header title="Inspector" count="method">
            <SideDockControls
              dock={dock('right', 304)}
              label="the inspector"
              testid="scratch-inspector-dock"
            />
          </Header>
          <Body
            lines={`probe.head
takes   Target { url: str, timeout: float }
emits   Probe  { status: int, headers: dict }
reads   targets.seed — 2,880 rows
writes  probes.head — open
served  10.124.0.4, 10.124.0.5
queue   kontra-probe · 2 pollers
digest  sha256:9f2c41b7d0ae5c38e1b6

fallout nscheck.lame reads probes.head
        and has not been re-checked`}
          />
        </Side>
      </Shell>
    </Frame>
  );
}

/**
 * BOTH CONTROLS, TOGETHER, ON EVERY PANEL THAT HAS EITHER. A panel that collapses but cannot move
 * is still stuck on the wrong edge; one that moves but cannot collapse still costs its width on a
 * narrow screen. Drawn as one group in one place, an operator learns the pair once — here on three
 * panels of three different surfaces, two docked left and one right.
 */
export function TheSamePairOnEveryPanel() {
  return (
    <Frame>
      <Shell>
        <Side width={252} side="left" grow>
          <Header title="Workflows" count="7 workflows">
            <SideDockControls dock={dock('left', 252)} label="the workflow list" testid="workflows-dock" />
          </Header>
          <Rows rows={WORKFLOWS} />
        </Side>
        <Side width={252} side="left" grow>
          <Header title="Machines" count="12 terminals">
            <SideDockControls dock={dock('left', 252)} label="the Machines tree" testid="monitor-tree-dock" />
          </Header>
          <Rows rows={MACHINES} />
        </Side>
        <Side width={252} side="right" grow>
          <Header title="Inspector" count="dataset">
            <SideDockControls
              dock={dock('right', 252)}
              label="the inspector"
              testid="scratch-inspector-dock"
            />
          </Header>
          <Body
            lines={`probes.head
open · 2,880 rows · 34 batches
written by probe.head
run probe-sweep · 12:04:29`}
          />
        </Side>
      </Shell>
    </Frame>
  );
}

/**
 * The narrowest a sidecar goes — `SIDE_BOUNDS.min`, 180px — with a name and a count already in the
 * header. `ml-auto` is what keeps the pair on the right edge instead of wrapping under the title:
 * the controls are the last thing a header may drop, because collapsing is the answer to a panel
 * being too narrow to read.
 */
export function OnANarrowHeader() {
  return (
    <Frame>
      <Shell>
        <Side width={180} side="left">
          <Header title="Workflows" count="7">
            <SideDockControls dock={dock('left', 180)} label="the workflow list" testid="workflows-dock" />
          </Header>
          <Rows rows={WORKFLOWS} />
        </Side>
        <Main>
          <Header title="probe.py" count="48 lines · Python · UTF-8" />
          <Body lines={EDITOR} />
        </Main>
      </Shell>
    </Frame>
  );
}
