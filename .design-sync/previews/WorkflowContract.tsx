import { WorkflowContract } from '@kontra/frontend';

function Frame({ children }: { children: React.ReactNode }) {
  return (
    <div className="dark">
      <div className="rounded-lg bg-background p-4 text-foreground">{children}</div>
    </div>
  );
}

const SAVED_AT = 1_752_000_000_000;

/**
 * A workflow whose author annotated a model instead of `dict`: the ordinary case, and the only one
 * with a table under it. The sentence is the class docstring's first paragraph — nothing else on the
 * Workflows page could say what a file is FOR, because a name, a size and an mtime do not.
 */
export function Registered() {
  return (
    <Frame>
      <WorkflowContract
        type="NsCheck"
        descriptor={{
          name: 'NsCheck',
          description:
            'Page a Dataset of domains, resolve each delegation on a fleet, and write the lame ones.',
          input: {
            type: 'object',
            required: ['dataset'],
            properties: {
              dataset: { type: 'string' },
              into: { type: 'string' },
              machines: { type: 'integer' },
              sessions: { type: 'integer' },
              size: { type: 'integer' },
            },
          },
          output: {
            type: 'object',
            properties: {
              pairs: { type: 'integer' },
              checked: { type: 'integer' },
              dropped: { type: 'integer' },
            },
          },
          savedAt: SAVED_AT,
        }}
      />
    </Frame>
  );
}

/**
 * What `.kontra/workflows/ping` really registers — `dict | None` in, `dict` out. Pydantic derives a
 * document with no `properties`, meaning ANY object, and an empty field table under a column header
 * would read as "takes an object with nothing in it": the opposite. So it gets a sentence and no
 * table. The description line is absent because that author wrote no docstring.
 */
export function DeclaresNoFields() {
  return (
    <Frame>
      <WorkflowContract
        type="Ping"
        descriptor={{
          name: 'Ping',
          input: { anyOf: [{ type: 'object', additionalProperties: true }, { type: 'null' }] },
          output: { type: 'object', additionalProperties: true },
          savedAt: SAVED_AT,
        }}
      />
    </Frame>
  );
}

/**
 * NOT DECLARED is a third answer, and it is not "declares no fields". An unannotated `run` told the
 * catalog nothing about either slot; a workflow that annotated `dict` told it "anything fits". Only
 * one of those is the author's omission.
 */
export function NotDeclared() {
  return (
    <Frame>
      <WorkflowContract type="ProbeHead" descriptor={{ name: 'ProbeHead', savedAt: SAVED_AT }} />
    </Frame>
  );
}

/**
 * One slot annotated and one not — the state that proves the two readings are per-slot rather than
 * per-workflow. A caller can be told exactly what to put in `--input` and still have nothing said
 * about what comes back.
 */
export function OneSlotEach() {
  return (
    <Frame>
      <WorkflowContract
        type="DnsSweep"
        descriptor={{
          name: 'DnsSweep',
          description: 'Page a Dataset, resolve each Batch, then ask which of them answer HTTP.',
          input: {
            type: 'object',
            required: ['dataset'],
            properties: {
              dataset: { type: 'string' },
              into: { type: 'string' },
              size: { type: 'integer' },
            },
          },
          savedAt: SAVED_AT,
        }}
      />
    </Frame>
  );
}

/**
 * The common state, and the reason the panel exists at all: a workflow that has never been served
 * has registered nothing. Saying "takes nothing" about it would be an invention — what is drawn is
 * the reason, which is something the operator can go and do with the serve button above.
 */
export function NeverServed() {
  return (
    <Frame>
      <WorkflowContract type="NsCheck" />
    </Frame>
  );
}

/**
 * Where it actually sits: above the two commands that serve and start the type, in the column beside
 * the editor. The `--input` on the second line is the thing the contract describes.
 */
export function BesideTheCommands() {
  return (
    <Frame>
      <div className="flex flex-col gap-3">
        <WorkflowContract
          type="DnsSweep"
          descriptor={{
            name: 'DnsSweep',
            description: 'Page a Dataset, resolve each Batch, then ask which of them answer HTTP.',
            input: {
              type: 'object',
              required: ['dataset'],
              properties: { dataset: { type: 'string' }, size: { type: 'integer' } },
            },
            output: { type: 'object', additionalProperties: true },
            savedAt: SAVED_AT,
          }}
        />
        <pre className="m-0 overflow-x-auto rounded bg-muted/50 px-2 py-1.5 font-mono text-[11px] text-muted-foreground">
          {`kontra workflow serve dnssweep.py --queue recon --tmux\nkontra workflow start DnsSweep --queue recon --input '{"dataset":"targets"}'`}
        </pre>
      </div>
    </Frame>
  );
}
