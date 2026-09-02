/**
 * "Call this Method", drawn: the Batch form, the Run button, the caller it would take to do this
 * yourself, and what came back.
 *
 * IT CALLS THE METHOD (ADR 0033). This panel used to write a file and say so in words, because
 * there was no server-side dispatch to collect a Batch for. There is now: a kontra-owned one-shot
 * workflow performs exactly ONE Method call over this Batch, through the same Nexus operation
 * production uses, and writes into an untagged Dataset. So the button does the thing it says.
 *
 * AND IT IS STILL NOT THE INTERPRETER, which is a COUNT rather than a promise: one Actor, one
 * version, one Method, one Batch. There is no control on this panel that adds a second Method, a
 * branch, a loop or a fan-out width, because the request has no field for one (ADR 0033 §1). Two
 * Methods is a topology and belongs in the operator's own workflow — which is exactly what the
 * source block below the button is for.
 *
 * THE CALLER SURVIVES AS A READ-ONLY ARTEFACT (§6). It is regenerated as the form changes — a
 * source block that outlives its form is a lie — and it is what the probe actually runs, not an
 * illustration of it. What went is the errand: no save target, no folder shelf, no write.
 *
 * FOUR THINGS THIS PANEL MUST NOT COLLAPSE, and each one is a bug it has been fixed for:
 *
 *   • A STALE WORKER IS NOT A WORKER. Temporal lists a poller for about five minutes after it
 *     stops, so a Run offered against a stale queue sits on a queue nobody drains and reads as
 *     slow. `actorWorkers.ts` is the authority; the button is disabled with the reason in it.
 *   • UNKNOWN IS NOT NO. Temporal could not be asked is a missing answer, not a negative, and the
 *     button says which.
 *   • A DROP IS NOT AN EMPTY RESULT. `probeVerdict` owns that distinction — see it.
 *   • A METHOD THAT DECLARES NOTHING SAYS SO AND NAMES THE FIX, rather than offering an empty form
 *     that claims the Method takes nothing.
 *
 * PROPS IN, MARKUP OUT — no fetches, no state, and nothing imported that reaches xterm. That is
 * what lets `methodCall.render.test.ts` draw every state of it in node with no jsdom;
 * `MethodCall.tsx` is the half that talks to the server.
 */

import { Fragment } from 'react';
import { FieldGroup } from './FieldGroup';
import { FieldInput } from './FieldInput';
import {
  ArrowLeft,
  ChevronDown,
  ChevronRight,
  Copy,
  Database,
  Play,
  Plus,
  X,
} from 'lucide-react';
import type { GeneratedCaller, ProbeReading, ProbeStarted } from '../run/api';
import type { ActorOperation, CatalogActor } from '../types';
import type { ActorServeState, ServeWords } from './actorWorkers';
import {
  addUnit,
  leafFields,
  nestedFields,
  probeVerdict,
  removeUnit,
  setCell,
  setJson,
  setUnitValues,
  toggleUnit,
  type BatchDraft,
  type BatchResult,
} from './methodCall';
import { Button } from '@/components/ui/button';

/** Who can serve this Actor right now — `actorWorkers.ts`'s reading, threaded in whole rather
 *  than as a boolean, because `unknown` and `registered` are different sentences. */
export interface ServeReading {
  state: ActorServeState;
  words: ServeWords;
  queue: string;
  /** How many workers are actually polling NOW. Stale ones are not in here. */
  serving: number;
}

