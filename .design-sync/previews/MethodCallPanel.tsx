import { MethodCallPanel } from '@kontra/frontend';

function Frame({ children }: { children: React.ReactNode }) {
  return (
    <div className="dark">
      <div className="rounded-lg bg-background p-4 text-foreground">{children}</div>
    </div>
  );
}

const PROBE = {
  key: 'probe@0.2.0',
  name: 'probe',
  version: '0.2.0',
  schemaVersion: '1',
  source: '/srv/checkout/examples/python/probe',
  operations: [],
};

const SUBFINDER = {
  key: 'subfinder@0.1.0',
  name: 'subfinder',
  version: '0.1.0',
  schemaVersion: '1',
  source: '/srv/checkout/examples/go/subfinder',
  operations: [],
};

const NSCHECK = {
  key: 'nscheck@0.1.0',
  name: 'nscheck',
  version: '0.1.0',
  schemaVersion: '1',
  source: '/opt/kontra/actor/nscheck',
  operations: [],
};

const HEAD = {
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
};

const ENUMERATE = {
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
  params: {
    type: 'object',
    properties: {
      max_subdomains: { type: 'integer' },
      timeout_seconds: { type: 'integer' },
      all: { type: 'boolean' },
    },
  },
};

/** `examples/go/nscheck` declares no types — so the Batch is raw JSON, not a table. */
const DELEGATION = {
  name: 'delegation',
  description:
    "Resolve each domain's delegated NS set — one domain in, one unit per nameserver out",
};

const ENUM_FIELDS = [
  { name: 'domain', type: 'string', required: true },
  { name: 'platform', type: 'string', required: false },
  { name: 'program', type: 'string', required: false },
];

const HEAD_FIELDS = [
  { name: 'url', type: 'string', required: true },
  { name: 'timeout', type: 'integer', required: false },
];

/** The registered Workflow folders. `ping` is absent — its directory left with a checkout — and the
 *  picker drops it, because every write to it is a 400. */
