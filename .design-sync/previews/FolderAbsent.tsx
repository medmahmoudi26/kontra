import { FolderAbsent, FolderActions } from '@kontra/frontend';

function Frame({ children }: { children: React.ReactNode }) {
  return (
    <div className="dark">
      <div className="rounded-lg bg-background p-4 text-foreground">{children}</div>
    </div>
  );
}

const noop = () => {};

/** A registered folder, as `GET /api/sources/:kind` answers it. */
function folder(over: Record<string, unknown> = {}) {
  return {
    id: 'workflow:nscheck:1f3k',
    kind: 'workflow',
    name: 'nscheck',
    path: '/root/kontra-local/.kontra/workflows/nscheck',
    version: '',
    description: 'Ask every nameserver for the zone it claims to serve, and keep the ones answering for a zone they are not authoritative for.',
    registeredAt: 1_752_000_000_000,
    ...over,
  };
}

/** The registration whose directory went with a `git checkout`. */
const GONE = folder({
  id: 'workflow:probe-sweep:4a70',
  name: 'probe-sweep',
  path: '/root/kontra-local/.claude/worktrees/desync/.kontra/workflows/probe-sweep',
  description: 'Head every target in the seed dataset and keep the response headers.',
  absent: true,
});

/** Its neighbour: one `git worktree remove` takes every registration made inside it. */
const GONE_TOO = folder({
  id: 'workflow:desync-probe:7c19',
  name: 'desync-probe',
  path: '/root/kontra-local/.claude/worktrees/desync/.kontra/workflows/desync-probe',
  description: 'Send the CL.0 and charset variants at every origin that answered a HEAD.',
  absent: true,
});

const PROBE = folder({
  id: 'actor:probe:9c21',
  kind: 'actor',
  name: 'probe',
  path: '/srv/checkout/examples/python/probe',
  version: '0.2.0',
  description: 'GET each target and keep the status line and the response headers.',
});

/**
 * The shelf a folder row is drawn on. The header is the SURFACE's own — the folders stopped being a
 * list of their own, and are lines on the rows the Workflows list and the Actors grid were already
 * drawing.
 */
function Shelf({ title, count, children }: { title: string; count: string; children: React.ReactNode }) {
  return (
    <div className="flex flex-col overflow-hidden rounded-md border border-border bg-card">
      <div className="flex shrink-0 items-baseline gap-2 border-b border-border px-2 py-1.5">
        <span className="text-[9.5px] uppercase tracking-wide text-muted-foreground">{title}</span>
        <span className="font-mono text-[10px] tabular-nums text-muted-foreground">{count}</span>
      </div>
      <div className="flex flex-col gap-1 p-1.5">{children}</div>
    </div>
  );
}

/** One folder row: the name, what it is for, and the path the registration recorded. */
function Row({
  name,
  description,
  path,
  when,
  children,
}: {
  name: string;
  description: string;
  path: string;
  when: string;
  children: React.ReactNode;
}) {
  return (
    <div className="rounded bg-background px-2 py-1.5">
      <div className="flex min-w-0 items-center gap-1.5">
        <span className="min-w-0 flex-1 truncate font-mono text-[12px]">{name}</span>
        <span className="shrink-0 font-mono text-[9.5px] tabular-nums text-muted-foreground">
          {when}
        </span>
      </div>
      <p className="m-0 mt-1 line-clamp-2 text-[10.5px] leading-snug text-muted-foreground">
        {description}
      </p>
      <div className="mt-1.5 flex min-w-0 items-start gap-1">
        <p className="m-0 min-w-0 flex-1 break-all font-mono text-[9.5px] leading-snug text-muted-foreground">
          {path}
        </p>
        {children}
      </div>
    </div>
  );
}

/** The three fields the row draws, off the same object the component is given. */
function fields(source: { name: string; description: string; path: string }) {
  return { name: source.name, description: source.description, path: source.path };
}

/**
 * THE FOLDER IS GONE and the row is still here on purpose. A rename, another checkout, an
 * unmounted volume — the registration is the operator's to keep or forget, so the row stays and
 * says so. Without the mark it reads as a healthy registration right up until `serve` fails with a
 * path nobody expected to be missing.
 *
 * Rose, and a word rather than an icon: `absent` is a fact about the disk, not a severity. Two rows
 * here because that is how it arrives — one `git worktree remove` takes every registration made
 * inside it, and the page has no idea until it asks.
 */
export function TheDirectoryIsGone() {
  return (
    <Frame>
      <Shelf title="Workflows" count="7 workflows · 2 absent">
        <Row {...fields(GONE)} when="registered 12 Jul">
          <FolderAbsent source={GONE} testid="workflow-probe-sweep" />
        </Row>
        <Row {...fields(GONE_TOO)} when="registered 12 Jul">
          <FolderAbsent source={GONE_TOO} testid="workflow-desync-probe" />
        </Row>
      </Shelf>
    </Frame>
  );
}

/**
 * The mark only where it is earned. Two present folders draw NOTHING — the component returns null
 * — and the one whose directory is gone is the only row carrying a badge, which is what makes a
 * shelf of registrations scannable: any mark at all means something needs a person.
 *
 * `absent` is three-valued, and the third value looks like the first two rows: an older
 * orchestrator never sets it, so undefined means "no server ever said this was absent", not "it is
 * there". Drawing nothing is the only honest reading of that.
 */
export function PresentDrawsNothing() {
  return (
    <Frame>
      <Shelf title="Registered folders" count="3 registered · 1 absent">
        <Row {...fields(folder())} when="registered 8 Jul">
          <FolderAbsent source={folder()} testid="workflow-nscheck" />
        </Row>
        <Row {...fields(GONE)} when="registered 12 Jul">
          <FolderAbsent source={GONE} testid="workflow-probe-sweep" />
        </Row>
        <Row {...fields(PROBE)} when="registered 2 Aug">
          <FolderAbsent source={PROBE} testid="actor-probe" />
        </Row>
      </Shelf>
    </Frame>
  );
}

/**
 * The pair as `WorkflowsPage` actually draws it: the state of the folder, then what can be done
 * about it. They are two components and not one because they answer different questions — `absent`
 * says the path is not on this disk any more, `forget` offers to drop the registration — and the
 * row deliberately keeps BOTH: putting the folder back is as valid a fix as forgetting it, and a
 * row that only offered the delete would push every operator towards the destructive one.
 */
export function NextToTheForgetButton() {
  return (
    <Frame>
      <Shelf title="Workflows" count="7 workflows · 1 absent">
        <Row {...fields(GONE)} when="registered 12 Jul">
          <FolderAbsent source={GONE} testid="workflow-probe-sweep" />
          <FolderActions
            source={GONE}
            busy={false}
            testid="workflow-probe-sweep"
            onForget={noop}
          />
        </Row>
        <Row {...fields(folder())} when="registered 8 Jul">
          <FolderAbsent source={folder()} testid="workflow-nscheck" />
          <FolderActions source={folder()} busy={false} testid="workflow-nscheck" onForget={noop} />
        </Row>
      </Shelf>
    </Frame>
  );
}
