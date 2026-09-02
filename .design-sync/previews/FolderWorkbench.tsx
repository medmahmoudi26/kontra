import { useEffect, useRef } from 'react';
import { FolderWorkbench, useAppStore } from '@kontra/frontend';

/* ── the store, seeded once ──────────────────────────────────────────────────────────────────
   The workbench reads `theme` from the store to pick CodeMirror's palette, and the pane beside it
   reads `watchTerminal` to hand a Terminal to the Monitor. The store is a module singleton, so this
   is seeded ONCE at module scope and every cell below shares it — per-cell variation is in props. */
useAppStore.setState({ theme: 'dark', view: 'actors' });

/* The height the editor/pane row was last dragged to (`usePaneHeight` persists it per pane). Seeded
   so a card shows the whole surface — editor, pane, and the serve console under them — rather than
   the 420px default pushing the console off the bottom of the frame. */
try {
  globalThis.localStorage?.setItem('kontra.pane.actor-workbench-editor.height', '240');
} catch {
  /* a browser with site data blocked; the hook falls back to its default */
}

/* ── the folders on the orchestrator's disk ──────────────────────────────────────────────────── */

const PROBE_PY = `"""probe — fetch a page, or just ask what is there. \`fetch\` returns the body, which is
what linkfind parses next; \`head\` reports status and server without downloading.

Two Methods, two SIGNATURES: \`fetch\` takes a URL and emits a body, \`head\` takes a
host and emits a status. That is why types live on the Method (ADR 0023 §9).
"""
import urllib.request
from dataclasses import dataclass

from actorkit import actor


@dataclass
class Target:
    url: str | None = None
    host: str | None = None

    def address(self) -> str:
        if self.url:
            return self.url
        if not self.host:
            raise ValueError("a target needs a url or a host")
        return "http://" + self.host


@dataclass
class Head:
    url: str
    status: int
    server: str


@actor.load
async def open_opener(self):
    self.get = urllib.request.build_opener().open


@actor.method(takes=Target, emits=Head)
async def head(self, batch):
    """GET each target and keep only the status line and the response headers."""
    out = []
    for unit in batch:
        with self.get(unit.address(), timeout=10) as res:
            out.append(Head(url=res.url, status=res.status, server=res.headers.get("server", "")))
    return out
`;

const PROBE_JSON = `{
  "schemaVersion": "kontra.actor.v1",
  "name": "probe",
  "version": "0.2.0",
  "targets": {
    "container": { "memory": "512m", "cpus": 1 },
    "machine": { "size": "s-1vcpu-1gb", "region": "sfo3", "image": "ubuntu-22-04-x64" }
  }
}
`;

const PROBE_MD = `# probe

Fetch a page, or just ask what is there. Stdlib only, so it runs on a bare Machine
with no image of its own.

## Methods

- \`head\` — GET each target, keep the status line and the headers.
- \`fetch\` — the body as well, capped at 512 KB.

\`\`\`python
await probe.head([{"url": "https://example.com"}])
\`\`\`
`;

const NSCHECK_WORKFLOW = `"""nscheck — a whole run, infrastructure included, as one durable program.

For every domain in a list, which of its delegated nameservers fails to answer for
that domain? A nameserver in a zone's NS set that does not serve the zone is a lame
delegation — a resolution fault at best, a takeover at worst.
"""
from kontra import workflow, fleet


@workflow.main
async def nscheck(run, domains: str = "apex.domains"):
    async with fleet.up(actor="nscheck", version="0.1.0", size=4) as f:
        await f.ready()
        out = run.dataset("nscheck.findings")
        async for page in run.pages(domains, batch_size=100):
            servers = await f.delegation(page)
            for chunk in run.repage(servers, batch_size=100):
                await out.write(await f.ask(chunk))
        return out.seal()
`;

const NSCHECK_MD = `# nscheck

Page a Dataset of domains and check every delegation on a fleet the run creates and
destroys inside its own scope.

The findings are the output, **not** the errors: \`ask\` emits a row for every
(domain, nameserver) pair including every failure, because a dropped Unit is absent
from the result Batch — and for this run that would mean discarding exactly what it
went looking for.
`;

const BEACON_PY = `"""beacon — one request per unit, with the timing kept."""
from dataclasses import dataclass

from actorkit import actor


@dataclass
class Ping:
    host: str


@actor.method(takes=Ping, emits=Ping)
async def sweep(self, batch):
    return list(batch)
`;

/** The registered folders these cells open, and what `GET …/files` and `GET …/file` answer for each.
 *  `files` is `null` for the folder whose listing is refused. */
const DISK: Record<
  string,
  { path: string; files: Record<string, string> | null; refusal?: string }