const FOLDERS = [
  {
    id: 'workflow:nscheck:aa41',
    kind: 'workflow',
    name: 'nscheck',
    path: '/home/med/.kontra/workflows/nscheck',
    version: '',
    description: '',
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
  {
    id: 'workflow:ping:19bd',
    kind: 'workflow',
    name: 'ping',
    path: '/home/med/.kontra/workflows/ping',
    version: '',
    description: '',
    registeredAt: 1_752_000_000_000,
    absent: true,
  },
];

/** What `POST /api/sources/actor/:id/caller` answers with — `actorControl.ts:callerFor`'s own
 *  output, down to the two commands in the docstring the panel reads back out. */
const CALLER = {
  filename: 'workflow.py',
  source: `"""Dispatch probe@0.2.0.head() over a Batch.

Shown by the Actors page beside its Run button — and this IS what that button runs (ADR 0033 §3):
the probe workflow makes the same call, on the same handle, through the same Nexus operation. Own
it when you want more than one call; the probe has nowhere to put a second Method (ADR 0033 §1).

    kontra workflow serve workflow.py --tmux
    kontra workflow start workflow.py --wait
"""

from actorkit import catalog
from temporalio import workflow

probe = catalog.actor("probe", "0.2.0")

# The Batch, as typed on the Actors page. Edit it here, or pass one to workflow start --input.
BATCH = [
    {
        "url": "http://acme-staging.test",
        "timeout": 5
    }
]


@workflow.defn
class ProbeHead:
    @workflow.run
    async def run(self, units: list | None = None) -> dict:
        batch = units if units is not None else BATCH
        out = await probe.dispatch(batch, method="head")
        return {"units": len(batch), "out": len(out)}


if __name__ == "__main__":
    catalog.serve([ProbeHead])
`,
};

const noop = () => {};

const base = {
  onDraft: noop,
  onGenerate: noop,
  onTarget: noop,
  onSave: noop,
  onClose: noop,
  busy: null,
  error: null,
  caller: null,
  saved: null,
  folders: FOLDERS,
  target: 'workflow:nscheck:aa41',
};

/**
 * A Batch is a LIST, so the form is a table: one row per Unit, columns from the Method's own
 * declared input. A form that collected one object and quietly wrapped it would teach the shape
 * wrong on the surface whose whole job is to teach it — and `required` is drawn because a blank
 * there is a dispatch that dies minutes later in a tmux pane.
 */
export function TheBatchTyped() {
  return (
    <Frame>
      <MethodCallPanel
        {...base}
        actor={SUBFINDER}
        op={ENUMERATE}
        draft={{
          kind: 'fields',
          fields: ENUM_FIELDS,
          units: [
            { domain: 'acme.com', platform: 'hackerone', program: 'acme' },
            { domain: 'acme-staging.com', platform: 'hackerone', program: 'acme' },
            { domain: 'acmecdn.net', platform: 'hackerone', program: 'acme' },
          ],
        }}
        batch={{
          units: [
            { domain: 'acme.com', platform: 'hackerone', program: 'acme' },
            { domain: 'acme-staging.com', platform: 'hackerone', program: 'acme' },
            { domain: 'acmecdn.net', platform: 'hackerone', program: 'acme' },
          ],
        }}
      />
    </Frame>
  );
}

/**
 * ONE reason, and it names the Unit and the field. A list of every problem at once reads as a wall;
 * this is the one the operator is about to fix, and the button stays shut until they have — the
 * whole point of validating here rather than at the far end of a serve.
 */
export function NotABatchYet() {
  return (
    <Frame>
      <MethodCallPanel
        {...base}
        actor={SUBFINDER}
        op={ENUMERATE}
        draft={{
          kind: 'fields',
          fields: ENUM_FIELDS,
          units: [
            { domain: 'acme.com', platform: 'hackerone', program: 'acme' },
            { domain: '', platform: 'hackerone', program: 'acme' },
          ],
        }}
        batch={{ error: 'unit 2 · domain is required' }}
      />
    </Frame>
  );
}

/**
 * A Method whose author declared nothing gets a JSON box and a sentence saying so. An empty field
 * table would claim the Method takes nothing — the one confusion the Actors page exists to prevent,
 * and `examples/go/nscheck` is a real Actor in exactly this state.
 */
export function NoInputSchema() {
  return (
    <Frame>
      <MethodCallPanel
        {...base}
        actor={NSCHECK}
        op={DELEGATION}
        draft={{
          kind: 'json',
          text: '[\n  {"domain": "acme.com"},\n  {"domain": "acme-staging.com"}\n]',
        }}
        batch={{ units: [{ domain: 'acme.com' }, { domain: 'acme-staging.com' }] }}
      />
    </Frame>
  );
}

/**
 * The server's refusal, verbatim. Generation is keyed by the registered folder the Actor's code is
 * in, so the route can refuse a folder it does not have — a thing the reader can act on, and worth
 * more than "something went wrong".
 */
export function TheServerRefused() {
  return (
    <Frame>
      <MethodCallPanel
        {...base}
        actor={PROBE}
        op={HEAD}
        draft={{
          kind: 'fields',
          fields: HEAD_FIELDS,
          units: [{ url: 'http://acme-staging.test', timeout: '5' }],
        }}
        batch={{ units: [{ url: 'http://acme-staging.test', timeout: 5 }] }}
        error="generate the caller failed: 400 Bad Request — /srv/checkout/examples/python/probe is not a registered actor folder"
      />
    </Frame>
  );
}

/**
 * What the panel hands back is a FILE, and the two commands that turn it into a Run are printed
 * beside it — read out of the file's own docstring, so the page and the file cannot name two
 * different queues. Once it has been written, the serve names the folder it landed in rather than
 * the bare `workflow.py` the docstring can only guess at.
 */
export function Written() {
  return (
    <Frame>
      <MethodCallPanel
        {...base}
        actor={PROBE}
        op={HEAD}
        draft={{
          kind: 'fields',
          fields: HEAD_FIELDS,
          units: [{ url: 'http://acme-staging.test', timeout: '5' }],
        }}
        batch={{ units: [{ url: 'http://acme-staging.test', timeout: 5 }] }}
        caller={CALLER}
        saved={{
          folder: '/home/med/.kontra/workflows/nscheck',
          file: '/home/med/.kontra/workflows/nscheck/workflow.py',
        }}
      />
    </Frame>
  );
}

/**
 * Nothing to save into. `workflow.py` is the marker that MAKES a folder a Workflow, so there is no
 * "create one here" — the instruction is to register a folder on the Workflows page, and until then
 * the source above is still copyable. A dead picker would have offered a write that 400s.
 */
export function NoWorkflowFolder() {
  return (
    <Frame>
      <MethodCallPanel
        {...base}
        actor={PROBE}
        op={HEAD}
        draft={{
          kind: 'fields',
          fields: HEAD_FIELDS,
          units: [{ url: 'http://acme-staging.test', timeout: '5' }],
        }}
        batch={{ units: [{ url: 'http://acme-staging.test', timeout: 5 }] }}
        caller={CALLER}
        folders={[]}
        target=""
      />
    </Frame>
  );
}
