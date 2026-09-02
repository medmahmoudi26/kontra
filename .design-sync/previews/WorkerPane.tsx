import { WorkerPane } from '@kontra/frontend';

function Frame({ children }: { children: React.ReactNode }) {
  return (
    <div className="dark">
      <div className="rounded-lg bg-background p-4 text-foreground">{children}</div>
    </div>
  );
}

/**
 * THE INVENTORY, AND NOTHING ELSE — the one thing here that is not the product's.
 *
 * This component resolves a session NAME against `GET /api/panels/terminals` and then streams the
 * pane it found over a WebSocket. A card is captured with no orchestrator and no streamer behind
 * it, so without this every cell would be the same "No pane" rectangle and the resolution rule —
 * the reason the component exists — would never be drawn.
 *
 * So the two panel reads are answered from a fixture, and NOTHING ELSE IS FAKED. The terminals
 * response is a real inventory: four Terminals, three of them answering to the session name
 * `probe-0_2_0`, which is the situation `paneForSession` was written for. The ticket read answers
 * with the exact 503 `control/orchestrator/src/server.ts` sends when `KONTRA_PANEL_TOKEN` is unset — the
 * most common reason a freshly brought-up console cannot stream — so the error strip below carries
 * the product's own sentence rather than a story-server 404.
 *
 * NO BYTES ARE INVENTED. The screen in the cells below is empty because no streamer answered, and
 * that is exactly what a browser draws here. A terminal painted with plausible output would be the
 * one lie this whole surface exists to prevent: the pane is the evidence for what the worker did.
 */
const TERMINALS = [
  {
    // The local worker the button beside this pane started. `local` mode, so this pane holds the
    // ACTUAL `kontra.host` process — not a journal follower.
    id: 'local:host/probe-0_2_0/0',
    machine: 'host',
    host: 'controller',
    publicIp: '',
    tag: 'local',
    fleet: '',
    actor: 'probe',
    version: '0.2.0',
    window: '0',
    health: { reachable: 'ok', session: 'present', poller: 'live', loads: 'ok' },
    lastSnapshotAt: 1_786_400_000_000,
  },
  {
    // Two droplets serving the same Actor. `discovery.ts` names a fleet Machine's session by the
    // same rule, so these answer to `probe-0_2_0` as well — which is why resolution is a function.
    id: 'fleet:kontra-4/probe-0_2_0/0',
    machine: 'kontra-4',
    host: 'kontra-probe-4',
    publicIp: '164.92.71.18',
    tag: 'probe',
    fleet: 'sweep-aug',
    actor: 'probe',
    version: '0.2.0',
    window: '0',
    health: { reachable: 'ok', session: 'present', poller: 'live', loads: 'ok' },
  },
  {
    id: 'fleet:kontra-7/probe-0_2_0/0',
    machine: 'kontra-7',
    host: 'kontra-probe-7',
    publicIp: '164.92.71.22',
    tag: 'probe',
    fleet: 'sweep-aug',
    actor: 'probe',
    version: '0.2.0',
    window: '0',
    health: { reachable: 'ok', session: 'present', poller: 'none', loads: 'failing' },
  },
  {
    // A workflow worker served an hour ago whose pane is gone: the process exited and tmux closed
    // the session with it. The Terminal is still in the inventory until the next discovery sweep.
    id: 'local:host/subfinder_sweep/0',
    machine: 'host',
    host: 'controller',
    publicIp: '',
    tag: 'local',
    fleet: '',
    actor: 'subfinder',
    version: '0.3.1',
    window: '0',
    health: {
      reachable: 'ok',
      session: 'absent',
      poller: 'none',
      loads: 'unknown',
      detail: 'the worker exited and tmux closed its session — Serve starts a new one',
    },
    lastSnapshotAt: 1_786_396_400_000,
  },
];

/** Verbatim from `control/orchestrator/src/server.ts` — what an unconfigured API answers, fail-closed. */
const TICKET_DISABLED =
  '{"error":"disabled: set KONTRA_PANEL_TOKEN on orchestrator-api to mint Dashboard tickets"}';

