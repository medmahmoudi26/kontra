import {
  Badge,
  Button,
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
  SideNav,
  datasetBadge,
  useAppStore,
} from '@kontra/frontend';

function Frame({ children, width }: { children: React.ReactNode; width?: string }) {
  return (
    <div className="dark">
      <div
        className="bg-background text-foreground rounded-lg p-4"
        style={width ? { width } : undefined}
      >
        {children}
      </div>
    </div>
  );
}

/**
 * `/api/datasets` returns one row per `version=…/dt=…` PARTITION, so a naive count says how many
 * dispatches there were. Eleven Datasets across fifty-four partitions is the exact shape that made
 * the rail print `54` over a page listing `11` names — the rail's number has to be the inventory of
 * the surface it labels.
 */
function partitions(): Array<Record<string, unknown>> {
  const shape: Array<[string, 'output' | 'standalone', number]> = [
    ['subfinder', 'output', 9],
    ['httpx', 'output', 8],
    ['crawler', 'output', 7],
    ['probe', 'output', 6],
    ['parser', 'output', 5],
    ['nscheck', 'output', 4],
    ['smuggler', 'output', 4],
    ['dnsfacts', 'output', 4],
    ['scope-apexes', 'standalone', 3],
    ['paid-programs', 'standalone', 2],
    ['seeds-aug', 'standalone', 2],
  ];
  const rows: Array<Record<string, unknown>> = [];
  for (const [name, kind, count] of shape) {
    for (let i = 0; i < count; i += 1) {
      rows.push({
        kind,
        name,
        version: '0.3.1',
        dt: `2026-08-1${(i % 5) + 1}T0${i % 6}-15-00`,
        rows: 37_412 - i * 811,
        bytes: 4_180_224 - i * 91_000,
        state: i === 0 ? 'open' : 'sealed',
      });
    }
  }
  return rows;
}

/** Twelve caller workflows Temporal knows about; three of them still open. Only the count and the
 *  `running` status are read by the rail — `in flight` is the number a page that is NOT the Runs
 *  page has no other way to ask for. */
function runs(): Array<Record<string, unknown>> {
  const types = ['ApexSweep', 'SubdomainSweep', 'ProbeSweep', 'CrawlCampaign'];
  return Array.from({ length: 12 }, (_, i) => ({
    runId: `sweep-aug-${String(i + 1).padStart(3, '0')}`,
    type: types[i % types.length],
    status: i < 3 ? 'running' : 'completed',
    tenant: 'default',
    startedAt: 1_755_400_000_000 + i * 600_000,
    closedAt: i < 3 ? 0 : 1_755_400_900_000 + i * 600_000,
    dispatches: 10 + i,
  }));
}

/**
 * Rows landing in the lake per second, between two catalog samples — bursty because a Batch
 * committing is a step, not a curve. Seeded here so the rail's footer is drawn from a series the
 * way the running product draws it; nothing in this file synthesises a shape for the line.
 */
const FLEET_SERIES = [
  0, 0, 412, 1_180, 1_640, 1_602, 980, 240, 0, 0, 760, 1_744, 1_810, 1_390, 1_402, 1_388, 620, 90,
  0, 480, 1_240, 1_704, 1_666, 1_512,
];

// ONE SEED, AT MODULE SCOPE. `useAppStore` is a module singleton: two cells cannot hold different
// state, and the product renders every cell of a card on one page, so a per-cell seed would be
// last-write-wins. Everything worth saying about this rail is therefore folded into one fleet:
// mid-campaign, twenty-four Machines, two of them attached, and one count that is not known yet.
useAppStore.setState({
  view: 'datasets',
  theme: 'dark',
  workflowCount: 7,
  // `null`, deliberately: the Scratch surface publishes this when it first reads the store, and a
  // rail printing `0` before then would state something false about sketches it has not counted.
  scratchCount: null,
  catalog: Array.from({ length: 9 }, (_, i) => ({ name: `actor-${i}` })),
  runs: runs(),
  datasets: partitions(),
  wall: { panes: 24, live: 2 },
  fleetSeries: FLEET_SERIES,
  unitsPerSec: FLEET_SERIES[FLEET_SERIES.length - 1],
} as never);

