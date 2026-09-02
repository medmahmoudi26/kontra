/**
 * One declared field, collected — a list where the schema closed the set, a box where it did not.
 *
 * BOTH SURFACES COLLECT THROUGH THIS ONE COMPONENT, for the reason `formFields.ts` gives about the
 * coercion it pairs with: the Actors page builds a Batch (a row per Unit) and the Workflows page
 * builds one argument, and the CONTAINER differs while the cell does not. A `<select>` written
 * twice is a `<select>` that grows a third state on one page only.
 *
 * A CLOSED SET IS A LIST, NOT A HINT. `status: "open" | "closed"` typed into a free-text box fails
 * at the far end of a `kontra workflow serve` minutes later, in a tmux pane — the exact failure
 * `coerceValues` exists to pull forward. Offering the members is the same fix one step earlier: the
 * value cannot be wrong because the wrong values were never on the screen.
 *
 * NO DOM AT MODULE LOAD — types and markup only, nothing that reaches xterm or CodeMirror, so the
 * render suites in node can draw every state of it.
 */

import type { SchemaField } from './MethodContract';

export function FieldInput({
  field,
  value,
  onChange,
  testId,
  className = 'w-full rounded border border-border bg-background px-1.5 py-1 font-mono text-[11.5px]',
}: {
  field: SchemaField;
  value: string;
  onChange(next: string): void;
  testId: string;
  className?: string;
}): JSX.Element {
  if (!field.enum) {
    return (
      <input
        className={className}
        data-testid={testId}
        spellCheck={false}
        value={value}
        onChange={(e) => onChange(e.target.value)}
      />
    );
  }

  // A NULLABLE ENUM KEEPS ITS `null`. The text box had one way to say "explicitly nothing" that was
  // not "leave the key out" (ADR-less but load-bearing — see `coerceField`), and a list that dropped
  // it would take a value away from the operator rather than making one easier to reach.
  const nullable = field.type.split('|').some((m) => m.trim() === 'null');
  // A VALUE THAT IS NOT A MEMBER STAYS ON THE SCREEN. A draft carried over from an older version of
  // this Actor can hold one, and a `<select>` that simply did not list it would silently re-answer
  // the question as its first option — a changed Batch nobody edited. Listing it keeps the refusal
  // where the operator can read it, in `coerceValues`.
  const stray = value !== '' && value !== 'null' && !field.enum.includes(value);

  return (
    <select
      className={className}
      data-testid={testId}
      value={value}
      onChange={(e) => onChange(e.target.value)}
    >
      {/* Blank is the ABSENT KEY, which is what a required field being blank refuses on. The two
          labels are different facts: one is a choice not yet made, the other is a key left out. */}
      <option value="">{field.required ? 'choose…' : '— not set —'}</option>
      {field.enum.map((member) => (
        <option key={member} value={member}>
          {member}
        </option>
      ))}
      {nullable && <option value="null">null</option>}
      {stray && (
        <option value={value}>{value} — not a declared choice</option>
      )}
    </select>
  );
}
