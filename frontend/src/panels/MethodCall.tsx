/**
 * Calling a Method: the half that talks to the server.
 *
 * TWO CALLS, AND ONLY ONE OF THEM STARTS ANYTHING. `POST /api/sources/actor/:id/caller` returns
 * SOURCE — the caller shown read-only beside the button, regenerated as the form changes;
 * `POST /api/sources/actor/:id/probe` starts the one-shot workflow that makes the call (ADR 0033),
 * and `GET /api/probes/:runId` reads its answer back while it runs. What went with ADR 0033 §6 is
 * the third call this component used to make: the write into a folder.
 *
 * THE GENERATION IS AUTOMATIC NOW, and that is not a convenience. It used to be a button, because
 * a generated file that outlived its form was a file about a different Batch — the danger being
 * that you could then SAVE it. There is no save, so the only remaining risk of a stale block is
 * teaching the wrong thing, and the cheapest fix is for it never to be stale: the source is
 * regenerated whenever the Batch changes, debounced so typing a URL is not one request per
 * keystroke.
 *
 * IT NEVER SHOWS A CALLER FOR A BATCH IT DID NOT COME FROM. Every generation is stamped with the
 * draft it was made from, and a reply that arrives after the form moved on is discarded rather than
 * rendered — the same race a save button hid behind a click.
 *
 * THE MARKUP IS `MethodCallPanes.tsx`, for the reason the workbench split the same way: this file
 * holds state and fetches, that one is props in and markup out, and a node test with no jsdom can
 * draw every state of the second.
 *
 * THE FOLDER IS THE ACTOR'S. Both routes are keyed by the registered folder the Actor's code is in,
 * which is what pins the probe to one Actor and one version — so a card whose Actor was catalogued
 * by a worker on a droplet and never registered from disk has no call button, the same absence and
 * for the same reason as its missing edit button.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
  generateCaller,
  readProbe,
  runProbe,
  type GeneratedCaller,
  type ProbeReading,
  type ProbeStarted,
  type Source,
} from '../run/api';
import { useAppStore } from '../state/store';
import type { PollerReport } from '../run/workflowState';
import type { ActorOperation, CatalogActor } from '../types';
import { actorServeState, serveWords, workerReadings, dispatchTargets } from './actorWorkers';
import { draftFor, unitsOf, type BatchDraft } from './methodCall';
import { MethodCallPanel, type ServeReading } from './MethodCallPanes';

/**
 * How often a started probe is asked what it found.
 *
 * SLOWER THAN IT FEELS IT SHOULD BE, on purpose. A probe fails the way production fails — the
 * backing workflow retries `RunBatch` up to ten times with a two-minute heartbeat — so a probe
 * against a hanging `@actor.load` takes minutes, not seconds (ADR 0033's third consequence). A
 * one-second poll would spend a hundred requests learning nothing about a run whose own clock moves
 * in minutes.
 */
const PROBE_POLL_MS = 2_000;

/** How many consecutive failed reads before the panel stops asking and shows why. Some failures
 *  never recover (Temporal dropped the execution, the id is not a probe's), and a loop that retried
 *  those for as long as the tab was open would learn nothing and say nothing. */
const PROBE_POLL_TRIES = 5;

/** How long the form has to stop moving before the caller is regenerated. One keystroke in a URL
 *  field must not be one request. */
const REGENERATE_MS = 300;

