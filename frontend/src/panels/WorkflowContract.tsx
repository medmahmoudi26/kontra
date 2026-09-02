/**
 * What a workflow TAKES, RETURNS and IS FOR — the three things the Workflows page could not say.
 *
 * The list beside this draws a file: a name, a size, an mtime, and a `description.md` if somebody
 * wrote one. None of that is the workflow's contract. The contract is in the source — the
 * `@workflow.run` annotations and the class docstring — and until the worker started pushing a
 * descriptor on serve (`internals/catalog.py:publish_workflow_catalog`) it never left the file.
 *
 * IT IS KEYED BY THE TYPE, NOT THE FILE, and the page passes the type it read out of the source it
 * is showing. A file can declare several `@workflow.defn` classes and each registers separately, so
 * a contract joined by filename would show one class's schemas under another's name.
 *
 * NOT REGISTERED IS ITS OWN STATE, and it is the common one: a workflow that has never been served
 * has told the catalog nothing, and saying "takes nothing" about it would be an invention. What is
 * drawn instead is the reason — nobody has served this type yet — because that is a thing the
 * operator can go and do with the button above.
 *
 * IT LIVES IN ITS OWN FILE so a test can draw it: `WorkflowsPage.tsx` imports CodeMirror at module
 * load, and the suite runs in node with no DOM. Same reason `ActorCard.tsx` is not in `ActorsPage`.
 */

import type { WorkflowDescriptor } from '../run/api';
import { readSchema, sayNothing, type SchemaReading } from './workflowContract';

export function WorkflowContract({
  type,
  descriptor,
}: {
  /** The `@workflow.defn` type this page is showing, read from the source. */
  type: string;
  /** What the catalog holds for that type, or undefined when nothing has registered it. */
  descriptor?: WorkflowDescriptor;
}): JSX.Element {
  if (!descriptor) {
    return (
      <div className="text-[11.5px] text-muted-foreground" data-testid="workflow-contract-absent">
        <span className="font-mono">{type || 'this workflow'}</span> has not registered a contract.
        A worker describes what a workflow takes, returns and is for when it SERVES it — serve this
        one and what it declares appears here.
      </div>
    );
  }
  // A BROKEN FILE IS A STATE, NOT A SILENCE. When watch mode re-derives the contract after a save
  // and the file no longer imports, there is no schema — and drawing the last good form would read
  // as "your code is fine" over a file that will not load. So the error takes precedence over the
  // schema slots (which the broken descriptor cleared anyway): the panel says the file no longer
  // imports and shows what the import said. Recovering re-derives a clean descriptor with no error,
  // and the form comes back — without the serve being restarted.
  if (descriptor.error) {
    return (
      <div data-testid="workflow-contract-broken">
        <div className="flex items-baseline gap-2">
          <span className="font-mono text-[12px] font-semibold">{descriptor.name}</span>
          <span className="text-[9.5px] uppercase tracking-wide text-red-500">
            this file no longer imports
          </span>
        </div>
        <pre
          className="m-0 mt-1 max-h-40 overflow-auto whitespace-pre-wrap rounded bg-red-500/10 p-2 font-mono text-[11px] text-red-600 dark:text-red-400"
          data-testid="workflow-contract-error"
        >
          {descriptor.error}
        </pre>
      </div>
    );
  }
  const input = readSchema(descriptor.input);
  const output = readSchema(descriptor.output);
  return (
    <div data-testid="workflow-contract">
      <div className="flex items-baseline gap-2">
        <span className="font-mono text-[12px] font-semibold">{descriptor.name}</span>
        <span className="text-[9.5px] uppercase tracking-wide text-muted-foreground">
          as its worker described it
        </span>
      </div>
      {/* THE AUTHOR'S OWN WORDS, from the class docstring's first paragraph. Absent means the
          author wrote none — a fixable omission by the person reading this, not a field the
          system lacks — so nothing is drawn rather than a placeholder pretending to one. */}
      {descriptor.description && (
        <p className="m-0 mt-1 text-[11.5px] text-muted-foreground" data-testid="workflow-description">
          {descriptor.description}
        </p>
      )}
      <div className="mt-2 flex flex-wrap gap-6">
        <Slot label="input" reading={input} />
        <Slot label="output" reading={output} />
      </div>
    </div>
  );
}

/** One slot of the contract: a field table, or the sentence that says why there is not one. */
function Slot({ label, reading }: { label: string; reading: SchemaReading }): JSX.Element {
  return (
    <div data-testid={`workflow-${label}`}>
      <div className="font-mono text-[10px] uppercase tracking-wide text-muted-foreground">
        {label}
      </div>
      {reading.kind === 'fields' ? (
        <ul className="m-0 list-none p-0">
          {reading.fields.map((f) => (
            <li key={f.name} className="flex gap-2 font-mono text-[11px]">
              <span>{f.name}</span>
              <span className="text-muted-foreground">{f.type}</span>
              {f.required && <span className="text-[10px] text-amber-500">required</span>}
            </li>
          ))}
        </ul>
      ) : (
        // NEVER AN EMPTY TABLE. `dict` derives a schema with no properties — "any object", not "an
        // object with no fields" — and a zero-row table under a column header is how the second
        // reading happens. `ping` reaches this on both slots; `nscheck` reaches it on its `dict`
        // output while its typed input draws the table above.
        <div className="text-[11px] italic text-muted-foreground">{sayNothing(reading)}</div>
      )}
    </div>
  );
}