export function MethodCallPanel({
  actor,
  op,
  draft,
  batch,
  onDraft,
  caller,
  serve,
  busy,
  error,
  started,
  reading,
  onRun,
  onCopy,
  copied,
  onOpenDataset,
  onClose,
}: {
  actor: CatalogActor;
  op: ActorOperation;
  draft: BatchDraft;
  /** The Batch as it currently parses — or the sentence saying why it does not. */
  batch: BatchResult;
  onDraft(next: BatchDraft): void;
  /** The caller workflow that makes this call, regenerated as the form changes. Read-only. */
  caller: GeneratedCaller | null;
  serve: ServeReading;
  busy: string | null;
  error: string | null;
  /** What the run answered when it was started: the Run id, the Dataset, the endpoint. */
  started: ProbeStarted | null;
  /** What it has answered since — status while it runs, counts when it returns. */
  reading: ProbeReading | null;
  onRun(): void;
  onCopy(): void;
  /** True for a moment after a copy, so the button can confirm it did something. */
  copied: boolean;
  /** Go to the Datasets surface, scoped to this probe's Run. */
  onOpenDataset(): void;
  onClose(): void;
}): JSX.Element {
  const blocked = 'error' in batch ? batch.error : null;
  // THE BUTTON IS DISABLED FOR EXACTLY ONE REASON AT A TIME, and it says which. A control that is
  // simply grey is a control an operator retries.
  const stopper = runStopper(blocked, serve);

  return (
    <main className="flex min-h-0 min-w-0 flex-1 flex-col overflow-y-auto" data-testid="method-call">
      <header className="shrink-0 border-b border-border px-4 py-3">
        <Button
          variant="ghost"
          size="sm"
          className="h-6 px-1.5 text-[10.5px]"
          data-testid="method-call-close"
          onClick={onClose}
        >
          <ArrowLeft size={11} className="mr-1" />
          Actors
        </Button>
        <div className="mt-1.5 flex flex-wrap items-baseline gap-2">
          <code className="font-mono text-[14px] font-semibold">
            {actor.name}.{op.name}()
          </code>
          <span className="font-mono text-[10px] text-muted-foreground">v{actor.version}</span>
        </div>
        {/* THE ANSWER TO "WHAT WILL THIS DO", before the button is pressed — and it is a different
            answer than it used to be. One call, one Batch, and evidence rather than a screenful. */}
        <p className="m-0 mt-1.5 max-w-[62ch] text-[11.5px] leading-snug text-muted-foreground">
          This <strong className="font-semibold">calls the Method</strong> — one Actor, one version,
          one Method, one Batch — through the same dispatch a caller&rsquo;s workflow uses. The
          results land in an untagged Dataset you can query. Chaining a second Method is your own
          workflow&rsquo;s job, and the code below is where it starts.
        </p>
        {op.description && (
          <p className="m-0 mt-1.5 max-w-[62ch] text-[11.5px] leading-snug">{op.description}</p>
        )}
      </header>

      <section className="shrink-0 border-b border-border px-4 py-3">
        <h2 className="m-0 mb-2 flex flex-wrap items-baseline gap-2 text-[11px] uppercase tracking-wide text-muted-foreground">
          Batch
          <span className="text-[10.5px] normal-case tracking-normal">
            {draft.kind === 'fields'
              ? 'one row per Unit, from this Method’s declared input'
              : 'typed by hand — see below'}
          </span>
        </h2>

        {/* A METHOD THAT DECLARES NOTHING SAYS SO AND NAMES THE FIX. A form derived from nothing
            would be an empty box claiming the Method takes nothing, which is the one reading this
            page exists to prevent. `examples/go/dnsfacts` and `examples/go/nscheck` are real Actors
            in this state, so the sentence names both SDKs' way out rather than a general wish. */}
        {draft.kind === 'json' && (
          <p
            className="m-0 mb-2 max-w-[70ch] rounded border border-border bg-muted/30 p-2 text-[11px] leading-snug text-muted-foreground"
            data-testid="method-call-no-schema"
          >
            <strong className="font-semibold text-foreground">
              {actor.name}.{op.name}() declares no input schema
            </strong>
            , so there are no fields to draw and this is a Batch typed by hand — a list of Units, as
            JSON. It dispatches exactly the same way. To get a form: annotate the Method&rsquo;s
            input type in Python (<code className="font-mono">async def {op.name}(self, batch: list[Target], …)</code>
            ), or declare it in Go with <code className="font-mono">.Takes(Target{'{}'})</code> — then
            serve the Actor again and its worker registers the schema.
          </p>
        )}

        <BatchFields draft={draft} onDraft={onDraft} />

        {blocked && (
          <p
            className="m-0 mt-2 rounded border border-amber-500/40 bg-amber-500/10 p-2 font-mono text-[11px] text-amber-600 dark:text-amber-400"
            data-testid="method-call-batch-error"
          >
            {blocked}
          </p>
        )}

        <div className="mt-2.5 flex flex-wrap items-center gap-2.5">
          <Button
            size="sm"
            className="h-7 px-2.5 text-[11.5px]"
            data-testid="method-call-run"
            disabled={busy !== null || blocked !== null || stopper !== null}
            title={
              stopper ??
              `dispatch ${actor.name}.${op.name}() over this Batch, through ${serve.queue}`
            }
            onClick={onRun}
          >
            <Play size={13} className="mr-1.5" />
            {busy === 'run' ? 'Calling…' : `Call ${op.name}()`}
          </Button>
          <span className="text-[11px] text-muted-foreground">
            {'error' in batch
              ? 'fix the Batch first'
              : `${batch.units.length} ${batch.units.length === 1 ? 'Unit' : 'Units'}`}
          </span>
          {/* WHO CAN ACTUALLY RUN IT. Registered, serving, stale and unknown are four different
              facts (`actorWorkers.ts`), and the one that matters here is that a stale poller is not
              a smaller kind of serving — a dispatch aimed at one reads as a slow run forever. */}
          <span
            className={`rounded px-1.5 py-0.5 font-mono text-[10px] ${serveTone(serve.state)}`}
            data-testid="method-call-serve"
            title={serve.words.title}
          >
            {serve.words.label}
            {serve.state === 'serving' && serve.serving > 1 ? ` · ${serve.serving} workers` : ''}
          </span>
        </div>

        {stopper && (
          <p
            className="m-0 mt-2 max-w-[70ch] text-[10.5px] leading-snug text-amber-600 dark:text-amber-400"
            data-testid="method-call-blocked"
          >
            {stopper}
          </p>
        )}

        {/* PARAMS ARE RUN-WIDE AND THE PROBE PASSES NONE. An Actor that declares them and is called
            without them fails inside the Method, so the omission is named here rather than
            discovered there — and the caller below is where they would be added. */}
        {op.params && (
          <p className="m-0 mt-2 text-[10.5px] leading-snug text-muted-foreground">
            This Actor declares run-wide <code className="font-mono">params</code>. A probe passes
            none — add them to the dispatch as{' '}
            <code className="font-mono">params=&#123;…&#125;</code> in your own caller if it needs
            them.
          </p>
        )}
      </section>

      {error && (
        <p
          className="m-4 mb-0 shrink-0 whitespace-pre-wrap rounded border border-destructive/40 bg-destructive/10 p-2 font-mono text-[11.5px] text-destructive"
          data-testid="method-call-error"
        >
          {error}
        </p>
      )}

      {started && (
        <ProbeRun started={started} reading={reading} onOpenDataset={onOpenDataset} />
      )}

      {caller && (
        <section className="min-h-0 px-4 py-3" data-testid="method-call-result">
          <h2 className="m-0 mb-2 flex flex-wrap items-baseline gap-2 text-[11px] uppercase tracking-wide text-muted-foreground">
            the caller that makes this call
            <span className="text-[10.5px] normal-case tracking-normal">
              read-only — this is the code the button runs, with your Batch as the{' '}
              <code className="font-mono">BATCH</code> constant
            </span>
            <Button
              variant="ghost"
              size="sm"
              className="ml-auto h-6 px-1.5 text-[10.5px]"
              data-testid="method-call-copy"
              title="copy it — own the dispatch when you want a loop, a second Method, or a retry of the drops"
              onClick={onCopy}
            >
              <Copy size={11} className="mr-1" />
              {copied ? 'Copied' : 'Copy'}
            </Button>
          </h2>
          <pre
            className="m-0 max-h-[46vh] overflow-auto rounded border border-border bg-muted/40 p-2.5 font-mono text-[11px] leading-snug"
            data-testid="method-call-source"
          >
            {caller.source}
          </pre>
          {/* THE ERRAND IS GONE AND THE TEACHING IS NOT (ADR 0033 §6). There is no save target and
              no write: a file on disk can diverge from what actually ran, and this one now always
              has something to diverge from. The commands to serve a copy are in its own docstring. */}
          <p className="m-0 mt-1.5 max-w-[70ch] text-[10.5px] leading-snug text-muted-foreground">
            Nothing is written anywhere. Copy it into a folder of your own when you want more than
            one call — a loop, a second Method, a retry of what was dropped — and its docstring
            carries the two commands that serve and start it.
          </p>
        </section>
      )}
    </main>
  );
}