export function MethodCall({
  actor,
  op,
  folder,
  pollers,
  polledAt,
  onClose,
}: {
  actor: CatalogActor;
  op: ActorOperation;
  /** The registered folder holding this Actor's code — the id both routes are keyed by. */
  folder: Source;
  /**
   * Who is polling this Actor's shared queue, as the page last read it — threaded in rather than
   * fetched here, because the grid already asks once per queue on a timer and a second poll from
   * this component would be a second answer to one question. `null` is `unknown`, which is NOT the
   * same as nothing serving.
   */
  pollers: PollerReport | null;
  /** When that report was read. Every age is measured against THIS, never against render time. */
  polledAt: number;
  onClose(): void;
}): JSX.Element {
  const [draft, setDraft] = useState<BatchDraft>(() => draftFor(op.input));
  const [caller, setCaller] = useState<GeneratedCaller | null>(null);
  const [started, setStarted] = useState<ProbeStarted | null>(null);
  const [reading, setReading] = useState<ProbeReading | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);
  /* THE OTHER HALF OF THE ROUND TRIP. A probe's whole value is that it leaves evidence rather than
     a screenful, and evidence you cannot reach is a claim. `openDataset` is the store's own
     navigation — the same one a Run's detail uses — so arriving from here is indistinguishable
     from arriving from anywhere else, scoped to this probe's Run. */
  const openDataset = useAppStore((s) => s.openDataset);

  const batch = useMemo(() => unitsOf(draft), [draft]);

  /** The Batch as one comparable string — what decides whether a generation is still current, and
   *  what the regeneration effect depends on. An object in the deps would regenerate on every
   *  render; the parse result is a new object each time even when nothing changed. */
  const batchKey = useMemo(
    () => ('error' in batch ? `!${batch.error}` : JSON.stringify(batch.units)),
    [batch]
  );
  /** The generation in flight, so a reply for a Batch that has since changed can be dropped. */
  const wanted = useRef(batchKey);
  wanted.current = batchKey;

  const onDraft = useCallback((next: BatchDraft) => {
    setDraft(next);
    setError(null);
  }, []);

  /**
   * REGENERATE THE CALLER WHENEVER THE BATCH CHANGES.
   *
   * A source block that outlives its form is a lie: the `BATCH` constant is the whole payload, so a
   * block built from the old Batch says the run did something it did not. It is generated by the
   * SERVER (`actorControl.ts:callerFor`) rather than composed here, which is what makes it the same
   * bytes the probe's own docstring claim refers to.
   *
   * A BATCH THAT DOES NOT PARSE GENERATES NOTHING, and the previous block is cleared with it — a
   * caller left on screen beside a red Batch error is the exact staleness this effect exists to
   * prevent.
   */
  useEffect(() => {
    if ('error' in batch) {
      setCaller(null);
      return;
    }
    const units = batch.units;
    const key = batchKey;
    const timer = setTimeout(() => {
      void generateCaller(folder.id, op.name, units)
        .then((got) => {
          if (wanted.current !== key) return;
          setCaller(got);
        })
        // THE SERVER'S SENTENCE. It refuses a name that is not a Method and a folder it does not
        // have registered, and both are things the reader can act on. It does NOT clear a started
        // run's report: failing to draw the teaching artefact is not a reason to hide the answer.
        .catch((err: unknown) => {
          if (wanted.current !== key) return;
          setCaller(null);
          setError(message(err));
        });
    }, REGENERATE_MS);
    return () => clearTimeout(timer);
  }, [batch, batchKey, folder.id, op.name]);

  /** Who can serve this Actor NOW — `actorWorkers.ts` is the authority, and a stale poller is not
   *  a smaller kind of serving. `dispatchTargets` is what the button may be offered against. */
  const serve = useMemo<ServeReading>(() => {
    const queue = pollers?.queue ?? `${actor.name}-${actor.version}`;
    const state = actorServeState(pollers, polledAt);
    return {
      state,
      words: serveWords(state, queue),
      queue,
      serving: dispatchTargets(workerReadings(pollers, polledAt)).length,
    };
  }, [actor.name, actor.version, pollers, polledAt]);

  const run = useCallback(() => {
    if ('error' in batch) return;
    setBusy('run');
    setError(null);
    // A NEW PROBE REPLACES THE OLD REPORT, rather than appending to it. Each press is its own Run
    // with its own id and its own Dataset (ADR 0033 §2 — unkeyed, fresh per probe), and a panel
    // accumulating them would be inviting the reader to compare two runs it is not showing.
    setStarted(null);
    setReading(null);
    void runProbe(folder.id, op.name, batch.units)
      .then(setStarted)
      // THE SERVER'S REFUSALS ARE THE USEFUL PART HERE: an Actor with no Nexus endpoint, a queue
      // whose only pollers are stale, a probe worker nobody started. Each names its own fix, so it
      // is shown verbatim rather than summarised.
      .catch((err: unknown) => setError(message(err)))
      .finally(() => setBusy(null));
  }, [batch, folder.id, op.name]);

  /**
   * ASK WHAT IT FOUND, until it has finished finding it.
   *
   * The Run's own status is Temporal's; what this reads is the workflow's RETURN VALUE, which is
   * where `results`, `isolated` and `done` live (ADR 0028 §4) and which no general run surface has
   * a shape for. Polling stops the moment the workflow is terminal, so a completed probe costs
   * nothing to leave on screen.
   */
  useEffect(() => {
    const runId = started?.runId;
    if (!runId) return;
    let live = true;
    let timer: ReturnType<typeof setTimeout>;
    let misses = 0;
    const tick = async (): Promise<void> => {
      try {
        const got = await readProbe(runId);
        if (!live) return;
        misses = 0;
        setReading(got);
        // TERMINAL IS TERMINAL. `RUNNING` is the only status worth asking again about — a failed,
        // timed-out, cancelled or terminated run has said everything it is going to say.
        if (got.status === 'RUNNING') timer = setTimeout(() => void tick(), PROBE_POLL_MS);
      } catch (err: unknown) {
        if (!live) return;
        /* A READ THAT FAILS IS NOT A RUN THAT FAILED, and must not be drawn as one: the run keeps
           going and the next tick may well answer, so the panel keeps showing what it last knew.
           BUT IT IS BOUNDED. Some failures never recover — Temporal dropped the execution after
           retention, the id is not a probe's — and a loop that retried those forever would ask
           every two seconds for as long as the tab was open, learning nothing and saying nothing.
           After a handful it stops and hands over the server's own sentence, which is the only
           thing that can tell those two apart. */
        misses += 1;
        if (misses < PROBE_POLL_TRIES) {
          timer = setTimeout(() => void tick(), PROBE_POLL_MS);
          return;
        }
        setReading({ runId, status: 'unreadable', failure: message(err) });
      }
    };
    void tick();
    return () => {
      live = false;
      clearTimeout(timer);
    };
  }, [started?.runId]);

  /**
   * COPY THE CALLER. The one thing that replaced the write (ADR 0033 §6), and it is deliberately
   * the whole of it: a file that lands on disk is code that can diverge from what actually ran.
   *
   * BEST-EFFORT AND SILENT ABOUT ITS OWN FAILURE — `navigator.clipboard` is unavailable on a
   * non-secure origin, and an operator who cannot copy still has a selectable block in front of
   * them. Reporting a clipboard permission as an error beside a Run they started would be the
   * loudest thing on the panel about the least important one.
   */
  const copy = useCallback(() => {
    if (!caller) return;
    void navigator.clipboard?.writeText(caller.source).then(
      () => setCopied(true),
      () => undefined
    );
  }, [caller]);

  // The confirmation is a moment, not a state: a button that says "Copied" forever is a button
  // that has stopped saying what it does.
  useEffect(() => {
    if (!copied) return;
    const timer = setTimeout(() => setCopied(false), 1_500);
    return () => clearTimeout(timer);
  }, [copied]);

  return (
    <MethodCallPanel
      actor={actor}
      op={op}
      draft={draft}
      batch={batch}
      onDraft={onDraft}
      caller={caller}
      serve={serve}
      busy={busy}
      error={error}
      started={started}
      reading={reading}
      onRun={run}
      onCopy={copy}
      copied={copied}
      onOpenDataset={() =>
        started && openDataset({ name: started.dataset, run: started.runId })
      }
      onClose={onClose}
    />
  );
}

function message(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
