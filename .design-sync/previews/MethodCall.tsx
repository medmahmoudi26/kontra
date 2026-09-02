import { MethodCall } from '@kontra/frontend';

/**
 * The shelf of registered Workflow folders, as `GET /api/sources/workflow` answers it.
 *
 * MethodCall is the half that TALKS TO THE SERVER — it reads this list on mount so the generated
 * file has somewhere to be written — and a card has no orchestrator behind it. Answering that one
 * route here is what keeps these cells showing the component's real states instead of an unreachable
 * orchestrator's error banner. Nothing else is stubbed and nothing is re-implemented: what is drawn
 * below is the real component, in the state a real response puts it in.
 */
const SHELF = {
  defaultRoot: '/home/med/.kontra/workflows',
  sources: [
    {
      id: 'workflow:nscheck:aa41',
      kind: 'workflow',
      name: 'nscheck',
      path: '/home/med/.kontra/workflows/nscheck',
      version: '',
      description: 'Page a Dataset of domains and check every delegation on a fleet.',
      registeredAt: 1_752_000_000_000,
    },
    {
      id: 'workflow:dnssweep:7c02',
      kind: 'workflow',
      name: 'dnssweep',
      path: '/srv/checkout/examples/python/workflows/dnssweep',
      version: '',
      description: '',
      registeredAt: 1_752_000_000_000,
    },
  ],
};

const realFetch = globalThis.fetch.bind(globalThis);
globalThis.fetch = ((input: RequestInfo | URL, init?: RequestInit) => {
  const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
  if (url.includes('/sources/workflow')) {
    return Promise.resolve(
      new Response(JSON.stringify(SHELF), {
        status: 200,
        headers: { 'content-type': 'application/json' },
      })
    );
  }
  return realFetch(input, init);
}) as typeof fetch;

function Frame({ children }: { children: React.ReactNode }) {
  return (
    <div className="dark">
      <div className="rounded-lg bg-background p-4 text-foreground">{children}</div>
    </div>
  );
}

/** The registered folder holding the Actor's code — the id the generate route is keyed by, which is
 *  why an Actor catalogued by a worker on a droplet has no call button at all. */
const PROBE_FOLDER = {
  id: 'actor:probe:1f3k',
  kind: 'actor',
  name: 'probe',
  path: '/srv/checkout/examples/python/probe',
  version: '0.2.0',
  description: '',
  registeredAt: 1_752_000_000_000,
};

const NSCHECK_FOLDER = {
  id: 'actor:nscheck:6b12',
  kind: 'actor',
  name: 'nscheck',
  path: '/srv/checkout/examples/go/nscheck',
  version: '0.1.0',
  description: '',
  registeredAt: 1_752_000_000_000,
};

const SUBFINDER_FOLDER = {
  id: 'actor:subfinder:c803',
  kind: 'actor',
  name: 'subfinder',
  path: '/srv/checkout/examples/go/subfinder',
  version: '0.1.0',
  description: '',
  registeredAt: 1_752_000_000_000,
};

const noop = () => {};

/**
 * What an operator meets the moment they press `call` on a Method row: the form is built ONCE from
 * the declared input, so it opens with one empty Unit — and an empty required field is not a Batch
 * yet. The refusal names the Unit and the field, and the button stays shut, because generating
 * anyway writes a file that fails at the far end of a `kontra workflow serve` minutes later.
 */
export function AtOpen() {
  return (
    <Frame>
      <MethodCall
        actor={{
          key: 'probe@0.2.0',
          name: 'probe',
          version: '0.2.0',
          schemaVersion: '1',
          source: '/srv/checkout/examples/python/probe',
          operations: [],
        }}
        op={{
          name: 'head',
          description: 'HEAD each target — status and headers, no body.',
          input: {
            type: 'object',
            required: ['url'],
            properties: { url: { type: 'string' }, timeout: { type: 'integer' } },
          },
          output: {
            type: 'object',
            required: ['url', 'status', 'server'],
            properties: {
              url: { type: 'string' },
              status: { type: 'integer' },
              server: { type: 'string' },
            },
          },
        }}
        folder={PROBE_FOLDER}
        onClose={noop}
      />
    </Frame>
  );
}

/**
 * `examples/go/nscheck` declares no types, so there is nothing to build a table out of — the draft
 * falls back to a JSON list holding one Unit, which is a legal Batch, so this one opens ready to
 * generate. The subtitle says WHY it is a box rather than a form; an empty field table would have
 * claimed the Method takes nothing.
 */
export function NoInputSchema() {
  return (
    <Frame>
      <MethodCall
        actor={{
          key: 'nscheck@0.1.0',
          name: 'nscheck',
          version: '0.1.0',
          schemaVersion: '1',
          source: '/srv/checkout/examples/go/nscheck',
          operations: [],
        }}
        op={{
          name: 'delegation',
          description:
            "Resolve each domain's delegated NS set — one domain in, one unit per nameserver out",
        }}
        folder={NSCHECK_FOLDER}
        onClose={noop}
      />
    </Frame>
  );
}

/**
 * An Actor that declares run-wide `params` — `subfinder`'s `max_subdomains`, `timeout_seconds`,
 * `all`. The generator passes NONE of them, and the omission is named here rather than discovered
 * at the far end of a serve, where an actor called without its dials just behaves differently.
 */
export function RunWideParams() {
  return (
    <Frame>
      <MethodCall
        actor={{
          key: 'subfinder@0.1.0',
          name: 'subfinder',
          version: '0.1.0',
          schemaVersion: '1',
          source: '/srv/checkout/examples/go/subfinder',
          operations: [],
        }}
        op={{
          name: 'enumerate',
          input: {
            type: 'object',
            required: ['domain'],
            properties: {
              domain: { type: 'string' },
              platform: { type: 'string' },
              program: { type: 'string' },
            },
          },
          output: {
            type: 'object',
            required: ['domain', 'subdomain'],
            properties: { domain: { type: 'string' }, subdomain: { type: 'string' } },
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
        folder={SUBFINDER_FOLDER}
        onClose={noop}
      />
    </Frame>
  );
}