/**
 * The Run, once one has been started: what it is, and what it answered.
 *
 * IT IS AN ORDINARY RUN (ADR 0033's first consequence) — there is no "probe run" kind, so the id
 * shown here is the one the Runs surface lists, with a Temporal history and a two-dimensional
 * status like any other. What this draws is the one thing no general surface has a shape for: the
 * Method call's own `(results, dropped)`.
 */
function ProbeRun({
  started,
  reading,
  onOpenDataset,
}: {
  started: ProbeStarted;
  reading: ProbeReading | null;
  onOpenDataset(): void;
}): JSX.Element {
  const result = reading?.result;
  const verdict = result ? probeVerdict(result) : null;
  return (
    <section className="shrink-0 border-b border-border px-4 py-3" data-testid="method-call-run-result">
      <h2 className="m-0 mb-1.5 flex flex-wrap items-baseline gap-2 text-[11px] uppercase tracking-wide text-muted-foreground">
        {reading?.status ? reading.status.toLowerCase() : 'started'}
        <code className="font-mono text-[10.5px] normal-case tracking-normal" data-testid="method-call-run-id">
          {started.runId}
        </code>
      </h2>

      {verdict ? (
        <>
          <p
            className={`m-0 rounded border p-2 text-[11.5px] leading-snug ${verdictTone(verdict.tone)}`}
            data-testid={`method-call-verdict-${verdict.tone}`}
          >
            <strong className="font-semibold">{verdict.headline}</strong>
            <span className="ml-1.5">{verdict.detail}</span>
          </p>
          {/* THE COUNTS BESIDE THE SENTENCE, always all three. The sentence names the finding; the
              numbers are what keep it honest when a reader disagrees with the wording. */}
          <dl
            className="m-0 mt-1.5 flex flex-wrap gap-x-4 gap-y-1 font-mono text-[10.5px]"
            data-testid="method-call-counts"
          >
            <Count label="sent" value={result!.units} />
            <Count label="rows" value={result!.results} />
            <Count label="isolated" value={result!.isolated} />
            <Count label="covered" value={result!.done ? 'yes' : 'no'} />
            {result!.machine && <Count label="machine" value={result!.machine} />}
          </dl>
        </>
      ) : (
        /* A FAILED PROBE IS NOT A SLOW ONE, and drawing them alike is the failure this whole panel
           is careful about. A run that ended with no counts and no sentence reads exactly like a
           run still going — so when there is a reason, it is the whole of what is said here. */
        reading?.failure ? (
          <p
            className="m-0 whitespace-pre-wrap rounded border border-destructive/40 bg-destructive/10 p-2 font-mono text-[11.5px] text-destructive"
            data-testid="method-call-failed"
          >
            {reading.failure}
          </p>
        ) : (
          <p className="m-0 text-[11.5px] leading-snug text-muted-foreground" data-testid="method-call-running">
            {started.units} {started.units === 1 ? 'Unit' : 'Units'} dispatched to{' '}
            <code className="font-mono">{started.endpoint}</code>. A probe fails the way production
            fails — the dispatch retries with a two-minute heartbeat before it gives up — so this can
            take minutes against an Actor that is hanging, and that is the measurement rather than a
            stall.
          </p>
        )
      )}

      <p className="m-0 mt-1.5 flex flex-wrap items-baseline gap-x-1.5 text-[10.5px] leading-snug text-muted-foreground">
        Output:{' '}
        <code className="font-mono" data-testid="method-call-dataset">
          {started.dataset}
        </code>{' '}
        — an ordinary untagged Dataset, queryable from the Datasets page and swept on the ordinary
        TTL. Tag it there to keep it.
        {/* THE LINK ONLY ONCE THERE IS SOMETHING TO OPEN. The awaited form publishes when the call
            returns, so a Dataset offered while the probe is still running is a link to a name the
            lake has never heard of — which reads as a broken feature rather than as a run in
            progress. It carries the RUN, so the Datasets page opens scoped to this probe rather
            than to every run that ever wrote a Dataset by that name. */}
        {result && (
          <Button
            variant="ghost"
            size="sm"
            className="h-5 px-1.5 text-[10px]"
            data-testid="method-call-open-dataset"
            title={`open ${started.dataset} on the Datasets page, scoped to this run`}
            onClick={onOpenDataset}
          >
            <Database size={10} className="mr-1" />
            open it
          </Button>
        )}
      </p>
    </section>
  );
}