/**
 * The rail mid-campaign, which is the only state that shows what it is for.
 *
 * THE COUNT BESIDE EACH ITEM IS THE INVENTORY OF THAT SURFACE, so "how many workflows / actors /
 * panes / datasets" is answerable without visiting all four — and `Scratch` deliberately prints
 * NOTHING rather than `0`, because that surface has not been opened and counted yet. "No sketches"
 * is a real and actionable state that must not be shown to a session that simply has not looked.
 *
 * THE FOOTER IS THE ONE THING STILL HAPPENING WHILE YOU LOOK ELSEWHERE. `units/s` and its line are
 * measured between two catalog samples; `live panes` is how many Terminals hold a real PTY attach —
 * an sshd session, a PTY and a per-viewer tmux session on an `s-1vcpu-2gb` Machine each — and it
 * goes green only when it is non-zero, because a wall of snapshots is the resting state.
 */
export function AtWork() {
  return (
    <Frame width="272px">
      <div className="flex" style={{ height: '620px' }}>
        <SideNav />
      </div>
    </Frame>
  );
}

/**
 * A RAIL RATHER THAN A TOP BAR, and it is a measurement decision rather than a taste one: most of
 * these surfaces are full-height columns of their own, so a horizontal bar spent a whole row of a
 * 900-pixel screen to say six words and started every page 40 px lower. Vertically the same words
 * cost width nothing else wanted — and the counters get a home that is not somebody's page.
 *
 * This is the composition that shows it: the rail against a surface, holding its 240 px and its
 * `border-r` while the page beside it scrolls its own content.
 */
export function InTheShell() {
  return (
    <Frame>
      <div className="flex overflow-hidden rounded border border-border" style={{ height: '620px' }}>
        <SideNav />
        <div className="flex min-w-0 flex-1 flex-col gap-3 p-4">
          <div className="flex items-baseline justify-between gap-3">
            <span className="text-sm font-semibold">Datasets</span>
            <span className="font-mono text-[11px] text-muted-foreground tabular-nums">
              11 datasets · 54 partitions
            </span>
          </div>

          <Card>
            <CardHeader>
              <div className="flex items-center justify-between gap-3">
                <CardTitle className="font-mono">subdomains.subfinder</CardTitle>
                <Badge
                  variant="outline"
                  className={datasetBadge('sealed').className}
                  title={datasetBadge('sealed').title}
                >
                  {datasetBadge('sealed').label}
                </Badge>
              </div>
              <CardDescription>37,412 rows · 9 partitions · written 6 minutes ago</CardDescription>
            </CardHeader>
            <CardContent className="flex gap-2">
              <Button size="sm">Query</Button>
              <Button size="sm" variant="outline">
                Copy name
              </Button>
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <div className="flex items-center justify-between gap-3">
                <CardTitle className="font-mono">responses.httpx</CardTitle>
                <Badge
                  variant="outline"
                  className={datasetBadge('open').className}
                  title={datasetBadge('open').title}
                >
                  {datasetBadge('open').label}
                </Badge>
              </div>
              <CardDescription>
                12,880 rows · 8 partitions · a Run is still appending to this
              </CardDescription>
            </CardHeader>
            <CardContent className="flex gap-2">
              <Button size="sm">Query</Button>
              <Button size="sm" variant="outline">
                Copy name
              </Button>
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <div className="flex items-center justify-between gap-3">
                <CardTitle className="font-mono">scope-apexes</CardTitle>
                <Badge
                  variant="outline"
                  className={datasetBadge('none').className}
                  title={datasetBadge('none').title}
                >
                  {datasetBadge('none').label}
                </Badge>
              </div>
              <CardDescription>
                119 rows · 3 partitions · an operator-loaded list, so no writer recorded one
              </CardDescription>
            </CardHeader>
            <CardContent className="flex gap-2">
              <Button size="sm">Query</Button>
              <Button size="sm" variant="outline">
                Copy name
              </Button>
            </CardContent>
          </Card>
        </div>
      </div>
    </Frame>
  );
}
