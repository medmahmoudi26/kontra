/**
 * What a Method IS, drawn once: its name, what its author said it does, and its three schemas.
 *
 * IT LIVED IN `ActorCard.tsx` AND TWO SURFACES NOW DRAW IT. The Actors page lists an Actor's
 * Methods on its card; Scratch's inspector lists the same Methods so an author can see which one a
 * node uses and change it. A second rendering would disagree eventually — and the one in Scratch is
 * the one nobody would notice going stale, because nobody opens a sketch to audit a field table.
 * So the row moved here and both import it.
 *
 * THE THREE ANSWERS ABOUT A SCHEMA STAY THREE. `not declared`, `no fields` and a table are
 * different facts — an actor that declares nothing (`examples/go/dnsfacts`) and one whose Method
 * takes nothing are not the same thing to a caller, and only one of them can be called with
 * anything. That distinction is the reason a field table is worth drawing at all, and it is why
 * {@link schemaFields} answers `null` rather than `[]` for an absent schema.
 *
 * AND AN UNDESCRIBED METHOD SAYS SO. Descriptions travel now (`209b0ad`): Python reads the
 * docstring's first paragraph, Go types the same sentence into `Does("…")`. Both SDKs omit the key
 * rather than sending `""`, so a Method with no description is an author who wrote none — a fixable
 * omission by the person reading the row, not a field the system lacks — and the row says THAT
 * rather than leaving a blank line where a sentence goes.
 *
 * NO DOM AT MODULE LOAD, deliberately, for the reason `ActorCard.tsx` records: the suites here run
 * in node with no jsdom, and anything that reaches xterm or CodeMirror on the way in fails a whole
 * test file with `ReferenceError: self is not defined` before a test runs. This module imports
 * lucide, the shared Button, types and the pure schema reader — nothing else — so both surfaces'
 * render tests can draw it.
 */

import { Play } from 'lucide-react';
import type { ActorOperation, JsonSchema } from '../types';
import { schemaFields } from './schemaTree';
import { Button } from '@/components/ui/button';

/**
 * One Method: what a caller types, what it is for, and what it moves.
 *
 * `expanded` is the FIELD TABLES only. The name, the description and the `takes → emits` summary
 * are always drawn, because an Actor holds many Methods (ADR 0023 §9) and each self-registers its
 * own signature — a row that folded away to a bare name would leave a reader to pick by memory.
 */
export function MethodRow({
  actor,
  op,
  expanded,
  onCall,
}: {
  actor: string;
  op: ActorOperation;
  expanded: boolean;
  /** Absent when this Actor's code is not on this disk — see where it is passed. */
  onCall?: () => void;
}): JSX.Element {
  return (
    <div className="flex gap-3 border-t border-border px-3.5 py-2">
      {/* What a caller literally types. A Batch in, a Batch out. */}
      <code
        className="w-[118px] shrink-0 truncate text-[11px]"
        title={`await ${actor}.${op.name}(batch)`}
      >
        {op.name}()
      </code>
      <div className="min-w-0 flex-1">
        {/* WHAT IT DOES, above what it takes. The catalog carried a name and two schemas, so this
            row could say `{host} → {addrs}` and never why anybody would call it — and an operator
            composing one Actor's Method into another's had nothing to compose from. The author
            wrote the answer in a docstring; it travels now (`internals/catalog.py:operations_of`,
            `registrar.Describe` on the Go side).

            THE ABSENCE IS DRAWN AS AN ABSENCE. An actor whose author wrote no docstring says so,
            because a missing line here is indistinguishable from a Method nobody thought worth
            describing — and one of those is fixable by the person reading. */}
        {op.description ? (
          <div className="text-[11px] leading-snug" data-testid={`method-doc-${actor}-${op.name}`}>
            {op.description}
          </div>
        ) : (
          <div className="text-[10.5px] italic text-muted-foreground/70">
            no description — add a docstring to this Method
          </div>
        )}
        <div className="truncate font-mono text-[10px] text-muted-foreground">
          {summarise(op.input)} → {summarise(op.output)}
        </div>
        {expanded && (
          <div className="mt-2 flex flex-col gap-1.5">
            <Fields label="takes" schema={op.input} />
            <Fields label="emits" schema={op.output} />
            {/* Params are ACTOR-level run-wide config, not a per-Method signature — shown only when
                the actor declares them, and labelled so it does not read as a third argument. */}
            {op.params && <Fields label="params (run-wide)" schema={op.params} />}
          </div>
        )}
      </div>
      {/* CALL CALLS IT (ADR 0033), and the word is finally the action. It used to hand over code
          instead, worded as a generator so nobody read it as "run it" — and it is worth recording
          that the wording was right for what the button then did. Opening the panel is not itself a
          dispatch: the Batch is collected first, and the Run button lives there, beside the caller
          this one used to be the only way to see. */}
      {onCall && (
        <Button
          variant="ghost"
          size="sm"
          className="h-6 shrink-0 self-start px-1.5 text-[10px]"
          data-testid={`method-call-${actor}-${op.name}`}
          title={`call ${actor}.${op.name}() over a Batch you type — one Method, one call, into a Dataset`}
          onClick={onCall}
        >
          <Play size={11} className="mr-1" />
          call
        </Button>
      )}
    </div>
  );
}

