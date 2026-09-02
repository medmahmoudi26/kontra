import { Badge, datasetBadge, DATASET_STATES } from '@kontra/frontend';

/** Dark-first: the control plane is read on a dark ground, so every card renders there.
 *  `dark` must be an ANCESTOR — the `dark:` variant compiles to `&:is(.dark *)`. */
function Frame({ children }: { children: React.ReactNode }) {
  return (
    <div className="dark">
      <div className="bg-background text-foreground rounded-lg p-4">{children}</div>
    </div>
  );
}

/** Every variant the component actually declares, labelled with the thing it marks. */
export function Variants() {
  return (
    <Frame>
      <div className="flex flex-wrap items-center gap-2">
        <Badge>queued</Badge>
        <Badge variant="secondary">v0.3.1</Badge>
        <Badge variant="success">sealed</Badge>
        <Badge variant="destructive">4 dropped</Badge>
        <Badge variant="outline">operator-loaded</Badge>
      </div>
    </Frame>
  );
}

/** A Run's status, the way the toolbar reads it. */
export function RunStatus() {
  return (
    <Frame>
      <div className="flex flex-col gap-3">
        {[
          { label: 'running', variant: 'default' as const, run: 'run_01J8XW4K2QH' },
          { label: 'completed', variant: 'success' as const, run: 'run_01J8XV9B7TM' },
          { label: 'failed', variant: 'destructive' as const, run: 'run_01J8XT2P5RD' },
        ].map((r) => (
          <div key={r.run} className="flex items-center gap-3">
            <Badge variant={r.variant}>{r.label}</Badge>
            <span className="font-mono text-xs text-muted-foreground">{r.run}</span>
          </div>
        ))}
      </div>
    </Frame>
  );
}

/**
 * The Dataset lifecycle, drawn from `datasetBadge` — the one place that mapping lives.
 * Three states, three treatments, plus the absence of one. Never collapsed into two:
 * an `open` Dataset drawn as `sealed` reads as a finished result while its producer
 * may still be writing — or may have died.
 */
export function DatasetLifecycle() {
  const states = [...DATASET_STATES, 'none' as const];
  return (
    <Frame>
      <div className="flex flex-col gap-2">
        {states.map((s) => {
          const b = datasetBadge(s);
          return (
            <div key={s} className="flex items-center gap-3">
              <span
                title={b.title}
                className={`inline-flex w-fit shrink-0 items-center rounded-full border px-2 py-0.5 text-xs font-medium ${b.className}`}
              >
                {b.label}
              </span>
              <span className="font-mono text-xs text-muted-foreground">
                subdomains.subfinder
              </span>
            </div>
          );
        })}
      </div>
    </Frame>
  );
}

/** In place: how a badge sits beside a monospace identifier in a catalogue row. */
export function InCatalogueRow() {
  return (
    <Frame>
      <div className="flex items-center justify-between gap-4 rounded-md border border-border bg-card px-3 py-2">
        <div className="flex items-center gap-2">
          <span className="font-mono text-sm">subfinder</span>
          <Badge variant="secondary">v0.3.1</Badge>
        </div>
        <div className="flex items-center gap-2">
          <Badge variant="success">polling</Badge>
          <span className="font-mono text-xs text-muted-foreground">kontra-subfinder</span>
        </div>
      </div>
    </Frame>
  );
}
