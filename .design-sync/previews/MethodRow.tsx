import { MethodRow } from '@kontra/frontend';

function Frame({ children }: { children: React.ReactNode }) {
  return (
    <div className="dark">
      <div className="rounded-lg bg-background p-4 text-foreground">{children}</div>
    </div>
  );
}

/** The Actors page draws rows inside the card, under a two-column strip. Same framing here, so a
 *  row is read the way it is met. */
function MethodTable({ children }: { children: React.ReactNode }) {
  return (
    <div className="overflow-hidden rounded-xl border border-border bg-card">
      <div className="flex gap-3 bg-muted px-3.5 py-1.5 text-[9px] uppercase tracking-wider text-muted-foreground">
        <span className="w-[118px] shrink-0">method</span>
        <span className="flex-1">takes → emits</span>
      </div>
      {children}
    </div>
  );
}

/** `probe`'s `Target`: a url OR a host, both optional — a derived optional arrives as a union and
 *  the row shows it as written. */
const TARGET = {
  type: 'object',
  properties: {
    url: { anyOf: [{ type: 'string' }, { type: 'null' }] },
    host: { anyOf: [{ type: 'string' }, { type: 'null' }] },
  },
};

const HEAD = {
  name: 'head',
  description: 'HEAD each target — status and headers, no body.',
  input: TARGET,
  output: {
    type: 'object',
    required: ['url', 'status', 'server'],
    properties: {
      url: { type: 'string' },
      status: { type: 'integer' },
      server: { type: 'string' },
    },
  },
};

const FETCH = {
  name: 'fetch',
  description: 'GET each target and emit its status, headers and body.',
  input: TARGET,
  output: {
    type: 'object',
    required: ['url', 'status', 'body'],
    properties: {
      url: { type: 'string' },
      status: { type: 'integer' },
      body: { type: 'string' },
    },
  },
};

const noop = () => {};

/**
 * The row as the Actors page draws it: what a caller types, what the author said it does, and the
 * one-line signature. `call` is there because probe's folder is registered and on disk — generating
 * is keyed by that folder, so the button appears exactly where `edit` does.
 */
export function Described() {
  return (
    <Frame>
      <MethodTable>
        <MethodRow actor="probe" op={HEAD} expanded={false} onCall={noop} />
      </MethodTable>
    </Frame>
  );
}

/**
 * The disclosure open. This is what a caller literally puts in a Batch — the author's own
 * `takes=`/`emits=` types flattened, never raw JSON Schema — and `required` is the half that
 * decides whether a dispatch fails at the actor or in the form.
 */
export function FieldTablesOpen() {
  return (
    <Frame>
      <MethodTable>
        <MethodRow actor="probe" op={FETCH} expanded onCall={noop} />
      </MethodTable>
    </Frame>
  );
}

/**
 * `examples/go/nscheck` declares no types at all. NOT DECLARED is its own answer and never an empty
 * table: a Method that takes nothing and a Method whose author declared nothing are different facts,
 * and only one of them can be called with anything. No `call` either — this is Scratch's inspector,
 * where a Method is picked rather than generated from.
 */
export function NothingDeclared() {
  return (
    <Frame>
      <MethodTable>
        <MethodRow
          actor="nscheck"
          op={{
            name: 'delegation',
            description:
              "Resolve each domain's delegated NS set — one domain in, one unit per nameserver out",
          }}
          expanded
        />
      </MethodTable>
    </Frame>
  );
}

/**
 * `linkfind` registers `a.Method("urls", urls)` — no `Does(…)`, no types. The row says the sentence
 * is MISSING rather than leaving a blank line: descriptions travel now, so silence here is an author
 * who wrote none, which is one docstring away from being fixed by whoever is reading the row.
 */
export function Undescribed() {
  return (
    <Frame>
      <MethodTable>
        <MethodRow actor="linkfind" op={{ name: 'urls' }} expanded={false} onCall={noop} />
        <MethodRow actor="linkfind" op={{ name: 'secrets' }} expanded={false} onCall={noop} />
      </MethodTable>
    </Frame>
  );
}

/**
 * `subfinder` declares run-wide `Params` as well as a signature. They are ACTOR-level config for the
 * whole run, not a third argument to the call, so they get their own labelled table under the two
 * that are the Method's — a caller who reads `max_subdomains` as a Unit field writes a Batch the
 * actor ignores.
 */
export function RunWideParams() {
  return (
    <Frame>
      <MethodTable>
        <MethodRow
          actor="subfinder"
          op={{
            name: 'enumerate',
            input: {
              type: 'object',
              required: ['domain'],
              properties: {
                domain: { type: 'string' },
                platform: { type: 'string' },
                program: { type: 'string' },
                program_url: { type: 'string' },
              },
            },
            output: {
              type: 'object',
              required: ['domain', 'subdomain'],
              properties: {
                domain: { type: 'string' },
                subdomain: { type: 'string' },
                platform: { type: 'string' },
                program: { type: 'string' },
                program_url: { type: 'string' },
              },
            },
            params: {
              type: 'object',
              properties: {
                max_subdomains: { type: 'integer' },
                timeout_seconds: { type: 'integer' },
                all: { type: 'boolean' },
              },
            },
          }}
          expanded
          onCall={noop}
        />
      </MethodTable>
    </Frame>
  );
}

/**
 * An Actor is not the unit — its Methods are, and each self-registers its own signature. Stacked,
 * the rows are what the reader picks BETWEEN: `head` and `fetch` differ in one emitted field, which
 * a card showing only `probe@0.2.0` would have left them to guess.
 */
export function TheMethodTable() {
  return (
    <Frame>
      <MethodTable>
        <MethodRow actor="probe" op={HEAD} expanded={false} onCall={noop} />
        <MethodRow actor="probe" op={FETCH} expanded={false} onCall={noop} />
      </MethodTable>
    </Frame>
  );
}