function Count({ label, value }: { label: string; value: number | string }): JSX.Element {
  return (
    <div className="flex items-baseline gap-1">
      <dt className="text-muted-foreground">{label}</dt>
      <dd className="m-0 tabular-nums">{value}</dd>
    </div>
  );
}

/**
 * The ONE reason the Run button is not pressable, or `null`.
 *
 * ORDER IS THE CONTRACT, and it is the order the operator can act in: a Batch that does not parse
 * is theirs to fix here and now, and everything else is about the cluster. A stale queue and an
 * unreachable Temporal are separate branches for the reason `actorWorkers.ts` keeps them separate —
 * "nothing is there" and "we could not ask" send a reader to two different places.
 */
export function runStopper(blocked: string | null, serve: ServeReading): string | null {
  /* A BATCH THAT DOES NOT PARSE DISABLES THE BUTTON WITHOUT A SECOND SENTENCE. It is already drawn,
     in red, immediately above — repeating it under the button would be the same fix said twice, and
     the reader would have to work out whether they were two problems. The DISABLING is not this
     function's job either way: the caller ors this with `blocked` (drawing them apart is the whole
     reason it is a separate value), which is what keeps an unparseable Batch from ever being sent. */
  if (blocked) return null;
  if (serve.state === 'serving') return null;
  if (serve.state === 'unknown') {
    return (
      `Temporal could not be asked who is polling ${serve.queue}, so nothing here knows whether ` +
      'this Actor can run. That is a missing answer, not a negative — a dispatch is not offered ' +
      'against it.'
    );
  }
  if (serve.state === 'stale') {
    return (
      `Temporal still lists a poller on ${serve.queue}, but none has polled recently — a worker ` +
      'that was killed stays listed for about five more minutes. A call would sit on this queue ' +
      'and look like a slow run. Serve this Actor again first.'
    );
  }
  return (
    `nothing is polling ${serve.queue}. Serve this Actor — the workbench on its card does it on ` +
    'this machine — and the call has somewhere to land.'
  );
}

