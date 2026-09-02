import { ScratchInspector } from '@kontra/frontend';

/**
 * The inspector is a SIDECAR — a 304px column with a right border, pinned down the left of the
 * canvas. Framed against the page background with the canvas's edge showing, because that
 * border is the component's own and reads as a seam rather than as a card outline.
 */
function Frame({ children }: { children: React.ReactNode }) {
  return (
    <div className="dark">
      <div
        className="flex rounded-lg bg-background text-foreground"
        style={{ width: 372, overflow: 'hidden' }}
      >
        {children}
      </div>
    </div>
  );
}

/** When the lake listing behind these numbers answered. Every count drawn from it is a fact
 *  about this moment, which is why `asOfText` prints it beside them. */
const MEASURED = Date.parse('2024-05-15T11:58:12Z');

const noop = () => undefined;

const TARGET = {
  type: 'object',
  properties: { url: { type: ['string', 'null'] }, host: { type: ['string', 'null'] } },
};

/** `probe@0.2.0`, as its worker described it — the docstring's first paragraph and the two
 *  dataclasses the Method is annotated with. */
const FETCH = {
  name: 'fetch',
  description:
    'GET each target and emit its status, headers and body. Follows redirects. The body is whatever came back — deciding what is interesting is the caller’s job and not this one’s.',
  input: TARGET,
  output: {
    type: 'object',
    properties: {
      url: { type: 'string' },
      status: { type: 'integer' },
      body: { type: 'string' },
    },
    required: ['url', 'status', 'body'],
  },
};

const HEAD = {
  name: 'head',
  description:
    'HEAD each target — status and headers, no body. The cheap half of fetch, for deciding whether a target is worth fetching at all.',
  input: TARGET,
  output: {
    type: 'object',
    properties: {
      url: { type: 'string' },
      status: { type: 'integer' },
      server: { type: 'string' },
    },
    required: ['url', 'status', 'server'],
  },
};

/** The Go actor: two Methods, each with `Does(...)` and neither with `Takes`/`Emits`. Its
 *  signatures read `not declared`, which is a fact about the actor and not a gap here. */
const DELEGATION = {
  name: 'delegation',
  description:
    "Resolve each domain's delegated NS set — one domain in, one unit per nameserver out",
};

const ASK = {
  name: 'ask',
  description: 'Ask one nameserver whether it actually serves the zone it is delegated for',
};

const actor = (id: string, name: string, version: string, method: string) => ({
  id,
  kind: 'actor',
  at: { x: 60, y: 48 },
  actor: name,
  version,
  method,
});

/**
 * THE READING THE CANVAS HAS NO ROOM FOR. The node says `probe@0.2.0 .fetch()`; what `fetch`
 * is FOR, and what it takes and emits, is what decides whether the edge being drawn makes
 * sense — and it was reachable only by leaving Scratch for the Actors page and coming back.
 *
 * The field tables open on the Method IN USE, because that is the one whose types the edges
 * either fit or do not. `head` keeps its description and its `takes → emits` summary, which is
 * what you choose BETWEEN.
 */
export function ActorWithItsMethods() {
  return (
    <Frame>
      <ScratchInspector
        reading={{
          kind: 'actor',
          node: actor('n1', 'probe', '0.2.0', 'fetch'),
          methods: [FETCH, HEAD],
          standing: { state: 'resolved', op: FETCH },
        }}
        fallout={[]}
        onPickMethod={noop}
      />
    </Frame>
  );
}

/**
 * A METHOD CHANGE IS A CHANGE TO WHAT THE NODE IS, and the edges already drawn were drawn
 * against the old signature. `fetch` emitted a body and `head` does not, so everything
 * downstream was reading something that is no longer there.
 *
 * NOTHING WAS REMOVED, and the strip says so first — that is the question an amber block
 * raises. The author drew those lines; only a person can say whether they still mean what
 * they meant by them.
 */
export function MethodChangeFallout() {
  return (
    <Frame>
      <ScratchInspector
        reading={{
          kind: 'actor',
          node: actor('n1', 'probe', '0.2.0', 'head'),
          methods: [FETCH, HEAD],
          standing: { state: 'resolved', op: HEAD },
        }}
        fallout={[
          {
            edgeId: 'e1',
            side: 'out',
            other: 'crawl_pages',
            detail:
              'head() emits {url: string, status: integer, server: string}, where fetch() emitted {url: string, status: integer, body: string} — crawl_pages was drawn reading the old one.',
          },
          {
            edgeId: 'e2',
            side: 'out',
            other: 'linkfind@0.1.0.extract()',
            detail:
              'head() emits {url: string, status: integer, server: string}, where fetch() emitted {url: string, status: integer, body: string} — linkfind@0.1.0.extract() was drawn reading the old one.',
          },
        ]}
        onPickMethod={noop}
      />
    </Frame>
  );
}

/**
 * THE HALF THE CANVAS HAD NO ANSWER FOR AT ALL. The palette lists one row per Method, so
 * placing a node was the only moment the Method was ever chosen — grab the wrong row and the
 * node could only be deleted and drawn again, taking its edges with it.
 *
 * Not an error, and the sentence says what to do rather than what is wrong. `nscheck` is the
 * Go actor: both Methods carry `Does(...)` and neither declares `Takes`/`Emits`, so their
 * summaries read `not declared` — which is why the descriptions are what you pick between.
 */