/**
 * A one-line summary of a schema for the collapsed row.
 *
 * `not declared` is its own answer and must never collapse into `{}`: the whole point of a field
 * table is the difference between a Method that takes nothing and a Method whose author declared
 * nothing.
 */
export function summarise(schema?: JsonSchema): string {
  const fields = schemaFields(schema);
  if (fields === null) return 'not declared';
  if (fields.length === 0) return '{}';
  const shown = fields.slice(0, 3).map((f) => f.name);
  return `{${shown.join(', ')}${fields.length > shown.length ? `, +${fields.length - shown.length}` : ''}}`;
}

/** One schema as a field table. */
function Fields({ label, schema }: { label: string; schema?: JsonSchema }) {
  const fields = schemaFields(schema);
  return (
    <div>
      <div className="font-mono text-[10px] uppercase tracking-wide text-muted-foreground">
        {label}
      </div>
      {fields === null ? (
        // NOT an empty table. "This Method declares no schema" and "this Method takes nothing"
        // are different facts, and only one of them means you can call it with anything.
        <div className="text-[11px] italic text-muted-foreground">not declared</div>
      ) : fields.length === 0 ? (
        <div className="text-[11px] italic text-muted-foreground">no fields</div>
      ) : (
        <ul className="m-0 list-none p-0">
          {fields.map((f) => (
            <li key={f.name} className="flex flex-wrap gap-2 font-mono text-[11px]">
              <span>{f.name}</span>
              <span className="text-muted-foreground">{f.type}</span>
              {f.required && <span className="text-[10px] text-amber-500">required</span>}
              {/* THE CHOICES, WHERE THE SET IS CLOSED. The form beside this table offers them as a
                  list, and a table that showed only `string` would describe a different Method than
                  the one being called — the drift this module exists to prevent. */}
              {f.enum && (
                <span className="text-[10px] text-muted-foreground">{f.enum.join(' | ')}</span>
              )}
              {f.default !== undefined && (
                <span className="text-[10px] text-muted-foreground">= {f.default}</span>
              )}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

/**
 * THE READER LIVES IN `schemaTree.ts` NOW, and these are its old names kept pointing at it.
 *
 * It moved because it stopped being one level deep: a nested object is a nested GROUP of fields and
 * an array is a repeatable ROW, and the format knowledge that reads them — Go inlining its nested
 * structs, Python spelling its own as `$ref` into `$defs` — is a module rather than a helper under a
 * component. Re-exported rather than relocated in every caller, because this file is where five
 * surfaces already import the flattening from and a rename would have been churn in all of them.
 *
 * WHAT THE TABLE ABOVE DRAWS IS THE TOP LEVEL OF THAT TREE — one reading, rendered at two depths, so
 * a form can never offer a field the contract beside it does not show.
 */
export {
  schemaFields,
  schemaTree,
  schemaEnum,
  schemaDefault,
  schemaType,
  elementAt,
  joinPath,
  whyJson,
  declaredValue,
} from './schemaTree';
export type { SchemaField, FieldNode, FieldKind } from './schemaTree';