> = {
  'actor:probe:1f3k': {
    path: '/srv/checkout/examples/python/probe',
    files: { 'actor.json': PROBE_JSON, 'actor.py': PROBE_PY, 'description.md': PROBE_MD },
  },
  'workflow:nscheck:aa41': {
    path: '/home/med/.kontra/workflows/nscheck',
    files: { 'description.md': NSCHECK_MD, 'workflow.py': NSCHECK_WORKFLOW },
  },
  'actor:beacon:9d40': {
    path: '/srv/checkout/examples/python/beacon',
    files: { 'actor.json': '{\n  "name": "beacon",\n  "version": "0.1.0"\n}\n', 'actor.py': BEACON_PY },
  },
  'actor:crawl4ai:4e77': {
    path: '/srv/checkout/examples/python/crawl4ai',
    files: null,
    refusal:
      '/srv/checkout/examples/python/crawl4ai is not on this machine any more — put the folder back, or forget the registration',
  },
};

/**
 * The four routes the workbench reads, answered with real-shaped data — and nothing else.
 *
 * The workbench is the half that TALKS TO THE SERVER: it lists the folder on mount, reads each file
 * as it is selected, asks the streamer for the worker's pane, and POSTs the serve. A card has no
 * orchestrator behind it, so without these the cells would all be one error banner. Everything else
 * still goes to the real fetch.
 *
 * `panels/terminals` answers with an EMPTY inventory rather than a fabricated pane: a live pane is
 * xterm over a WebSocket to the streamer, which cannot exist in a static capture, and inventing one
 * would draw a screenful of output no worker produced. Empty is the honest state, and it is the one
 * the workbench is designed to explain.
 */
const realFetch = globalThis.fetch.bind(globalThis);
globalThis.fetch = ((input: RequestInfo | URL, init?: RequestInit) => {
  const raw = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
  const url = decodeURIComponent(raw);
  const json = (body: unknown, status = 200, statusText = 'OK') =>
    Promise.resolve(
      new Response(JSON.stringify(body), {
        status,
        statusText,
        headers: { 'content-type': 'application/json' },
      })
    );

  if (url.includes('/panels/terminals')) return json({ terminals: [] });

  const files = /\/sources\/(?:actor|workflow)\/(.+?)\/files$/.exec(url);
  if (files) {
    const folder = DISK[files[1]];
    if (!folder) return realFetch(input, init);
    if (!folder.files) return json({ error: folder.refusal }, 400, 'Bad Request');
    return json({
      path: folder.path,
      files: Object.entries(folder.files)
        .map(([name, source]) => ({
          name,
          bytes: new TextEncoder().encode(source).length,
          modifiedAt: 1_752_000_000_000,
        }))
        .sort((a, b) => a.name.localeCompare(b.name)),
    });
  }

  const one = /\/sources\/(?:actor|workflow)\/(.+?)\/file(?:\?name=(.+))?$/.exec(url);
  if (one) {
    const folder = DISK[one[1]];
    const name = one[2] ?? '';
    const source = folder?.files?.[name];
    if (source === undefined) return realFetch(input, init);
    return json({ name, source });
  }

  const serve = /\/sources\/actor\/(.+?)\/serve$/.exec(url);
  if (serve) {
    const folder = DISK[serve[1]];
    if (!folder) return realFetch(input, init);
    return json({
      actor: 'probe',
      version: '0.2.0',
      path: folder.path,
      session: 'probe-0_2_0',
      attach: 'tmux attach -t probe-0_2_0',
    });
  }

  return realFetch(input, init);
}) as typeof fetch;

/* ── framing ─────────────────────────────────────────────────────────────────────────────────── */

/**
 * The app shell's box. `FolderWorkbench` is a `<main>` that fills a height-constrained flex column —
 * every pane inside it is `min-h-0`, and the editor/pane row is a pixel height an operator drags. A
 * card that let it size to its content would draw a layout the product never has.
 */
function Shell({ children, height = 632 }: { children: React.ReactNode; height?: number }) {
  return (
    <div className="dark">
      <div
        className="overflow-hidden rounded-lg bg-background p-4 text-foreground"
        style={{ height, display: 'flex' }}
      >
        {children}
      </div>
    </div>
  );
}

/**
 * Click one of the workbench's own affordances once the async render has drawn it.
 *
 * Which file is open, whether a serve has been performed, and whether `description.md` has been
 * started are all internal state reached by clicking — there is no prop for any of them. This drives
 * the REAL controls by their own test ids rather than standing in a lookalike, and polls because the
 * row it clicks does not exist until the folder listing lands.
 *
 * `display: contents` so this wrapper is not a box: the workbench has to stay the flex child of the
 * shell, or its `flex-1` and every `min-h-0` under it stop meaning anything.
 */
