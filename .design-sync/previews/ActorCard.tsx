import { ActorCard } from '@kontra/frontend';

function Frame({ children }: { children: React.ReactNode }) {
  return (
    <div className="dark">
      <div className="rounded-lg bg-background p-4 text-foreground">{children}</div>
    </div>
  );
}

const HEALTHY = { reachable: 'ok', session: 'present', poller: 'live', loads: 'ok' } as const;
const DEAD = { reachable: 'ok', session: 'present', poller: 'none', loads: 'failing' } as const;

function machine(over: Record<string, unknown> = {}) {
  return {
    id: 'probe-01',
    machine: 'kontra-probe-1',
    host: '10.124.0.4',
    publicIp: '164.92.71.18',
    tag: 'probe',
    fleet: 'sweep-aug',
    actor: 'probe',
    version: '0.2.0',
    window: 'probe',
    health: HEALTHY,
    ...over,
  };
}

/** The folder the orchestrator has registered for this Actor's code, on THIS disk. */
function folder(over: Record<string, unknown> = {}) {
  return {
    id: 'actor:probe:1f3k',
    kind: 'actor',
    name: 'probe',
    path: '/srv/checkout/examples/python/probe',
    version: '0.2.0',
    description: '',
    registeredAt: 1_752_000_000_000,
    ...over,
  };
}

const PROBE = {
  key: 'probe@0.2.0',
  name: 'probe',
  version: '0.2.0',
  schemaVersion: '1',
  digest: 'sha256:9f2c41b7d0ae5c38e1b6',
  source: '/srv/checkout/examples/python/probe',
  operations: [
    {
      name: 'head',
      description: 'GET each target and keep only the status line and the response headers.',
      input: {
        type: 'object',
        required: ['url'],
        properties: {
          url: { type: 'string', description: 'the absolute URL to probe' },
          timeout: { type: 'number' },
        },
      },
      output: {
        type: 'object',
        required: ['status', 'headers'],
        properties: {
          status: { type: 'integer' },
          headers: { type: 'object' },
          elapsed_ms: { type: 'number' },
        },
      },
    },
    {
      name: 'fetch',
      description: 'Fetch the body as well, capped at 512 KB.',
      input: {
        type: 'object',
        required: ['url'],
        properties: { url: { type: 'string' }, cap: { type: 'integer' } },
      },
      output: {
        type: 'object',
        required: ['body'],
        properties: { body: { type: 'string' }, truncated: { type: 'boolean' } },
      },
    },
  ],
};

const noop = () => {};

/**
 * The whole card, as an operator meets it: a served Actor with its code on this disk.
 * Both Methods are visible — the Method is the unit, not the Actor (ADR 0023 §9) — and
 * each row carries its own author's sentence.
 */
export function Served() {
  return (
    <Frame>
      <ActorCard
        actor={PROBE}
        machines={[machine(), machine({ id: 'probe-02', host: '10.124.0.5', publicIp: '164.92.71.22' })]}
        folder={folder()}
        open={false}
        onToggle={noop}
        onEdit={noop}
        onCall={noop}
      />
    </Frame>
  );
}

/**
 * Field tables open. THIS is what the disclosure is for: what a caller literally puts in
 * a Batch, derived from the author's own `takes=`/`emits=` types, never raw JSON Schema.
 */
export function FieldTablesOpen() {
  return (
    <Frame>
      <ActorCard
        actor={PROBE}
        machines={[machine()]}
        folder={folder()}
        open
        onToggle={noop}
        onEdit={noop}
        onCall={noop}
      />
    </Frame>
  );
}

/**
 * Four Machines, one of them not serving. Counted separately rather than folded in,
 * because "four Machines, one dead" and "three Machines" are different situations and
 * only one of them needs somebody.
 */
export function PartiallyServing() {
  return (
    <Frame>
      <ActorCard
        actor={PROBE}
        machines={[
          machine(),
          machine({ id: 'probe-02', host: '10.124.0.5' }),
          machine({ id: 'probe-03', host: '10.124.0.6' }),
          machine({ id: 'probe-04', host: '10.124.0.7', health: DEAD }),
        ]}
        folder={folder()}
        open={false}
        onToggle={noop}
        onEdit={noop}
        onCall={noop}
      />
    </Frame>
  );
}

