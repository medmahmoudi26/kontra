import { SideResizer } from '@kontra/frontend';

function Frame({ children }: { children: React.ReactNode }) {
  return (
    <div className="dark">
      <div className="rounded-lg bg-background p-4 text-foreground">{children}</div>
    </div>
  );
}

/**
 * The two-column shell a side handle lives in. A sidecar handle is six pixels wide and is never
 * drawn alone in the product: the panel on one side and the page on the other are what make it
 * legible, and which side the panel is on is the whole of its behaviour.
 */
function Shell({ height = 232, children }: { height?: number; children: React.ReactNode }) {
  return (
    <div
      className="flex overflow-hidden rounded-md border border-border bg-card"
      style={{ height }}
    >
      {children}
    </div>
  );
}

/**
 * The sidecar. Its width is a NUMBER the handle moves — `flex: 0 0 <width>px` — and it sits on
 * `bg-background` inside a `bg-card` shell, which is what keeps the handle readable: a header
 * filled with `bg-muted` directly against 6px of `bg-border/40` merges into one strip.
 */
function Side({ width, children }: { width: number; children: React.ReactNode }) {
  return (
    <div
      className="flex min-w-0 flex-col overflow-hidden bg-background"
      style={{ flex: `0 0 ${width}px`, width }}
    >
      {children}
    </div>
  );
}

/** Everything the sidecar takes its width FROM. Dragging the handle spends this. */
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

/** One row of the Workflows list: the name, and under it the path the registration recorded. */
function ListRow({ name, path }: { name: string; path: string }) {
  return (
    <div className="rounded px-2 py-1.5">
      <div className="font-mono text-[11.5px]">{name}</div>
      <p className="m-0 mt-0.5 break-all font-mono text-[9.5px] leading-snug text-muted-foreground">
        {path}
      </p>
    </div>
  );
}

function WorkflowList() {
  return (
    <div className="flex min-h-0 flex-1 flex-col gap-0.5 overflow-hidden p-1.5">
      <ListRow name="nscheck" path="/root/kontra-local/.kontra/workflows/nscheck" />
      <ListRow name="probe-sweep" path="/srv/checkout/examples/python/probe" />
      <ListRow name="subfinder-scan" path="~/.kontra/workflows/subfinder-scan" />
      <ListRow name="report-render" path="~/.kontra/workflows/report-render" />
    </div>
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

const INSPECTOR = `probe.head
takes   Target { url: str, timeout: float }
emits   Probe  { status: int, headers: dict }
reads   targets.seed — 2,880 rows
writes  probes.head — open
served  10.124.0.4, 10.124.0.5
queue   kontra-probe · 2 pollers
digest  sha256:9f2c41b7d0ae5c38e1b6`;

/**
 * The handle on a LEFT-docked sidecar, which is where the Workflows list sits: the panel first,
 * the handle on its right edge, the page after it. Dragging RIGHT widens the panel here — the
 * side is a prop and not an inference precisely because the same gesture on a right-docked panel
 * has to narrow it, and a panel that runs away from the cursor is obvious in use and invisible in
 * review.
 *
 * 264px is the width `WorkflowsPage` starts at — the number that used to be `w-[264px]` in the
 * markup, and is now only where an operator's own width begins.
 */
export function DockedLeft() {
  return (
    <Frame>
      <Shell>
        <Side width={264}>
          <Head title="Workflows" right="4 workflows" />
          <WorkflowList />
        </Side>

        <SideResizer
          width={264}
          onWidth={noop}
          side="left"
          label="the workflow list"
          testid="workflows-resizer"
        />

        <Main>
          <Head title="probe.py" right="48 lines · Python · UTF-8" />
          <Body lines={SOURCE} />
        </Main>
      </Shell>
    </Frame>
  );
}

/**
 * The same handle on a RIGHT-docked sidecar — the Scratch inspector after an operator has flipped
 * it across the page. The handle is now on the panel's LEFT edge, and the pointer arithmetic is
 * mirrored with it: `dragWidth` signs the delta by the side, so moving left widens this one.
 *
 * The panel keeps the width it had through the flip; only the edge changes.
 */
export function DockedRight() {
  return (
    <Frame>
      <Shell>
        <Main>
          <Head title="Scratch" right="probe.head → probes.head" />
          <Body
            lines={`nodes 6 · edges 5 · saved 12:04

targets.seed ──▶ probe.head ──▶ probes.head
                          └─▶ nscheck.lame
probes.head  ──▶ report.render

kontra scratch open sweep-aug
run probe-sweep · 34 batches · 12 completed`}
          />
        </Main>

        <SideResizer
          width={304}
          onWidth={noop}
          side="right"
          label="the inspector"
          testid="scratch-inspector-resizer"
        />

        <Side width={304}>
          <Head title="Inspector" right="method" />
          <Body lines={INSPECTOR} />
        </Side>
      </Shell>
    </Frame>
  );
}

/**
 * Dragged all the way in, stopped at `SIDE_BOUNDS.min` (180px). The clamp is the point: a panel
 * dragged to nothing is one an operator has to know to drag back from an invisible edge, and
 * collapsing — which leaves a rail that says what it is — is the other control for that.
 *
 * This is also where the handle's keyboard half matters. It is a real `role="separator"` with
 * `aria-valuenow=180`, `aria-valuemin=180`, `aria-valuemax=640`, so a screen reader says it is
 * already at the floor; ← is a no-op and → widens by `SIDE_STEP` (24px) a press. The focus ring
 * and the grip only appear once it is focused or hovered, which no static capture can show.
 */
export function AtItsNarrowest() {
  return (
    <Frame>
      {/* Taller than the other cards on purpose: at 180px every path wraps to two lines, so the
          same four workflows need half again the height to be listed without one being cut off. */}
      <Shell height={276}>
        <Side width={180}>
          <Head title="Workflows" right="180px · min" />
          <WorkflowList />
        </Side>

        <SideResizer
          width={180}
          onWidth={noop}
          side="left"
          label="the workflow list"
          testid="workflows-resizer"
        />

        <Main>
          <Head title="probe.py" right="48 lines · Python" />
          <Body lines={SOURCE} />
        </Main>
      </Shell>
    </Frame>
  );
}

/**
 * The width the handle exists to buy. At 180 a registered path wraps to four lines and no amount
 * of reading it does anything about that; at 420 each row is one line and the tail of the path —
 * the half that tells two checkouts of one workflow apart — is the part on screen.
 *
 * The editor pays for it, which is the trade only the operator can make: they know whether they
 * are reading paths or reading code.
 */
export function WideEnoughForAPath() {
  return (
    <Frame>
      <Shell>
        <Side width={420}>
          <Head title="Workflows" right="420px · 4 workflows" />
          <WorkflowList />
        </Side>

        <SideResizer
          width={420}
          onWidth={noop}
          side="left"
          label="the workflow list"
          testid="workflows-resizer"
        />

        <Main>
          <Head title="probe.py" />
          <Body
            lines={`@actor.method(
    takes=Target,
    emits=Probe,
)
async def head(
    self,
    batch,
):
    ...

# 420px leaves this
# much of the editor`}
          />
        </Main>
      </Shell>
    </Frame>
  );
}
