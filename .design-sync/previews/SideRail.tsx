import { SideRail } from '@kontra/frontend';

function Frame({ children }: { children: React.ReactNode }) {
  return (
    <div className="dark">
      <div className="rounded-lg bg-background p-4 text-foreground">{children}</div>
    </div>
  );
}

/**
 * A rail is 28 pixels of a page and nothing on its own: what it means is the width the page just
 * got back. So every card draws the shell — the rail, and beside it the surface that grew into
 * the space the panel gave up.
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

function Main({ children }: { children: React.ReactNode }) {
  return <div className="flex min-w-0 flex-1 flex-col overflow-hidden bg-background">{children}</div>;
}

function Head({ title, right }: { title: string; right?: string }) {
  return (
    <div className="flex shrink-0 items-center gap-2 border-b border-border px-2 py-1">
      <span className="font-mono text-[10px] uppercase tracking-widest text-muted-foreground">
        {title}
      </span>
      {right ? (
        <span className="ml-auto whitespace-nowrap font-mono text-[9.5px] text-muted-foreground">
          {right}
        </span>
      ) : null}
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

const noop = () => {};

const SOURCE = `@actor.method(takes=Target, emits=Probe)
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
 * What collapsing the Workflows list leaves behind, and what the editor did with the 264 pixels.
 *
 * THE BADGE IS THE WHOLE ARGUMENT FOR THE RAIL. The question a collapsed panel has to answer is
 * not "is something hidden here" but "is what is hidden worth the width" — so the rail carries the
 * panel's own name and its count, and clicking it brings the panel back at the width it had.
 */
export function WhatACollapsedListLeaves() {
  return (
    <Frame>
      <Shell>
        <SideRail label="Workflows" badge={7} onExpand={noop} testid="workflows-rail" />
        <Main>
          <Head title="probe.py" right="48 lines · Python · UTF-8" />
          <Body lines={SOURCE} />
        </Main>
      </Shell>
    </Frame>
  );
}

/**
 * A rail with no count, which is how `ScratchInspector` mounts it: the inspector holds ONE node's
 * reading, so a number beside its name would be `1` on every screen it ever appears on. The badge
 * is optional for exactly this — a count nobody can act on is noise, and the name alone still says
 * what comes back.
 */
export function NoCountWorthShowing() {
  return (
    <Frame>
      <Shell>
        <SideRail label="Inspector" onExpand={noop} testid="scratch-inspector-rail" />
        <Main>
          <Head title="Scratch" right="6 nodes · 5 edges" />
          <Body
            lines={`targets.seed ──▶ probe.head ──▶ probes.head
                          └─▶ nscheck.lame
probes.head  ──▶ report.render

saved 12:04 · kontra scratch open sweep-aug
probe.head   takes Target, emits Probe
nscheck.lame takes Probe,  emits Finding
report.render reads probes.head, findings.lame`}
          />
        </Main>
      </Shell>
    </Frame>
  );
}

/**
 * Collapsed on the RIGHT edge, where a flipped inspector leaves it. The rail is the same element
 * on either side — the page puts it in the flex order the dock's side asks for — so a reader who
 * learned it once on the left finds the same 28-pixel strip, the same vertical name, the same
 * click to bring the panel back.
 */
export function CollapsedOnTheRight() {
  return (
    <Frame>
      <Shell>
        <Main>
          <Head title="Machines" right="6 terminals · fleet sweep-aug" />
          <Body
            lines={`kontra-probe-1   10.124.0.4   probe@0.2.0    live
kontra-probe-2   10.124.0.5   probe@0.2.0    live
kontra-probe-3   10.124.0.8   probe@0.2.0    live
kontra-subf-1    10.124.0.6   subfinder      live
kontra-subf-2    10.124.0.7   subfinder      no poller
kontra-host      10.124.0.2   —              controller

fleet sweep-aug · 6 terminals · 1 needs somebody`}
          />
        </Main>
        <SideRail label="Inspector" badge="probe.head" onExpand={noop} testid="scratch-inspector-rail" />
      </Shell>
    </Frame>
  );
}

/**
 * Every rail this app can leave, side by side — the reason the component is one component. A
 * collapsed panel on the Workflows page, the Scratch canvas and the Monitor used to be three
 * different closed buttons; they are one strip now, so an operator who folds a panel away on one
 * surface already knows what it looks like on the next.
 *
 * It also shows the constraint: the name reads vertically in 28 pixels, so it is the PANEL's short
 * name — `Machines`, not "the Machines tree".
 */
export function EveryRailOnTheApp() {
  return (
    <Frame>
      <Shell>
        <SideRail label="Workflows" badge={7} onExpand={noop} testid="workflows-rail" />
        <SideRail label="Machines" badge={12} onExpand={noop} testid="monitor-tree-rail" />
        <SideRail label="Inspector" onExpand={noop} testid="scratch-inspector-rail" />
        <Main>
          <Head title="Everything is folded away" right="the page has the whole width" />
          <Body
            lines={`run  probe-sweep · 34 batches · 12 completed · 1 running
dataset probes.head — open, 2,880 rows

12:04:31.108  ActivityTaskScheduled  probe.head  batch 12/34
12:04:31.402  ActivityTaskStarted    probe.head  10.124.0.4
12:04:33.977  ActivityTaskCompleted  probe.head  240 rows
12:04:34.010  ActivityTaskScheduled  probe.head  batch 13/34
12:04:34.288  ActivityTaskStarted    probe.head  10.124.0.5`}
          />
        </Main>
      </Shell>
    </Frame>
  );
}
