/**
 * `callerFor` — the caller snippet shown beside a Method's Run button.
 *
 * IT IS HERE BECAUSE THE CONSOLE RENDERS IT AND `actorControl.ts` CANNOT MOVE: that module reaches
 * `secrets/store`, `secrets/identity` and `workflowControl`, and a secret store has no business in
 * a browser bundle. This function takes strings and returns a string.
 *
 * THE SOURCE ON SCREEN IS THE SOURCE THAT RUNS, which is why it is shared rather than reimplemented
 * in the console: `methodCall.render.test.ts` asserts against THIS generator, so a second one would
 * let the panel show a snippet that no longer matches what the probe executes.
 */

/**
 * The caller workflow that dispatches one Method over a Batch — the ARTEFACT beside the button.
 *
 * IT IS NO LONGER AN ERRAND, AND IT IS NO LONGER THE DISPATCH (ADR 0033 §6). The Actors page calls
 * the Method now: a kontra-owned one-shot workflow performs exactly one Method call over the Batch
 * the form collected, through the same Nexus operation production uses (`control/orchestrator/src/probe.ts`,
 * `sdk/python/actorkit/probe.py`). What survives here is the half that TEACHES — the page shows this
 * source read-only beside the Run button, regenerating as the form changes, and there is nowhere to
 * write it to any more.
 *
 * WHICH IS WHAT MAKES IT HONEST RATHER THAN MERELY ILLUSTRATIVE: the probe runs the same three
 * lines. `catalog.actor(...)`, a callable handle, a destructured `(results, dropped)`, an optional
 * Dataset writer — not a reimplementation of them, so the code shown beside the button is the code
 * the button runs. An operator who wants to own the dispatch — a loop, a second Method, a retry of
 * the drops — copies this and serves it, and that is composition inside their own workflow (ADR
 * 0023 §16), which was always where it belonged.
 *
 * A NOTE ON A SENTENCE THAT USED TO BE HERE. This comment said "`POST /api/runs` returns 404 on
 * purpose". It has not since 2026-08-15: what ADR 0023 §12 removed was the route that took a GRAPH
 * and started the interpreter. The invariant that survived is narrower and still holds —
 * **the orchestrator starts workflows; it does not execute Batches.**
 *
 * THE DISPATCH IS `handle.<method>(batch)`, RETURNING `(results, dropped)` (ADR 0028 §4). The handle
 * is callable now — a Method name is an attribute on the `ActorHandle` `catalog.actor()` returns
 * (`ActorHandle.__getattr__` in `sdk/python/actorkit/catalog.py`), and awaiting the call hands back
 * a `(results, dropped)` tuple the caller must destructure. This is the SAME spelling the SDK
 * documents and every example uses, so the first file an operator meets teaches the surface the
 * rest of the repo speaks — and it works with or without an `async with`, because a one-shot call
 * on a bare handle is a load/run/close on the actor's shared queue.
 *
 * IT HAS DIED HERE BEFORE, ONE RENAME BEHIND THE SDK. This generator once emitted `handle.<method>`
 * against a handle that had NO `__getattr__` and every file raised `AttributeError` the moment the
 * worker ran it — generated fine, served fine, started fine, died in the worker. The fix then was
 * `handle.dispatch(batch, method="…")`, which the callable handle (ADR 0028) has since retired.
 * `actorControl.test.ts` no longer only parses the output: it EXECUTES it against the real SDK
 * handle, so a spelling the parser accepts but the worker rejects fails the test where it is cheap.
 *
 * A METHOD NAME THE GO SDK ALLOWS BUT PYTHON CANNOT SPELL rides `getattr`. `core.Registry.AddMethod`
 * takes any non-empty string, so `dns-facts` is a real catalogued Method — and `handle.dns-facts`
 * is a SyntaxError, not a slightly-wrong call. `getattr(handle, "dns-facts")(batch)` reaches it and
 * still goes through the same callable-handle path, so the tuple return is identical.
 *
 * THE CALLER NAMES WHERE OUTPUT GOES (ADR 0028 §2) when one is asked for. Given a Dataset name, the
 * caller publishes each returned Batch into that named Dataset once the call returns (per chunk, not
 * per push) — passed as the SECOND positional argument, `handle.<method>(batch, out)`, the same
 * shape the SDK and examples use. Omitted, the results stay an unnamed, chainable Batch and the
 * caller's `run` just reports the counts.
 */