/**
 * What registering this version said about the one before it. The Method and the
 * DIRECTION are both named: `head() output FORWARD` says which half of which signature
 * moved and which way. Registration was not refused — a breaking change is what versions
 * are for — and there is deliberately no "compatible" badge for the other case.
 */
export function BreaksACaller() {
  return (
    <Frame>
      <ActorCard
        actor={{
          ...PROBE,
          incompatibilities: [
            {
              previous: '0.1.0',
              method: 'head',
              field: 'elapsed_ms',
              rule: 'output FORWARD',
              detail: 'a caller of 0.1.0 does not read this field',
            },
            {
              previous: '0.1.0',
              method: 'fetch',
              field: 'cap',
              rule: 'input BACKWARD',
              detail: 'newly required — data shaped for 0.1.0 no longer fits',
            },
          ],
        }}
        machines={[machine()]}
        folder={folder()}
        open={false}
        onToggle={noop}
        onEdit={noop}
        onCall={noop}
      />
    </Frame>
  );
}

/**
 * A folder that is GONE — registered, then deleted underneath us. Every read of it 400s, so the
 * card withholds the editor and says why: this is fixable by putting the directory back.
 *
 * THERE USED TO BE A SECOND CARD HERE, for an Actor with no registered folder at all, and the
 * comment called them "two sentences the card must never collapse into one". That state no longer
 * exists: the Actors page is one row per REGISTERED FOLDER now, so an Actor without one is not
 * drawn at all rather than drawn as an unactionable card. `folder` went from optional to required
 * with it, and this cell passed `undefined` — which is why the render check caught it as
 * `Cannot read properties of undefined (reading 'path')` on a preview nobody had touched.
 */
export function NoCodeOnThisDisk() {
  const shared = { open: false, onToggle: noop, onEdit: noop, onCall: noop };
  return (
    <Frame>
      <ActorCard actor={PROBE} machines={[machine()]} folder={folder({ absent: true })} {...shared} />
    </Frame>
  );
}

/**
 * A load-only Actor: registered, deployed, and declaring no Methods at all. A real
 * deployment, not a broken registration — the badge says `load-only` and the body says
 * why there are no rows, instead of drawing an empty table.
 */
export function LoadOnly() {
  return (
    <Frame>
      <ActorCard
        actor={{
          key: 'dnsfacts@0.1.0',
          name: 'dnsfacts',
          version: '0.1.0',
          schemaVersion: '1',
          source: '/opt/kontra/actor/dnsfacts',
          operations: [],
        }}
        machines={[]}
        folder={folder({
          id: 'actor:dnsfacts:8c21',
          name: 'dnsfacts',
          path: '/srv/checkout/examples/python/dnsfacts',
          version: '0.1.0',
        })}
        open={false}
        onToggle={noop}
        onEdit={noop}
        onCall={noop}
      />
    </Frame>
  );
}

/**
 * An Actor nothing has registered a source or a digest for, whose Methods declare no
 * types. AN UNDECLARED SCHEMA SAYS SO: an empty field table is indistinguishable from a
 * Method that takes nothing, and that distinction is the reason the page is worth opening.
 */
export function NothingDeclared() {
  return (
    <Frame>
      <ActorCard
        actor={{
          key: 'nscheck@0.1.0',
          name: 'nscheck',
          version: '0.1.0',
          schemaVersion: '1',
          operations: [
            { name: 'lame', description: 'Ask every nameserver for the zone it claims to serve.' },
            { name: 'axfr' },
          ],
        }}
        machines={[machine({ id: 'ns-01', actor: 'nscheck', health: DEAD })]}
        folder={folder({
          id: 'actor:nscheck:4b7e',
          name: 'nscheck',
          path: '/srv/checkout/examples/go/nscheck',
          version: '0.1.0',
        })}
        open
        onToggle={noop}
        onEdit={noop}
        onCall={noop}
      />
    </Frame>
  );
}