/**
 * THE TERMINAL PALETTE, PICKED THE WAY AN OPERATOR PICKS IT.
 *
 * `useTerminalStyle` defaults to `follow`, which resolves against the `.dark` class on `<html>` —
 * and a preview puts `.dark` on its own wrapper, not on the document, so the screen would render
 * in the `paper` palette inside a dark card. This writes the same `localStorage` key `StyleControls`
 * writes when somebody chooses Night, which is the product's own mechanism and touches no DOM the
 * rest of the page shares.
 */
try {
  globalThis.localStorage?.setItem(
    'kontra-dashboard-terminal-theme',
    JSON.stringify({ version: 1, palette: 'night', fontSize: 12, sidebar: false })
  );
} catch {
  /* a browser with site data blocked throws on access; the pane still renders, in `follow` */
}

const g = globalThis as unknown as { fetch: typeof fetch };
const realFetch = g.fetch.bind(globalThis);
g.fetch = ((input: RequestInfo | URL, init?: RequestInit) => {
  const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
  if (url.includes('/api/panels/terminals')) {
    return Promise.resolve(
      new Response(JSON.stringify({ terminals: TERMINALS }), {
        status: 200,
        headers: { 'content-type': 'application/json' },
      })
    );
  }
  if (url.includes('/api/panels/ticket')) {
    return Promise.resolve(
      new Response(TICKET_DISABLED, {
        status: 503,
        headers: { 'content-type': 'application/json' },
      })
    );
  }
  return realFetch(input, init);
}) as typeof fetch;

/** The pane is `flex-1` inside a fixed-height row on both surfaces it lives on. */
function Box({ children, width }: { children: React.ReactNode; width?: number }) {
  return (
    <div
      className="flex overflow-hidden rounded-md border border-border bg-card"
      style={{ height: 236, width }}
    >
      {children}
    </div>
  );
}

/**
 * THE STATE MOST OPERATORS MEET FIRST, and the two sentences it must not confuse.
 *
 * A session name that resolves to nothing is not an error — nobody has pressed Serve, or the
 * worker was served before this build of the inventory. So the pane says which SUBJECT it looked
 * for: `subject` is a prop precisely so that a pane beside an Actor's code never says "no pane for
 * this workflow" and send its reader hunting for a workflow they never opened.
 *
 * The third line is the one that took a measurement to write: the streamer rediscovers local
 * sessions about every 30 seconds (`DEFAULT_DISCOVER_MS`), so a worker served a second ago is
 * genuinely not here yet, and half a minute of blank rectangle after pressing Serve reads as a
 * failure instead of as a wait.
 *
 * SKIPPED, because it cannot be captured statically: the state BEFORE this one, while the first
 * inventory read is still in flight, which prints `looking for the worker's pane…` under a dashed
 * dot. It lasts one request and there is no per-cell way to hold a fetch open without holding it
 * open for every cell on the product's own grid page.
 */
export function TheEmptyStateNamesItsSubject() {
  return (
    <Frame>
      <div className="flex gap-2">
        <Box>
          <WorkerPane session={null} derived="nscheck_zones" />
        </Box>
        <Box>
          <WorkerPane session={null} derived="crawler-0_1_0" subject="actor" />
        </Box>
      </div>
    </Frame>
  );
}

/**
 * THREE TERMINALS ANSWER TO `probe-0_2_0` AND THE LOCAL ONE WINS — the whole reason resolution is
 * `paneForSession` and not a `find`.
 *
 * Two droplets serve the same Actor at the same version, so the inventory lists their sessions
 * under the same name; taking the first match would put a droplet's screen beside a workbench
 * whose own console prints `tmux attach -t probe-0_2_0`, a command that on this host reaches a
 * different worker entirely. The header names which one was chosen — `controller · probe-0_2_0` —
 * so the pane and the command below it are provably the same process.
 *
 * `Monitor` is the only control, and it is a LINK, not a play button. This pane is a snapshot by
 * construction (ADR 0020): a live attach costs an sshd session, a PTY and a per-viewer tmux
 * session on the Machine, and the Monitor is the surface that holds a budget for spending that.
 *
 * The screen is empty and the strip beneath it says why, in the API's own words: this console has
 * no `KONTRA_PANEL_TOKEN`, so no ticket can be minted and nothing can be streamed. A pane that
 * failed to connect must never be a quiet black rectangle — that is indistinguishable from a
 * worker that has printed nothing, which is the one reading this surface exists to rule out.
 */
