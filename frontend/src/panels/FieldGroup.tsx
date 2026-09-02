/**
 * A field tree, drawn — all the way down.
 *
 * A NESTED OBJECT IS A NESTED GROUP AND AN ARRAY IS A REPEATABLE ROW. Both used to be a one-line
 * JSON paste, which put the operator back to hand-writing the very document the schema exists to
 * describe: a list of two ports typed as `[80,443]`, a nested `retry` typed as `{"tries":3}`, on the
 * one surface whose whole job is to teach the shape. Everything the reader knows (`schemaTree.ts`)
 * is drawn here, and nothing else is.
 *
 * THE CELL IS SHARED AND ONLY THE CONTAINER DIFFERS. Every leaf at every depth goes through
 * `FieldInput` — the same component the Actors table puts in a `<td>` and the Workflows form puts in
 * a `<label>` — so a `<select>` cannot grow a third state on one page only, and a closed set nested
 * three deep offers exactly the members a top-level one does.
 *
 * THREE ANSWERS STAY THREE, AT EVERY DEPTH. A group that declares no fields says `no fields`; a leaf
 * whose author declared no type says `not declared` and names the fix; a leaf that declares an open
 * object says THAT instead, because one of those is a fixable omission and the other is a choice.
 * Collapsing any two of them tells the reader something false about the actor they are calling.
 *
 * PROPS IN, MARKUP OUT — no state, no fetches, nothing imported that reaches xterm or CodeMirror.
 * That is what lets `fieldGroup.render.test.ts` draw every state of it in node with no jsdom, which
 * is the same split (and the same reason) as `MethodCallPanes` and `WorkflowInputForm`.
 */

import { Plus, X } from 'lucide-react';
import { FieldInput } from './FieldInput';
import { elementAt, whyJson, type FieldNode } from './schemaTree';
import { addRow, removeRow, rowCount, setPath, type FieldValues } from './formFields';
import { Button } from '@/components/ui/button';

/**
 * How a level below its parent is set apart.
 *
 * A RULE DOWN THE LEFT, NOT AN INDENT PER LEVEL. Five levels of padding is a wall the operator reads
 * sideways; one hairline and a small inset says "inside this" at whatever depth it appears, and the
 * inputs stay lined up where the eye already is.
 */
const INSIDE = 'border-l border-border/60 pl-2.5';

/** How the two nudges are worded. Different sentences because only one of them is fixable by the
 *  person reading it — an omitted annotation is one type away from a form. */
const NUDGE = {
  undeclared: 'not declared — type it as JSON. A type annotation here turns it into fields.',
  'no-fields': 'declares an open shape (any object) — type it as JSON.',
} as const;

export function FieldGroup({
  nodes,
  values,
  onValues,
  testPrefix,
}: {
  nodes: readonly FieldNode[];
  /** The whole record, keyed by path — a group draws its own leaves out of the same flat map. */
  values: FieldValues;
  onValues(next: FieldValues): void;
  /** What every `data-testid` here starts with; a cell's is `${testPrefix}-${path}`, which is why
   *  the flat form's ids (`workflow-input-field-dataset`) survived the recursion unchanged. */
  testPrefix: string;
}): JSX.Element {
  return (
    <div className="flex flex-col gap-2">
      {nodes.map((node) => (
        <FieldCell
          key={node.path}
          node={node}
          values={values}
          onValues={onValues}
          testPrefix={testPrefix}
        />
      ))}
    </div>
  );
}

function FieldCell({
  node,
  values,
  onValues,
  testPrefix,
}: {
  node: FieldNode;
  values: FieldValues;
  onValues(next: FieldValues): void;
  testPrefix: string;
}): JSX.Element {
  if (node.kind === 'group') {
    return (
      <div data-testid={`${testPrefix}-group-${node.path}`}>
        <FieldLabel node={node} />
        {(node.children ?? []).length === 0 ? (
          // NOT AN EMPTY FORM. An author who declared an object with no properties said something,
          // and it is not the same thing as an author who declared nothing at all.
          <div
            className="mt-0.5 text-[10.5px] italic text-muted-foreground"
            data-testid={`${testPrefix}-nofields-${node.path}`}
          >
            no fields
          </div>
        ) : (
          <div className={`mt-1 ${INSIDE}`}>
            <FieldGroup
              nodes={node.children ?? []}
              values={values}
              onValues={onValues}
              testPrefix={testPrefix}
            />
          </div>
        )}
      </div>
    );
  }

  if (node.kind === 'list') {
    return (
      <ListRows node={node} values={values} onValues={onValues} testPrefix={testPrefix} />
    );
  }

  const why = whyJson(node);
  return (
    <label className="flex flex-col gap-0.5 text-[11px] text-muted-foreground">
      <FieldLabel node={node} />
      <FieldInput
        field={node}
        className="rounded border border-border bg-background px-1.5 py-1 font-mono text-[11.5px]"
        testId={`${testPrefix}-${node.path}`}
        value={values[node.path] ?? ''}
        onChange={(next) => onValues(setPath(values, node.path, next))}
      />
      {why && (
        <span className="text-[10px] italic" data-testid={`${testPrefix}-why-${node.path}`}>
          {NUDGE[why]}
        </span>
      )}
    </label>
  );
}