export function NoMethodChosenYet() {
  return (
    <Frame>
      <ScratchInspector
        reading={{
          kind: 'actor',
          node: actor('n2', 'nscheck', '0.1.0', ''),
          methods: [DELEGATION, ASK],
          standing: { state: 'unchosen' },
        }}
        fallout={[]}
        onPickMethod={noop}
      />
    </Frame>
  );
}

/**
 * A SELECTION WITH NOTHING TO CONFIGURE. The file is in `.kontra/workflows/` and its
 * `description.md` is drawn — but a contract comes from a WORKER, on serve, and nobody has
 * served this one. So there is no Method to pick and no field table to read: the panel says
 * which absence it is and what the operator can go and do about it.
 */
export function WorkflowWithNoContract() {
  return (
    <Frame>
      <ScratchInspector
        reading={{
          kind: 'workflow',
          node: { id: 'n3', kind: 'workflow', at: { x: 336, y: 48 }, file: 'sweep.py' },
          file: {
            name: 'sweep.py',
            bytes: 2814,
            modifiedAt: MEASURED - 86_400_000,
            description:
              're-dispatch just the drops, singly — the branch a graph has no shape for.',
          },
          listed: true,
          standing: { state: 'unregistered' },
        }}
        fallout={[]}
        onPickMethod={noop}
      />
    </Frame>
  );
}

/**
 * A DATASET THE LAKE HAS ANSWERED FOR. Three things at once, and each is a different question:
 * what it IS (drawn as written to, derived from where the edges land), what is IN it (the
 * columns, which are also this node's ports), and what the lake holds under the name.
 *
 * THE COUNT CARRIES ITS SCOPE and the moment it was measured. `26,543 rows · every run` is
 * the Dataset total across three dispatches — not one partition's contribution — and `open`
 * says a Run may still be appending to it, which is why the timestamp is beside the number.
 */
export function DatasetInTheLake() {
  const dispatches = [
    {
      kind: 'output',
      name: 'crawl_pages',
      version: '0.2.0',
      dt: '2024-05-15T11-40-02',
      rows: 12_904,
      bytes: 41_882_016,
      updatedAt: MEASURED - 41_000,
      state: 'open',
    },
    {
      kind: 'output',
      name: 'crawl_pages',
      version: '0.2.0',
      dt: '2024-05-14T09-02-55',
      rows: 9_431,
      bytes: 30_114_770,
      updatedAt: MEASURED - 96_400_000,
      state: 'open',
    },
    {
      kind: 'output',
      name: 'crawl_pages',
      version: '0.1.0',
      dt: '2024-05-11T17-21-38',
      rows: 4_208,
      bytes: 13_902_441,
      updatedAt: MEASURED - 358_000_000,
      state: 'open',
    },
  ];
  const rows = 12_904 + 9_431 + 4_208;
  const bytes = 41_882_016 + 30_114_770 + 13_902_441;
  return (
    <Frame>
      <ScratchInspector
        reading={{
          kind: 'dataset',
          node: {
            id: 'n4',
            kind: 'dataset',
            at: { x: 612, y: 48 },
            name: 'crawl_pages',
            direction: 'out',
          },
          measuredAt: MEASURED,
          columns: {
            state: 'listed',
            columns: [
              { name: 'url', type: 'VARCHAR' },
              { name: 'status', type: 'BIGINT' },
              { name: 'title', type: 'VARCHAR' },
              { name: 'body', type: 'STRUCT(body_len BIGINT, body_preview VARCHAR)' },
              { name: 'fetched_at', type: 'TIMESTAMP' },
            ],
          },
          standing: {
            state: 'listed',
            groups: [
              {
                kind: 'output',
                name: 'crawl_pages',
                dispatches,
                total: {
                  total: {
                    rows,
                    scope: 'dataset',
                    statement: `datasets:listing@${MEASURED}`,
                    measuredAt: MEASURED,
                  },
                  bytes,
                  dispatches: dispatches.length,
                  kind: 'output',
                },
                bytes,
                updatedAt: MEASURED - 41_000,
                versions: ['0.2.0', '0.1.0'],
              },
            ],
          },
        }}
        fallout={[]}
        onPickMethod={noop}
      />
    </Frame>
  );
}

/**
 * THE OTHER SELECTION WITH NOTHING TO CONFIGURE, and the ordinary way an output is drawn: a
 * name nothing has written yet. The listing HAS answered — that is what makes this a claim
 * rather than a silence — so the panel names the Dataset, offers the second reading (a name
 * spelled differently in the lake), and says the columns are none rather than drawing an empty
 * table that would read as a Dataset with no columns.
 */
export function DatasetNothingHasWritten() {
  return (
    <Frame>
      <ScratchInspector
        reading={{
          kind: 'dataset',
          node: { id: 'n5', kind: 'dataset', at: { x: 612, y: 200 }, name: 'lame', direction: 'out' },
          measuredAt: MEASURED,
          columns: { state: 'absent' },
          standing: { state: 'absent' },
        }}
        fallout={[]}
        onPickMethod={noop}
      />
    </Frame>
  );
}
