/**
 * Starting a run is a FORM, not a JSON blob — drawn.
 *
 * A workflow that declares its input gets a field per property, labelled and typed and marked where
 * it is required, the same way the Actors page collects a Batch. A workflow that declares nothing —
 * `ping`'s `dict`, or a run that takes no argument at all — keeps the raw JSON box, because a field
 * table with no columns is a form claiming the workflow takes nothing, which is the one reading the
 * contract beside it exists to prevent. The box SAYS which of those it is, since the operator can fix
 * one (write a type) and is meant to leave the other alone.
 *
 * IT LIVES IN ITS OWN FILE for the reason `WorkflowContract.tsx` does: `WorkflowsPage.tsx` imports
 * CodeMirror at module load and the suite runs in node with no DOM, so a form that page could only
 * draw would be a form no test could. Props in, markup out — native inputs, nothing that reaches
 * xterm — so `workflowInput.render.test.ts` draws every state of it.
 *
 * THE FIELDS COME FROM THE DRAFT, which `draftForInput` built from `readSchema` — the SAME reading
 * `WorkflowContract` draws its table from. So this form can never offer a field that card does not
 * show; they are one reading of the workflow's input, rendered at two depths: the card's table stops
 * at the top level, and the form goes all the way down through `FieldGroup`.
 */

import { FieldGroup } from './FieldGroup';
import type { InputDraft, InputResult } from './workflowInput';
import { setInputJson, setValues } from './workflowInput';

export function WorkflowInputForm({
  draft,
  onDraft,
  result,
}: {
  draft: InputDraft;
  onDraft(next: InputDraft): void;
  /** What the draft currently coerces to — or the field/parse error that blocks the start. */
  result: InputResult;
}): JSX.Element {
  const blocked = 'error' in result ? result.error : null;
  return (
    <div data-testid="workflow-input">
      {draft.kind === 'fields' ? (
        // THE WHOLE TREE, THROUGH THE SHARED DRAWING. A nested object is a nested group and an array
        // is a repeatable row here exactly as it is on the Actors page, because it is the same
        // component — the container is what differs between the two surfaces, never the cell.
        <FieldGroup
          nodes={draft.fields}
          values={draft.values}
          onValues={(next) => onDraft(setValues(draft, next))}
          testPrefix="workflow-input-field"
        />
      ) : (
        <label className="flex flex-col gap-1 text-[11px] text-muted-foreground">
          {/* WHY IT IS A BOX AND NOT A FORM. `undeclared` is the author's omission — a type
              annotation away from a form — and `no-fields` is a declared open shape the operator is
              meant to fill freely. Different sentences, because one is fixable and the other is not a
              fault. */}
          <span data-testid="workflow-input-why">
            {draft.why === 'undeclared'
              ? 'This workflow declares no input — its run takes one free-form argument. Type it as JSON, or leave it blank to start with none. Annotate the argument on `@workflow.run` and this becomes a form.'
              : 'This workflow declares an open input (any object). Type its one argument as JSON, or leave it blank to start with none.'}
          </span>
          <textarea
            className="min-h-[72px] w-full resize-y rounded border border-border bg-background p-2 font-mono text-[11.5px]"
            data-testid="workflow-input-json"
            spellCheck={false}
            value={draft.text}
            onChange={(e) => onDraft(setInputJson(draft, e.target.value))}
          />
        </label>
      )}

      {blocked && (
        <p
          className="m-0 mt-2 rounded border border-amber-500/40 bg-amber-500/10 p-2 font-mono text-[11px] text-amber-600 dark:text-amber-400"
          data-testid="workflow-input-error"
        >
          {blocked}
        </p>
      )}
    </div>
  );
}
