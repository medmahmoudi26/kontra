import { PaneResizer } from '@kontra/frontend';

function Frame({ children }: { children: React.ReactNode }) {
  return (
    <div className="dark">
      <div className="rounded-lg bg-background p-4 text-foreground">{children}</div>
    </div>
  );
}

/**
 * The stack a handle lives in. A resizer is never drawn alone in the product: it is the only
 * thing between two panes, so the panes are what make it legible.
 */
function Stack({ children }: { children: React.ReactNode }) {
  return (
    <div className="flex flex-col overflow-hidden rounded-md border border-border bg-card">
      {children}
    </div>
  );
}

/**
 * A pane sits on `bg-background`; the stack behind it is `bg-card`. That is what makes the
 * handle read at rest: it is 6px of `bg-border/40` over the stack, and its GRIP only appears on
 * hover or focus, because a permanent line between every pair of panes is chrome on a page that
 * is mostly content.
 */
function Pane({ height, children }: { height: number; children: React.ReactNode }) {
  return (
    <div className="shrink-0 overflow-hidden bg-background" style={{ height }}>
      {children}
    </div>
  );
}

function PaneHead({ title, right }: { title: string; right?: string }) {
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
        return [await probe(c, t.url) for t in batch]`;

const EVENTS = `12:04:31.108  ActivityTaskScheduled    probe.head      batch 12/34
12:04:31.402  ActivityTaskStarted      probe.head      10.124.0.4
12:04:33.977  ActivityTaskCompleted    probe.head      240 rows
12:04:34.010  ActivityTaskScheduled    probe.head      batch 13/34`;

/**
 * The handle at the editor's BOTTOM edge, which is where the Workflows page and the Actor
 * workbench put it: `grow="down"` because dragging down has to make the editor taller.
 *
 * The height here is explicit so the card renders statically; in the product it comes from
 * `usePaneHeight('actor-workbench-editor', 420)`, which is what makes a drag survive the next
 * mount instead of snapping back to a number chosen once against one screen.
 */
export function UnderTheEditor() {
  return (
    <Frame>
      <Stack>
        <Pane height={96}>
          <PaneHead title="probe.py" right="48 lines · Python · UTF-8" />
          <Body lines={SOURCE} />
        </Pane>

        <PaneResizer
          height={96}
          onHeight={noop}
          grow="down"
          label="the editor and the pane"
          testid="workbench-editor-resizer"
        />

        <Pane height={74}>
          <PaneHead title="serve console" right="probe@0.2.0 · 10.124.0.4" />
          <Body
            lines={`kontra actor serve probe --queue kontra-probe --tmux
worker ready · 2 methods · session kontra-serve-probe`}
          />
        </Pane>
      </Stack>
    </Frame>
  );
}

/**
 * The same handle, the other way up. On the Runs page it sits at the event log's TOP edge, so
 * `grow="up"`: dragging DOWN gives the space back to the stats and the fleet window above.
 * Get the side wrong and the pane runs away from the cursor — which is why the direction is a
 * prop and not an inference.
 */
export function AboveTheEventLog() {
  return (
    <Frame>
      <Stack>
        <Pane height={62}>
          <PaneHead title="run" right="probe-sweep · 34 batches" />
          <Body
            lines={`completed 12 · running 1 · isolated 0 · 10.124.0.4-.7
dataset probes.head — open, 2,880 rows`}
          />
        </Pane>

        <PaneResizer
          height={104}
          onHeight={noop}
          grow="up"
          label="the event log"
          testid="event-log-resizer"
        />

        <Pane height={104}>
          <PaneHead title="Event log" right="live · 1,204 events" />
          <Body lines={EVENTS} />
        </Pane>
      </Stack>
    </Frame>
  );
}

/**
 * One handle under a ROW of two panes. The Workflows page and the workbench both resize the
 * editor and the worker's pane together, because they are one band of the page — a second
 * handle between them would be a second decision nobody arrives wanting to make.
 */
export function UnderTwoPanesAtOnce() {
  return (
    <Frame>
      <Stack>
        <Pane height={96}>
          <div className="flex h-full">
            <div className="min-w-0 flex-1 overflow-hidden">
              <PaneHead title="probe.py" />
              <Body lines={SOURCE} />
            </div>
            <div className="min-w-0 flex-1 overflow-hidden border-l border-border">
              <PaneHead title="worker" />
              <Body
                lines={`kontra-serve-probe:0
[probe] listening on kontra-probe
[probe] head: 240 rows in 2.6s`}
              />
            </div>
          </div>
        </Pane>

        <PaneResizer
          height={96}
          onHeight={noop}
          grow="down"
          label="the editor and the pane"
          testid="workflow-editor-resizer"
        />

        <Pane height={62}>
          <PaneHead title="contract" right="Target → Probe" />
          <Body lines={`kontra workflow start ProbeSweep --queue kontra-probe`} />
        </Pane>
      </Stack>
    </Frame>
  );
}

/**
 * A pane dragged all the way down, stopped at `PANE_BOUNDS.min` (120px). The clamp is the
 * point: a pane dragged to zero is one an operator has to KNOW to drag back, and the handle
 * reports where it stopped — `aria-valuenow=120`, `aria-valuemin=120` — so the keyboard resize
 * (↑ ↓, 24px a press) is not a control that silently does nothing.
 */
export function StoppedAtItsFloor() {
  return (
    <Frame>
      <Stack>
        <Pane height={120}>
          <PaneHead title="output" right="min height · 120px" />
          <Body
            lines={`subfinder@0.3.1 — 10.124.0.6
api.example.com
cdn.example.com
mail.example.com
vpn.example.com`}
          />
        </Pane>

        <PaneResizer
          height={120}
          onHeight={noop}
          grow="down"
          label="the output pane"
          testid="output-resizer"
        />

        <Pane height={74}>
          <PaneHead title="serve console" right="kontra-subfinder" />
          <Body
            lines={`worker ready · 1 method
[subfinder] enumerate: 1,412 hosts in 41s`}
          />
        </Pane>
      </Stack>
    </Frame>
  );
}
