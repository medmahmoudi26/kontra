import { Button } from '@kontra/frontend';

function Frame({ children }: { children: React.ReactNode }) {
  return (
    <div className="dark">
      <div className="bg-background text-foreground rounded-lg p-4">{children}</div>
    </div>
  );
}

/** Every declared variant, labelled with an action the control plane actually offers. */
export function Variants() {
  return (
    <Frame>
      <div className="flex flex-wrap items-center gap-2">
        <Button>Dispatch</Button>
        <Button variant="secondary">Refresh</Button>
        <Button variant="outline">Copy attach command</Button>
        <Button variant="ghost">Details</Button>
        <Button variant="destructive">Abandon Dataset</Button>
        <Button variant="link">View run</Button>
      </div>
    </Frame>
  );
}

/** The four sizes. `icon` is square and sized for a tile header. */
export function Sizes() {
  return (
    <Frame>
      <div className="flex flex-wrap items-center gap-2">
        <Button size="sm">sm</Button>
        <Button size="default">default</Button>
        <Button size="lg">lg</Button>
        <Button size="icon" aria-label="Refresh">
          ⟳
        </Button>
      </div>
    </Frame>
  );
}

/** Disabled: converging is a CLI operation, so the UI offers it read-only. */
export function Disabled() {
  return (
    <Frame>
      <div className="flex flex-wrap items-center gap-2">
        <Button disabled>Converge</Button>
        <Button variant="outline" disabled>
          Seal Dataset
        </Button>
        <Button variant="destructive" disabled>
          Abandon
        </Button>
      </div>
    </Frame>
  );
}

/** In place: the action cluster on a tile header. */
export function InAToolbar() {
  return (
    <Frame>
      <div className="flex items-center justify-between gap-4 rounded-md border border-border bg-card px-3 py-2">
        <span className="font-mono text-sm">kontra-subfinder:0</span>
        <div className="flex items-center gap-1.5">
          <Button variant="ghost" size="sm">
            Attach
          </Button>
          <Button variant="outline" size="sm">
            Copy
          </Button>
          <Button size="sm">Follow</Button>
        </div>
      </div>
    </Frame>
  );
}
