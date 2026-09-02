import { HealthChips } from '@kontra/frontend';

function Frame({ children }: { children: React.ReactNode }) {
  return (
    <div className="dark">
      <div className="bg-background text-foreground rounded-lg p-4">{children}</div>
    </div>
  );
}

/** A worker answering on every channel. */
export function AllHealthy() {
  return (
    <Frame>
      <HealthChips
        health={{ reachable: 'ok', session: 'present', poller: 'live', loads: 'ok' }}
        terminalId="subfinder-01"
      />
    </Frame>
  );
}

/**
 * The case the four chips exist for: the machine answers and tmux is up, but nothing
 * is polling the queue and loads are failing. A single "health" light would read green.
 */
export function PollerDown() {
  return (
    <Frame>
      <HealthChips
        health={{
          reachable: 'ok',
          session: 'present',
          poller: 'none',
          loads: 'failing',
          detail:
            'No poller on task queue kontra-subfinder for 4m12s · 81 of 82 resource loads failed on 10.124.0.7',
        }}
        terminalId="subfinder-02"
      />
    </Frame>
  );
}

/**
 * `unknown` is not `bad`. A dashed border and no fill say "nobody looked" — the
 * distinction a two-state chip destroys by folding unmeasured into healthy.
 */
export function NotMeasured() {
  return (
    <Frame>
      <HealthChips
        health={{
          reachable: 'unknown',
          session: 'unknown',
          poller: 'unknown',
          loads: 'unknown',
          detail: 'No heartbeat recorded since the worker was provisioned.',
        }}
        terminalId="crawler-07"
      />
    </Frame>
  );
}

/** Unreachable host: every downstream signal degrades, and each keeps its own sentence. */
export function HostUnreachable() {
  return (
    <Frame>
      <HealthChips
        health={{
          reachable: 'fail',
          session: 'no-tmux',
          poller: 'none',
          loads: 'unknown',
          detail:
            'SSH to 10.124.0.11 timed out after 10s · tmux is not installed on the target · No poller on task queue kontra-httpx',
        }}
        terminalId="httpx-03"
      />
    </Frame>
  );
}

/** Stacked, the way a wall of tiles is actually skimmed. */
export function OnAWall() {
  const rows = [
    { id: 'subfinder-01', h: { reachable: 'ok', session: 'present', poller: 'live', loads: 'ok' } },
    { id: 'subfinder-02', h: { reachable: 'ok', session: 'present', poller: 'none', loads: 'failing' } },
    { id: 'crawler-07', h: { reachable: 'unknown', session: 'unknown', poller: 'unknown', loads: 'unknown' } },
  ] as const;
  return (
    <Frame>
      <div className="flex flex-col gap-3">
        {rows.map((r) => (
          <div key={r.id} className="rounded-md border border-border bg-card p-2">
            <div className="mb-1.5 font-mono text-xs text-muted-foreground">{r.id}</div>
            <HealthChips health={r.h} terminalId={r.id} />
          </div>
        ))}
      </div>
    </Frame>
  );
}
