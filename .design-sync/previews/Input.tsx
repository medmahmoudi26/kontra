import { Input, Button } from '@kontra/frontend';

function Frame({ children }: { children: React.ReactNode }) {
  return (
    <div className="dark">
      <div className="bg-background text-foreground rounded-lg p-4">{children}</div>
    </div>
  );
}

/** Empty, filled, and disabled — the three states that render statically. */
export function States() {
  return (
    <Frame>
      <div className="flex max-w-sm flex-col gap-3">
        <Input placeholder="run id" />
        <Input defaultValue="run_01J8XV9B7TM" className="font-mono" />
        <Input defaultValue="kontra-subfinder" disabled className="font-mono" />
      </div>
    </Frame>
  );
}

/** Labelled, the way a filter row is built. */
export function Labelled() {
  return (
    <Frame>
      <div className="flex max-w-sm flex-col gap-3">
        <div className="flex flex-col gap-1">
          <label className="text-xs font-medium text-muted-foreground">Dataset name</label>
          <Input defaultValue="subdomains.subfinder" className="font-mono" />
        </div>
        <div className="flex flex-col gap-1">
          <label className="text-xs font-medium text-muted-foreground">Page size</label>
          <Input type="number" defaultValue={200} />
        </div>
      </div>
    </Frame>
  );
}

/** Beside an action — the filter bar idiom. */
export function WithAction() {
  return (
    <Frame>
      <div className="flex max-w-md items-center gap-2">
        <Input placeholder="Filter actors…" />
        <Button variant="outline">Filter</Button>
      </div>
    </Frame>
  );
}