/** Python's reserved words — an attribute cannot be one, so a Method so named rides `getattr`. */
const PY_KEYWORDS = new Set([
  'False', 'None', 'True', 'and', 'as', 'assert', 'async', 'await', 'break', 'class', 'continue',
  'def', 'del', 'elif', 'else', 'except', 'finally', 'for', 'from', 'global', 'if', 'import', 'in',
  'is', 'lambda', 'nonlocal', 'not', 'or', 'pass', 'raise', 'return', 'try', 'while', 'with',
  'yield',
]);

// The four helpers `callerFor` needs. Private to this module, as they were in actorControl.ts:
// they shape an identifier and a literal for the generated snippet and mean nothing on their own.
/** A Python identifier from an actor name: `crawl4ai-canon` → `crawl4ai_canon`. */
function safeIdent(name: string): string {
  const ident = name.replace(/[^A-Za-z0-9_]/g, '_');
  return /^[0-9]/.test(ident) ? `a_${ident}` : ident;
}

/** `probe` + `head` → `ProbeHead`. The class name `workflow start` is given. */
function className(actor: string, method: string): string {
  const camel = (s: string) =>
    s
      .split(/[^A-Za-z0-9]+/)
      .filter(Boolean)
      .map((part) => part.charAt(0).toUpperCase() + part.slice(1))
      .join('');
  const name = `${camel(actor)}${camel(method)}`;
  return /^[0-9]/.test(name) || name === '' ? `Call${name}` : name;
}

/**
 * The Batch as PYTHON SOURCE, not as JSON — the same layout, three different words.
 *
 * `JSON.stringify(units, null, 4)` was here, and the file it wrote parsed cleanly and then died at
 * import: JSON spells the three atoms `true`, `false` and `null`, and Python reads those as three
 * undefined names. So a Batch with one boolean in it — which is one checkbox on the form that feeds
 * this — generated a caller that raised `NameError: name 'true' is not defined` the first time a
 * worker loaded it, one `kontra workflow serve` and several minutes after the page said it had
 * handed over something runnable. `ast.parse` does not catch it (a bare `true` is a legal Name),
 * which is why `actorControl.test.ts` EVALUATES the constant instead of only parsing the file.
 *
 * Everything else is JSON's layout on purpose — four spaces, one entry per line, keys in
 * double quotes — because the module-level constant this feeds is indented for exactly that, and
 * because `JSON.stringify` on a string is already a valid Python string literal (the escapes it
 * emits are the ones Python reads).
 */
function pyLiteral(value: unknown, indent: number, depth = 0): string {
  const pad = ' '.repeat(indent * (depth + 1));
  const close = ' '.repeat(indent * depth);
  if (value === null || value === undefined) return 'None';
  if (typeof value === 'boolean') return value ? 'True' : 'False';
  // A non-finite number has no Python literal either (`Infinity` is one more undefined name), and
  // `JSON.stringify` turns it into `null` — which is the bug above wearing a different hat.
  if (typeof value === 'number') return Number.isFinite(value) ? JSON.stringify(value) : 'None';
  if (typeof value === 'string') return JSON.stringify(value);
  if (Array.isArray(value)) {
    if (value.length === 0) return '[]';
    return `[\n${value.map((v) => `${pad}${pyLiteral(v, indent, depth + 1)}`).join(',\n')}\n${close}]`;
  }
  if (typeof value === 'object') {
    const entries = Object.entries(value as Record<string, unknown>);
    if (entries.length === 0) return '{}';
    const rows = entries.map(([k, v]) => `${pad}${JSON.stringify(k)}: ${pyLiteral(v, indent, depth + 1)}`);
    return `{\n${rows.join(',\n')}\n${close}}`;
  }
  // A bigint or a symbol cannot arrive from a parsed request body; if one ever does, `None` is a
  // Batch entry an operator can see and fix, and a bigint's `toString` is not.
  return 'None';
}