/**
 * One array: its rows, and the two controls that decide how many there are.
 *
 * THE LENGTH IS THE OPERATOR'S. A list opens with no rows — a row nobody asked for is a key nobody
 * asked for — and `add` is how the second port stops being a bracket somebody types by hand.
 */
function ListRows({
  node,
  values,
  onValues,
  testPrefix,
}: {
  node: FieldNode;
  values: FieldValues;
  onValues(next: FieldValues): void;
  testPrefix: string;
}): JSX.Element {
  const rows = rowCount(values, node.path);
  return (
    <div data-testid={`${testPrefix}-list-${node.path}`}>
      <FieldLabel node={node} />
      {rows === 0 ? (
        // AN EMPTY LIST IS A STATE, NOT A GAP. Said out loud so a form with nothing under a heading
        // reads as "none yet" rather than as a renderer that failed.
        <div
          className="mt-0.5 text-[10.5px] italic text-muted-foreground"
          data-testid={`${testPrefix}-empty-${node.path}`}
        >
          no items yet
        </div>
      ) : (
        <div className={`mt-1 flex flex-col gap-1.5 ${INSIDE}`}>
          {Array.from({ length: rows }, (_, i) => (
            <ListRow
              key={i}
              row={elementAt(node, i)}
              index={i}
              values={values}
              onValues={onValues}
              onRemove={() => onValues(removeRow(node, values, i))}
              testPrefix={testPrefix}
            />
          ))}
        </div>
      )}
      <Button
        variant="ghost"
        size="sm"
        className="mt-1 h-6 px-1.5 text-[10.5px]"
        data-testid={`${testPrefix}-add-${node.path}`}
        title={`add one more ${node.name || 'item'}`}
        onClick={() => onValues(addRow(node, values))}
      >
        <Plus size={11} className="mr-1" />
        add {node.name || 'item'}
      </Button>
    </div>
  );
}

/** One element: a single input when the element is a scalar, the element schema's own fields when
 *  it is an object. Both keep the index beside them, because position is a row's only identity. */
function ListRow({
  row,
  index,
  values,
  onValues,
  onRemove,
  testPrefix,
}: {
  row: FieldNode;
  index: number;
  values: FieldValues;
  onValues(next: FieldValues): void;
  onRemove(): void;
  testPrefix: string;
}): JSX.Element {
  const why = row.kind === 'leaf' ? whyJson(row) : null;
  return (
    <div className="flex items-start gap-1.5" data-testid={`${testPrefix}-row-${row.path}`}>
      <span className="mt-1.5 w-[16px] shrink-0 font-mono text-[9.5px] tabular-nums text-muted-foreground">
        {index + 1}
      </span>
      <div className="min-w-0 flex-1">
        {row.kind === 'group' ? (
          (row.children ?? []).length === 0 ? (
            <div
              className="text-[10.5px] italic text-muted-foreground"
              data-testid={`${testPrefix}-nofields-${row.path}`}
            >
              no fields
            </div>
          ) : (
            <FieldGroup
              nodes={row.children ?? []}
              values={values}
              onValues={onValues}
              testPrefix={testPrefix}
            />
          )
        ) : (
          <>
            <FieldInput
              field={row}
              className="w-full rounded border border-border bg-background px-1.5 py-1 font-mono text-[11.5px]"
              testId={`${testPrefix}-${row.path}`}
              value={values[row.path] ?? ''}
              onChange={(next) => onValues(setPath(values, row.path, next))}
            />
            {why && (
              <span
                className="text-[10px] italic text-muted-foreground"
                data-testid={`${testPrefix}-why-${row.path}`}
              >
                {NUDGE[why]}
              </span>
            )}
          </>
        )}
      </div>
      <Button
        variant="ghost"
        size="sm"
        className="h-6 shrink-0 px-1"
        data-testid={`${testPrefix}-remove-${row.path}`}
        title="drop this one"
        onClick={onRemove}
      >
        <X size={11} />
      </Button>
    </div>
  );
}

/** The name, the type and whether it is required — the same three the contract table above the form
 *  shows, in the same words, because they are one reading rendered twice. */
function FieldLabel({ node }: { node: FieldNode }): JSX.Element {
  return (
    <span className="flex items-baseline gap-1.5 text-[11px] text-muted-foreground">
      <span className="font-mono text-foreground">{node.name}</span>
      <span className="font-mono text-[9.5px]">{node.type}</span>
      {node.required && <span className="text-[9.5px] text-amber-500">required</span>}
    </span>
  );
}

