import { PaneStack } from '@kontra/frontend';

function Frame({ children }: { children: React.ReactNode }) {
  return (
    <div className="dark">
      <div className="rounded-lg bg-background p-4 text-foreground">{children}</div>
    </div>
  );
}

/**
 * The bounded box a stack lives in. Every unfolded pane is `flex-1` inside it, so the stack needs
 * a height to divide — in the product that height is what `PaneResizer` above it leaves over.
 */
function Shell({ height = 300, children }: { height?: number; children: React.ReactNode }) {
  return (
    <div
      className="flex flex-col overflow-hidden rounded-md border border-border bg-card"
      style={{ height }}
    >
      {children}
    </div>
  );
}

function Body({ lines }: { lines: string }) {
  return (
    <pre className="m-0 min-h-0 flex-1 overflow-hidden bg-background px-2 py-1 font-mono text-[10px] leading-4 text-muted-foreground">
      {lines}
    </pre>
  );
}

/**
 * A per-pane action, the kind that lives at the right of a drag handle. Its group stops
 * pointer-down from reaching the header, so pressing it never picks the pane up — the reason the
 * stack owns that rule rather than each button.
 */
function Action({ children }: { children: React.ReactNode }) {
  return (
    <span className="rounded px-1 text-[9.5px] leading-5 text-muted-foreground hover:bg-accent">
      {children}
    </span>
  );
}

/**
 * A stored order for one stack, written before the card renders.
 *
 * The stack owns its order through `usePaneOrder`, and the states worth a card — an operator's own
 * order, a pane folded away — are reached by dragging and clicking, which a static capture cannot
 * do. Seeding the same key the hook reads is how the card shows where those gestures LAND. Every
 * cell uses its own stack name, so two cards on one page never write over each other's layout.
 */
function remembered(stack: string, order: string[], folded: string[] = []): string {
  try {
    globalThis.localStorage?.setItem(`kontra.stack.${stack}`, JSON.stringify({ order, folded }));
  } catch {
    // Storage can throw on ACCESS, not only on the value. The stack falls back to source order,
    // which is still a card worth looking at.
  }
  return stack;
}

const WORKER = `kontra-serve-probe:0
[probe] worker ready · queue kontra-probe · 2 methods
[probe] head: 240 rows in 2.6s
[probe] head: 240 rows in 2.4s
[probe] head: 240 rows in 2.9s`;

const TRACEBACK = `kontra-serve-probe:0
[probe] loading /srv/checkout/examples/python/probe
Traceback (most recent call last):
  File "probe.py", line 4, in <module>
    import httpx
ModuleNotFoundError: No module named 'httpx'
[probe] worker exited (1)`;

/** Three panes divide a stack's height three ways, so the pane bodies are written to fit it. */
const WORKER_SHORT = `kontra-serve-probe:0
[probe] worker ready · queue kontra-probe
[probe] head: 240 rows in 2.6s
[probe] head: 240 rows in 2.4s`;

const SERVE = `kontra actor serve probe --queue kontra-probe --tmux
session kontra-serve-probe · started 12:04:29
probe@0.2.0 registered · head, fetch`;

const FILES = `actor.json
probe.py
description.md
requirements.txt`;

/**
 * The workbench's own stack, in source order: the worker's pane over the serve console, under an
 * editor the card leaves out. Both are drawn by their header — the WHOLE header is the drag
 * handle, because a six-pixel grip is a target people miss and every tiling editor already taught
 * them to grab the title bar.
 */
export function TheWorkbenchStack() {
  return (
    <Frame>
      <Shell>
        <PaneStack
          stack={remembered('preview-workbench', ['worker', 'serve'])}
          className="min-h-0 flex-1 overflow-hidden"
          panes={[
            { key: 'worker', title: 'worker pane', meta: 'kontra-serve-probe', body: <Body lines={WORKER} /> },
            { key: 'serve', title: 'serve', meta: 'probe@0.2.0', body: <Body lines={SERVE} /> },
          ]}
        />
      </Shell>
    </Frame>
  );
}

/**
 * The same two panes, in the order an operator dragged them into — and the reason the stack exists.
 * Serving a folder for the first time, the console is the thing being read; chasing a worker that
 * booted and died, the pane holding the traceback is. The source order can only be right for one of
 * them, and here the traceback is on top where it is being read.
 *
 * The order is stored BY KEY, so it survives the workbench growing a third pane.
 */
export function ReorderedByHand() {
  return (
    <Frame>
      <Shell>
        <PaneStack
          stack={remembered('preview-workbench-traceback', ['serve', 'worker'])}
          className="min-h-0 flex-1 overflow-hidden"
          panes={[
            { key: 'worker', title: 'worker pane', meta: 'exited (1)', body: <Body lines={TRACEBACK} /> },
            { key: 'serve', title: 'serve', meta: 'probe@0.2.0', body: <Body lines={SERVE} /> },
          ]}
        />
      </Shell>
    </Frame>
  );
}

/**
 * Three panes with the middle one FOLDED — its caret turned, its body gone, its place kept.
 * Folding and moving are two things done for different reasons, so a fold leaves the pane exactly
 * where it was folded: `serve` has nothing left to say once the worker is up, and it is still
 * between the pane and the files where its reader left it.
 */
export function OneFoldedAway() {
  return (
    <Frame>
      <Shell>
        <PaneStack
          stack={remembered('preview-workbench-folded', ['worker', 'serve', 'files'], ['serve'])}
          className="min-h-0 flex-1 overflow-hidden"
          panes={[
            { key: 'worker', title: 'worker pane', meta: 'kontra-serve-probe', body: <Body lines={WORKER} /> },
            { key: 'serve', title: 'serve', meta: 'served 12:04:29', body: <Body lines={SERVE} /> },
            { key: 'files', title: 'files', meta: '4 files', body: <Body lines={FILES} /> },
          ]}
        />
      </Shell>
    </Frame>
  );
}

/**
 * What a header can carry beside the title: a `meta` (a session, a count, a state) and `actions`.
 * The bottom pane is `unfoldable` — a one-line status bar has no folded state worth having — and
 * it keeps the caret's slot empty rather than closing the gap, so the titles down the stack still
 * line up.
 *
 * Pressing an action never picks the pane up: the group stops pointer-down before the drag starts.
 * That, the grab cursor, the drop line and the ↑ ↓ keyboard reorder are the four things about this
 * component no still capture can show.
 */
export function MetaAndActionsInTheHeader() {
  return (
    <Frame>
      <Shell height={324}>
        <PaneStack
          stack={remembered('preview-workbench-actions', ['worker', 'files', 'status'])}
          className="min-h-0 flex-1 overflow-hidden"
          panes={[
            {
              key: 'worker',
              title: 'worker pane',
              meta: 'kontra-serve-probe',
              actions: (
                <>
                  <Action>restart</Action>
                  <Action>attach</Action>
                </>
              ),
              body: <Body lines={WORKER_SHORT} />,
            },
            {
              key: 'files',
              title: 'files',
              meta: '/srv/checkout/examples/python/probe',
              actions: <Action>reload</Action>,
              body: <Body lines={FILES} />,
            },
            {
              key: 'status',
              title: 'queue',
              meta: 'kontra-probe · 2 pollers · 0 backlog',
              unfoldable: true,
              body: (
                <Body
                  lines={`12:04:33.977  ActivityTaskCompleted  probe.head  240 rows  10.124.0.4
12:04:34.010  ActivityTaskScheduled  probe.head  batch 13/34`}
                />
              ),
            },
          ]}
        />
      </Shell>
    </Frame>
  );
}
