import { FolderActions } from '@kontra/frontend';

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

/**
 * A folder nobody registered: it was simply found in the default root. The `at:` id is the whole
 * test — `SourceStore.forget` refuses one — and `registeredAt` is 0 because no registration exists.
 */
const DISCOVERED = folder({
  id: 'at:/home/mo/.kontra/workflows/subfinder-scan',
  name: 'subfinder-scan',
  path: '/home/mo/.kontra/workflows/subfinder-scan',
  description: 'Enumerate subdomains for every apex in scope, one worker per droplet.',
  registeredAt: 0,
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

/**
 * One folder row. THE PATH IS ON THE ROW and it wraps rather than truncates: an operator with
 * three checkouts of one actor is here to find out which one this console serves, and the tail of
 * a path is the half that answers that.
 */
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
 * The affordance a registered folder gets: one ghost button on the row that names it, sized to sit
 * beside a path rather than above it. Forgetting removes the REGISTRATION — the tooltip says the
 * folder on disk is not touched, because "forget" beside a path reads like a delete otherwise.
 */
export function ARegisteredFolder() {
  return (
    <Frame>
      <Shelf title="Registered folders" count="2 registered">
        <Row {...fields(folder())} when="registered 8 Jul">
          <FolderActions source={folder()} busy={false} testid="workflow-nscheck" onForget={noop} />
        </Row>
        <Row {...fields(PROBE)} when="registered 2 Aug">
          <FolderActions source={PROBE} busy={false} testid="actor-probe" onForget={noop} />
        </Row>
      </Shelf>
    </Frame>
  );
}

/**
 * THE RULE THAT COSTS A 400, on one shelf: `nscheck` was registered by hand and can be forgotten;
 * `subfinder-scan` was found in the default root, so there is nothing to remove and it gets the
 * `discovered` badge instead of a button that could only fail.
 *
 * The badge is not a lesser button. It answers the question the missing button raises — why this
 * row is different — and its tooltip says the way out: move the folder to un-list it.
 */
export function DiscoveredHasNoRegistrationToRemove() {
  return (
    <Frame>
      <Shelf title="Workflows" count="7 workflows · 1 discovered">
        <Row {...fields(folder())} when="registered 8 Jul">
          <FolderActions source={folder()} busy={false} testid="workflow-nscheck" onForget={noop} />
        </Row>
        <Row {...fields(DISCOVERED)} when="in the default root">
          <FolderActions
            source={DISCOVERED}
            busy={false}
            testid="workflow-subfinder-scan"
            onForget={noop}
          />
        </Row>
      </Shelf>
    </Frame>
  );
}

/**
 * The same two folders on a surface that only REPORTS — no `onForget`, so the registered row gets
 * nothing at all. That is the third case and it is deliberate: a page that shows where code lives
 * without owning the registration should not grow a destructive control just by drawing a row.
 *
 * The `discovered` badge survives it, because the badge is a FACT about the folder and not an
 * affordance: the row would otherwise read as an ordinary registration on this surface and as an
 * unforgettable one on the next.
 */
export function ASurfaceThatOnlyReports() {
  return (
    <Frame>
      <Shelf title="Where the code lives" count="report only">
        <Row {...fields(folder())} when="registered 8 Jul">
          <FolderActions source={folder()} busy={false} testid="report-nscheck" />
        </Row>
        <Row {...fields(DISCOVERED)} when="in the default root">
          <FolderActions source={DISCOVERED} busy={false} testid="report-subfinder-scan" />
        </Row>
      </Shelf>
    </Frame>
  );
}

/**
 * A forget in flight. THE WHOLE SHELF DISABLES, not the row that was pressed: two forgets against
 * one list is a race the page's own copy of that list cannot represent, so `busy` is the shelf's
 * state and every button on it goes to `…` until the server has answered.
 */
export function WhileAForgetIsInFlight() {
  return (
    <Frame>
      <Shelf title="Registered folders" count="forgetting nscheck">
        <Row {...fields(folder())} when="forgetting…">
          <FolderActions source={folder()} busy testid="workflow-nscheck" onForget={noop} />
        </Row>
        <Row {...fields(PROBE)} when="registered 2 Aug">
          <FolderActions source={PROBE} busy testid="actor-probe" onForget={noop} />
        </Row>
      </Shelf>
    </Frame>
  );
}