function serveTone(state: ActorServeState): string {
  if (state === 'serving') return 'bg-emerald-500/10 text-emerald-600 dark:text-emerald-400';
  if (state === 'stale') return 'bg-amber-500/10 text-amber-600 dark:text-amber-400';
  if (state === 'unknown') return 'bg-muted text-muted-foreground';
  return 'bg-muted text-muted-foreground';
}

function verdictTone(tone: string): string {
  if (tone === 'dropped') return 'border-amber-500/40 bg-amber-500/10 text-amber-700 dark:text-amber-300';
  if (tone === 'partial') return 'border-amber-500/40 bg-amber-500/10 text-amber-700 dark:text-amber-300';
  if (tone === 'empty') return 'border-border bg-muted/40';
  return 'border-emerald-500/40 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300';
}

/**
 * The Batch itself: a table of Units when the Method declared its input, one JSON box when not.
 *
 * THE COLUMNS ARE THE LEAVES AND THE REST OPENS UNDER THE ROW. A Batch is a list, so the form is a
 * table an operator scans down — and a nested `retry` object or a repeatable `targets` list cannot be
 * a cell without making every row as tall as the deepest thing in the schema. So the scalars stay
 * columns, and a Unit with more than that gets a disclosure that opens the nested fields beneath it,
 * drawn by the SAME component the Workflows run form uses. A table nested inside a table was the
 * other option and it is unreadable at the second level.
 */