function ClickWhenDrawn({ testid, children }: { testid: string; children: React.ReactNode }) {
  const host = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const root = host.current;
    if (!root) return;
    const timer = setInterval(() => {
      const el = root.querySelector<HTMLElement>(`[data-testid="${testid}"]`);
      if (!el) return;
      clearInterval(timer);
      el.click();
    }, 20);
    return () => clearInterval(timer);
  }, [testid]);
  return (
    <div ref={host} style={{ display: 'contents' }}>
      {children}
    </div>
  );
}

const PROBE = {
  id: 'actor:probe:1f3k',
  kind: 'actor' as const,
  name: 'probe',
  path: '/srv/checkout/examples/python/probe',
  version: '0.2.0',
  description: 'Fetch a page, or just ask what is there.',
  registeredAt: 1_752_000_000_000,
};

const NSCHECK = {
  id: 'workflow:nscheck:aa41',
  kind: 'workflow' as const,
  name: 'nscheck',
  path: '/home/med/.kontra/workflows/nscheck',
  version: '',
  description: 'Page a Dataset of domains and check every delegation on a fleet.',
  registeredAt: 1_752_000_000_000,
};

const BEACON = {
  id: 'actor:beacon:9d40',
  kind: 'actor' as const,
  name: 'beacon',
  path: '/srv/checkout/examples/python/beacon',
  version: '0.1.0',
  description: '',
  registeredAt: 1_752_000_000_000,
};

const CRAWL4AI = {
  id: 'actor:crawl4ai:4e77',
  kind: 'actor' as const,
  name: 'crawl4ai',
  path: '/srv/checkout/examples/python/crawl4ai',
  version: '0.3.0',
  description: '',
  registeredAt: 1_752_000_000_000,
};

const noop = () => {};

/**
 * An Actor folder, opened — the whole surface at once, and the reason it is one screen: the files on
 * the left, `actor.py` in the editor (the CODE, never `actor.json`, which is what "the first file"
 * alphabetically would have been), the worker's pane beside it, and the serve console under both.
 *
 * A worker that boots and dies leaves the folder saved, the serve successful and every count on the
 * Actors grid unchanged — the traceback is only ever in that pane, which is why it is here and not
 * somewhere an operator has to know to go.
 */
export function AnActorFolderOpen() {
  return (
    <Shell>
      <FolderWorkbench source={PROBE} onClose={noop} onSaved={noop} />
    </Shell>
  );
}

/**
 * `description.md`, rendered beside itself — and NOT because a preview is nice to have. Only the
 * FIRST PARAGRAPH of this file becomes the description every list on the page shows, and
 * `firstParagraph` skips heading lines: an author who opens with `# probe` and expects that to be
 * the summary has written a description of nothing. Seeing the structure is how they find that out
 * here instead of from a row that says less than they wrote.
 *
 * A WORKFLOW folder, so there is no worker pane and no serve console: a caller's workflow is served
 * from the Workflows page, which owns its queue and its type, and a pane here would be a rectangle
 * that can never fill.
 */
export function DescriptionBesideItself() {
  return (
    <Shell height={470}>
      <ClickWhenDrawn testid="workbench-file-description.md">
        <FolderWorkbench source={NSCHECK} onClose={noop} onSaved={noop} />
      </ClickWhenDrawn>
    </Shell>
  );
}

/**
 * The folder has no `description.md`, so the aside offers to start one — the file this whole surface
 * exists to make somebody write. Pressed here, and every consequence is visible at once: the row
 * says `new` because nothing is on disk, the dot and the header bullet say the buffer is dirty from
 * its first render, and the banner says the one thing that can lose it — leaving the folder, not
 * switching files inside it.
 */
export function StartingTheDescription() {
  return (
    <Shell>
      <ClickWhenDrawn testid="workbench-add-description">
        <FolderWorkbench source={BEACON} onClose={noop} onSaved={noop} />
      </ClickWhenDrawn>
    </Shell>
  );
}

/**
 * Served from here. The console prints the SERVER's own session and attach command back — the proof
 * that the pane above and the terminal an operator opens are one tmux session and not two spellings
 * of one intention — while the pane still says there is nothing to draw, because the streamer
 * rediscovers local sessions about every 30 seconds and a blank rectangle for half a minute reads as
 * a failure rather than as a wait.
 */
export function ServedFromHere() {
  return (
    <Shell>
      <ClickWhenDrawn testid="actor-serve-button">
        <FolderWorkbench source={PROBE} onClose={noop} onSaved={noop} />
      </ClickWhenDrawn>
    </Shell>
  );
}

/**
 * The folder went away underneath the browser — a `git checkout`, a rename, an unmounted volume —
 * and the listing was refused. The server's sentence is verbatim, because it names the path and both
 * of the two fixes; the aside says why it has no rows; and the editor says what it is waiting for
 * rather than drawing an empty grey rectangle that could equally be a read that never returned.
 */
export function TheFolderWentAway() {
  return (
    <Shell>
      <FolderWorkbench source={CRAWL4AI} onClose={noop} onSaved={noop} />
    </Shell>
  );
}