export function TheLocalWorkerWins() {
  const inventory = [
    { id: 'local:host/probe-0_2_0/0', who: 'controller', chosen: true },
    { id: 'fleet:kontra-4/probe-0_2_0/0', who: 'kontra-probe-4 · 10.124.0.4', chosen: false },
    { id: 'fleet:kontra-7/probe-0_2_0/0', who: 'kontra-probe-7 · 10.124.0.7', chosen: false },
  ];
  return (
    <Frame>
      <div className="flex gap-2">
        <Box width={520}>
          <WorkerPane session="probe-0_2_0" derived="probe-0_2_0" subject="actor" />
        </Box>
        <div className="flex min-w-0 flex-1 flex-col gap-1.5">
          <div className="text-[9px] uppercase tracking-wide text-muted-foreground">
            what answered to probe-0_2_0
          </div>
          {inventory.map((t) => (
            <div
              key={t.id}
              className={`rounded-md border px-2 py-1.5 ${
                t.chosen ? 'border-primary bg-primary/10' : 'border-border'
              }`}
            >
              <div className="truncate font-mono text-[10.5px]">{t.id}</div>
              <div className="truncate text-[10px] text-muted-foreground">
                {t.chosen ? `${t.who} — drawn here` : t.who}
              </div>
            </div>
          ))}
        </div>
      </div>
    </Frame>
  );
}

/**
 * THE PANE RESOLVED AND THE SESSION IS GONE — the failure this surface was put beside an editor
 * for, in the shape it actually arrives in.
 *
 * A worker that boots and dies leaves a Terminal in the inventory and no tmux session behind it.
 * Every other status on the page stays green: the file saved, Serve returned successfully, and a
 * Run against this queue would simply wait forever. The tile draws its own words OVER the last
 * screen it painted, because that screen — if there were one — is now stale.
 *
 * Note what the pane does NOT offer here: `TerminalTile`'s converge button. `WorkerPane` passes no
 * `onConverge`, deliberately — converging is a fleet act against a Machine, and the fix for a local
 * worker that died is to read its traceback and press Serve again.
 */
export function NoSessionOnTheMachine() {
  return (
    <Frame>
      <Box>
        <WorkerPane session={null} derived="subfinder_sweep" />
      </Box>
    </Frame>
  );
}

/**
 * WHERE IT LIVES — the right-hand third of the workbench, beside the code it is the evidence for.
 *
 * The proportion is the product's (`flex-[0.95]` against the editor on the Workflows page), and it
 * is the argument for the whole component: a `TabError` at import, a missing package, a queue
 * nobody polls all leave the editor, the status pills and the Run button looking perfectly fine,
 * and the only evidence is in this rectangle. Without it an operator reads "my workflow hangs";
 * with it they read the exception.
 *
 * The editor on the left is a plain stand-in — the real one is CodeMirror, which is a page's
 * dependency and not part of this design system.
 */
export function BesideTheEditor() {
  return (
    <Frame>
      <div
        className="flex overflow-hidden rounded-md border border-border bg-card"
        style={{ height: 236 }}
      >
        <div className="flex min-w-0 flex-1 flex-col">
          <div className="flex h-[31px] shrink-0 items-center gap-2 border-b border-border bg-muted/60 px-2.5 font-mono text-[11px] font-medium">
            <span className="truncate">.kontra/workflows/subfinder_sweep.py</span>
          </div>
          <pre className="m-0 min-h-0 flex-1 overflow-hidden p-2 font-mono text-[10.5px] leading-snug text-muted-foreground">
{`@workflow.defn
class SubfinderSweep:
    @workflow.run
    async def run(self, req: dict) -> dict:
        apexes = catalog.dataset("scope_paid")
        async with catalog.actor(
            "subfinder", "0.3.1"
        ) as sf, out.writer() as w:
            async for b in apexes.batches(200):
                await w.publish(
                    await sf.enumerate(b)
                )`}
          </pre>
        </div>
        <div className="flex-[0.95] border-l border-border">
          <WorkerPane session={null} derived="subfinder_sweep" className="h-full" />
        </div>
      </div>
    </Frame>
  );
}