function BatchFields({
  draft,
  onDraft,
}: {
  draft: BatchDraft;
  onDraft(next: BatchDraft): void;
}): JSX.Element {
  if (draft.kind === 'json') {
    return (
      <textarea
        className="min-h-[104px] w-full resize-y rounded border border-border bg-background p-2 font-mono text-[11.5px]"
        data-testid="method-call-json"
        spellCheck={false}
        value={draft.text}
        onChange={(e) => onDraft(setJson(draft, e.target.value))}
      />
    );
  }
  const columns = leafFields(draft.fields);
  const nested = nestedFields(draft.fields);
  // The `#`, every leaf column, the disclosure when there is one, and the remove control.
  const span = 2 + columns.length + (nested.length > 0 ? 1 : 0);
  return (
    <div className="overflow-x-auto">
      <table className="w-full border-collapse text-left">
        <thead>
          <tr>
            <th className="w-[34px] pb-1 text-[9px] uppercase tracking-wider text-muted-foreground">
              #
            </th>
            {nested.length > 0 && <th className="w-[22px]" />}
            {columns.map((f) => (
              <th key={f.name} className="pb-1 pr-2 align-bottom">
                <div className="font-mono text-[11px] font-normal">{f.name}</div>
                <div className="font-mono text-[9.5px] font-normal text-muted-foreground">
                  {f.type}
                  {f.required && <span className="ml-1 text-amber-500">required</span>}
                </div>
              </th>
            ))}
            <th className="w-[28px]" />
          </tr>
        </thead>
        <tbody>
          {draft.units.map((unit, i) => (
            // Keyed by POSITION, which is the only identity a Unit has — it is a row in a list, and
            // the number beside it is that position.
            <Fragment key={i}>
              <tr data-testid={`method-call-unit-${i}`}>
                <td className="py-0.5 pr-1 font-mono text-[10px] tabular-nums text-muted-foreground">
                  {i + 1}
                </td>
                {nested.length > 0 && (
                  <td className="py-0.5">
                    <Button
                      variant="ghost"
                      size="sm"
                      className="h-6 px-0.5"
                      data-testid={`method-call-expand-${i}`}
                      title={
                        draft.expanded.includes(i)
                          ? 'close this Unit’s nested fields'
                          : `fill in this Unit’s ${nested.map((f) => f.name).join(', ')}`
                      }
                      onClick={() => onDraft(toggleUnit(draft, i))}
                    >
                      {draft.expanded.includes(i) ? (
                        <ChevronDown size={11} />
                      ) : (
                        <ChevronRight size={11} />
                      )}
                    </Button>
                  </td>
                )}
                {columns.map((f) => (
                  <td key={f.name} className="py-0.5 pr-2">
                    <FieldInput
                      field={f}
                      testId={`method-call-field-${i}-${f.path}`}
                      value={unit[f.path] ?? ''}
                      onChange={(next) => onDraft(setCell(draft, i, f.path, next))}
                    />
                  </td>
                ))}
                <td className="py-0.5">
                  <Button
                    variant="ghost"
                    size="sm"
                    className="h-6 px-1"
                    data-testid={`method-call-remove-${i}`}
                    title="drop this Unit from the Batch"
                    onClick={() => onDraft(removeUnit(draft, i))}
                  >
                    <X size={11} />
                  </Button>
                </td>
              </tr>
              {nested.length > 0 && draft.expanded.includes(i) && (
                <tr data-testid={`method-call-nested-${i}`}>
                  <td colSpan={span} className="pb-2 pl-6 pr-2 pt-1">
                    <FieldGroup
                      nodes={nested}
                      values={unit}
                      onValues={(next) => onDraft(setUnitValues(draft, i, next))}
                      testPrefix={`method-call-field-${i}`}
                    />
                  </td>
                </tr>
              )}
            </Fragment>
          ))}
        </tbody>
      </table>
      <Button
        variant="ghost"
        size="sm"
        className="mt-1 h-6 px-1.5 text-[10.5px]"
        data-testid="method-call-add-unit"
        title="a Batch is a list — this adds another Unit to it"
        onClick={() => onDraft(addUnit(draft))}
      >
        <Plus size={11} className="mr-1" />
        add Unit
      </Button>
    </div>
  );
}