/**
 * Whether a Method name can be spelled as a Python attribute — `handle.head` — or has to go
 * through `getattr(handle, "…")`.
 *
 * The Go SDK's `core.Registry.AddMethod` takes any non-empty string, so `dns-facts` is a Method a
 * worker really self-registers and `handle.dns-facts` is a SyntaxError the parser catches at import,
 * not a wrong call at runtime. A keyword (`class`, `for`) is the same trap wearing a legal-looking
 * name, so both are refused here and routed to `getattr`, which takes the name as a plain string.
 */
function pyAttr(method: string): boolean {
  if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(method)) return false;
  return !PY_KEYWORDS.has(method);
}

export function callerFor(
  actor: string,
  version: string,
  method: string,
  units: unknown,
  dataset?: string
): string {
  const handle = safeIdent(actor);
  const cls = className(actor, method);
  // A MODULE-LEVEL CONSTANT, not a literal inside the method. Embedding it in the body means
  // re-indenting a multi-line JSON blob to whatever column the statement sits at, and the result
  // parses while looking like a mistake. At module level the JSON's own indentation is correct as
  // printed, and the Batch is the first thing an operator sees when they open the file to edit it.
  const batch = pyLiteral(units ?? [], 4);
  // A NAME REACHES THE CODE AS AN ESCAPED LITERAL. The actor's name and version are read off
  // `actor.json` on the operator's own disk, which is a file nothing bounds, and interpolating one
  // raw meant a single `"` closed the string it sat in and left the rest of the line as source — in
  // a file the operator is about to run. In the DOCSTRING they are still prose, and a `"""` there
  // breaks the file loudly at the first line python reads; inside `catalog.actor(…)` the same
  // character was silent.
  const py = (s: string) => JSON.stringify(s);
  // `handle.head(batch)` when the Method name is a legal Python attribute, `getattr(handle,
  // "dns-facts")(batch)` when it is not — the latter reaches a Method the Go SDK registers that
  // Python cannot spell as an attribute at all. Either way it is one callable-handle call.
  const target = pyAttr(method) ? `${handle}.${method}` : `getattr(${handle}, ${py(method)})`;

  const named = (dataset ?? '').trim();
  // The dispatch line and its surrounding scope. Named: an open Dataset writer the caller hands
  // over as the second positional argument (ADR 0028 §2), so the rows publish under a name a query
  // can reach. Unnamed: the results stay a chainable Batch and nothing is materialized.
  const body = named
    ? `        async with catalog.dataset(${py(named)}).writer() as out:
            results, dropped = await ${target}(batch, out)`
    : `        results, dropped = await ${target}(batch)`;

  return `"""Dispatch ${actor}@${version}.${method}() over a Batch.

Shown by the Actors page beside its Run button — and this IS what that button runs (ADR 0033 §3):
the probe workflow makes the same call, on the same handle, through the same Nexus operation. So
copying this is not a second way to do it; it is the same way, in a file you own.

Own it when you want more than one call: a loop, a second Method, a retry of what was dropped.
That is composition inside YOUR workflow, which is where it belongs — the probe deliberately has
nowhere to put a second Method (ADR 0033 §1). Serve and start it with:

    kontra workflow serve workflow.py --tmux
    kontra workflow start workflow.py --wait

Both verbs take the FOLDER, never a typed queue: the queue is derived from this folder's content
(GitHub #15), so serve and start always agree and a pasted nickname cannot route your run wrong.
"""

from actorkit import catalog
from temporalio import workflow

${handle} = catalog.actor(${py(actor)}, ${py(version)})

# The Batch, as typed on the Actors page. Edit it here, or pass one to workflow start --input.
BATCH = ${batch}


@workflow.defn
class ${cls}:
    @workflow.run
    async def run(self, units: list | None = None) -> dict:
        batch = units if units is not None else BATCH
        # The handle is callable and a Method call returns (results, dropped) (ADR 0028 §4): you
        # cannot reach results without naming the drops, so ignoring them is a choice a reviewer
        # sees. len(dropped) costs no fetch; await dropped.rows() gets the Units back for a retry.
${body}
        return {"units": len(batch), "out": len(results), "dropped": len(dropped)}


if __name__ == "__main__":
    catalog.serve([${cls}])
`;
}
