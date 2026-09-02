/**
 * WHO CAN ACTUALLY SERVE THIS ACTOR, drawn — the three facts, plus the sessions behind them.
 *
 * WHAT IT IS FOR. The rest of the card is registration: a folder, a digest, the Methods a worker
 * once declared. All of that survives the process that made it, so a card without this strip says
 * "this Actor exists" in a way that reads as "this Actor works". It does not, and the difference is
 * a dispatch that sits on a queue nobody polls while Temporal reports the run as running — a
 * failure this repo has documented as indistinguishable from the outside from a slow run.
 *
 * THREE STATES, NEVER TWO. `actorWorkers.ts` owns the vocabulary and the reasoning; this file draws
 * it. The one rule that lives here is that STALE IS RENDERED, in full, as its own state — a stale
 * worker is not omitted and not shown as a duller serving one, because "there is a worker, it is
 * dead" is the finding, and an empty list would read as "nobody has served this yet".
 *
 * AND AN ACTOR WITH NOTHING SERVING IT SAYS SO IN WORDS. Every negative state on this strip is a
 * sentence naming the queue and what to do about it, never a blank list: an empty rectangle is
 * indistinguishable from a strip that failed to load, and that is the reading this page cannot
 * afford.
 *
 * NO DISPATCH BUTTON HERE, deliberately. `dispatchTargets` is the list a dispatch may be aimed at
 * and it is exported for the probe (issue 15) to consume; what this component owes that slice is a
 * surface where a stale worker is visibly not a target, before anything can aim at one.
 *
 * PROPS IN, MARKUP OUT, no fetch and no socket — the poll lives in `ActorsPage`. It is drawn by
 * `actorWorkers.render.test.ts` in node with no jsdom, which is only possible because nothing on
 * this path touches xterm at module load.
 */

import { chipState, STATE_CLASS, STATE_GLYPH } from './HealthChips';
import {
  actorHealthReading,
  actorServeState,
  actorSessions,
  dispatchTargets,
  serveWords,
  workerReadings,
  type ActorServeState,
  type WorkerReading,
} from './actorWorkers';
import { formatAge } from './widgets/format';
import type { PollerReport } from '../run/workflowState';
import type { Terminal } from './panelsClient';

/**
 * FOUR STATES, FOUR LOOKS — three of them the chips' own, and one that has to be added.
 *
 * `serving` and `stale` are `ok` and `bad`: something works, something is broken. `unknown` is the
 * chips' dashed, unfilled marker, which is how ADR 0020 says "this is not a reading" in a channel a
 * colour cannot fake.
 *
 * `registered` IS THE ONE THAT IS NOT RED, and that is a decision rather than an oversight. It is a
 * MEASURED negative — Temporal answered, and nothing is polling — but it is also the ordinary state
 * of every folder an operator has registered and not yet served, and a page that painted those red
 * would be a wall of alarms about nothing, which is how a red stops being read at all. The Workflows
 * page settled the same question the same way: `idle` there is muted, because it is most workflows
 * most of the time. So this is grey and SOLID: distinct from the greens and reds by hue, and from
 * `unknown` by fill, border style and glyph — a measurement that found nothing, not a missing one.
 */
const SERVE_CLASS: Record<ActorServeState, string> = {
  serving: STATE_CLASS.ok,
  stale: STATE_CLASS.bad,
  unknown: STATE_CLASS.unknown,
  registered: 'border-solid border-zinc-400/60 bg-zinc-500/10 text-zinc-600 dark:text-zinc-300',
};

/** Decorative, and marked so — the words already say the state. `○` is a measurement that found
 *  nothing, against `?` for a measurement that did not happen. */
const SERVE_GLYPH: Record<ActorServeState, string> = {
  serving: STATE_GLYPH.ok,
  stale: STATE_GLYPH.bad,
  unknown: STATE_GLYPH.unknown,
  registered: '○',
};

export interface ActorWorkersProps {
  /** The Actor's name — the test hooks and the sentences are keyed by it. */
  actor: string;
  /** The shared task queue, named in every sentence: an operator who has to check with
   *  `kontra workers list` needs the string, and the whole strip is about that one queue. */
  queue: string;
  /** What `/api/queues/:queue/pollers` answered. `null` means NOT ASKED YET — drawn as unknown,
   *  never as "nothing is serving", which is the same distinction the route goes to trouble to
   *  preserve on the wire. */
  report: PollerReport | null;
  /** When the report was read, so an age is measured against the reading and not against render
   *  time. Passed in rather than `Date.now()` here: a component that reads the clock itself cannot
   *  be drawn twice and compared. */
  now: number;
  /** The Monitor's panes for this Actor — where its worker is running, as opposed to who is
   *  polling. Both are needed; see `actorWorkers.actorSessions`. */
  machines: readonly Terminal[];
}

export function ActorWorkers({
  actor,
  queue,
  report,
  now,
  machines,
}: ActorWorkersProps): JSX.Element {
  const state = actorServeState(report, now);
  const words = serveWords(state, queue);
  const workers = workerReadings(report, now);
  const serving = dispatchTargets(workers);
  const stale = workers.filter((w) => w.state === 'stale');
  const health = chipState(actorHealthReading(machines));
  const sessions = actorSessions(machines);

  return (
    <section
      className="border-t border-border px-3.5 py-2.5"
      data-testid={`actor-workers-${actor}`}
    >
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-[9px] uppercase tracking-wider text-muted-foreground">workers</span>
        {/* THE STATE, in one word, with the whole reasoning on hover. `data-state` is the machine
            hook — the same convention `HealthChips` set: a spec asserts the state, never a class. */}
        <span
          data-testid={`actor-serving-${actor}`}
          data-state={state}
          title={words.title}
          className={`inline-flex items-center gap-1 rounded border px-1.5 py-0.5 font-mono text-[9.5px] ${SERVE_CLASS[state]}`}
        >
          <span aria-hidden="true">{SERVE_GLYPH[state]}</span>
          <span>{words.label}</span>
        </span>
        {/*
          HEALTH IS TRI-STATE AND `unknown` IS NOT `ok`. `chipState(null)` is the existing idiom for
          exactly this (ADR 0020): four channels of difference — hue, fill, border style and glyph —
          so "nobody looked" cannot be read as a paler "fine" at a glance. An Actor with no Machine
          in the Monitor's inventory has not been found healthy; nothing has looked at it.
        */}
        <span
          data-testid={`actor-health-${actor}`}
          data-state={health}
          title={
            health === 'ok'
              ? `every Machine running ${actor} was probed and its applicable signals are fine`
              : health === 'bad'
                ? `a Machine running ${actor} has a failing signal — open the Monitor for which`
                : machines.length === 0
                  ? `nothing is known about ${actor}'s health: the Monitor has no pane running it, so no probe has ever looked. NOT the same as healthy.`
                  : `${actor}'s health was not measured on every Machine running it — unknown, which is not the same as fine`
          }
          className={`inline-flex items-center gap-1 rounded border px-1.5 py-0.5 font-mono text-[9.5px] ${STATE_CLASS[health]}`}
        >
          <span aria-hidden="true">{STATE_GLYPH[health]}</span>
          <span>health: {health}</span>
        </span>
        {/* THE COUNTS, and stale is counted separately rather than folded into a total. "3 workers"
            over two live ones and a corpse is the sentence this strip exists to stop. */}
        {workers.length > 0 && (
          <span
            className="font-mono text-[9.5px] text-muted-foreground"
            data-testid={`actor-worker-count-${actor}`}
            title={`${serving.length} of ${workers.length} poller(s) on ${queue} polled within the freshness window`}
          >
            {serving.length} serving · {stale.length} stale
          </span>
        )}
      </div>

      {state === 'unknown' ? (
        // THE MISSING ANSWER, said as one. Drawn where the list would be, so the strip is never
        // blank — a blank one reads as "nothing is serving", which is the claim this state cannot
        // support.
        <p
          className="m-0 mt-1.5 text-[10.5px] italic text-muted-foreground"
          data-testid={`actor-workers-unknown-${actor}`}
        >
          Temporal could not be asked who is polling{' '}
          <code className="font-mono">{queue}</code>, so nothing here is a measurement.
          {report?.error ? ` (${report.error})` : ' The report has not arrived yet.'}
        </p>
      ) : workers.length === 0 ? (
        // REGISTERED AND NOTHING POLLING — the classic trap, in words, never an empty list.
        <p
          className="m-0 mt-1.5 text-[10.5px] italic text-amber-500/90"
          data-testid={`actor-workers-none-${actor}`}
        >
          registered, and nothing is polling <code className="font-mono">{queue}</code> — serve this
          Actor before dispatching to it, or the dispatch will sit on a queue nobody polls.
        </p>
      ) : (
        <>
          {serving.length === 0 && (
            // EVERY POLLER STALE. Distinct from the sentence above and it has to be: something WAS
            // here. The list below still shows them, because which worker died is the finding.
            <p
              className="m-0 mt-1.5 text-[10.5px] italic text-rose-500/90"
              data-testid={`actor-workers-stale-${actor}`}
            >
              no worker is serving this Actor. Temporal still lists{' '}
              {stale.length === 1 ? 'a poller' : `${stale.length} pollers`} on{' '}
              <code className="font-mono">{queue}</code>, and none of them has polled recently — a
              killed worker stays listed for about five minutes.
            </p>
          )}
          <ul className="m-0 mt-1.5 list-none space-y-1 p-0">
            {workers.map((w, i) => (
              <WorkerRow key={w.identity} actor={actor} index={i} worker={w} now={now} />
            ))}
          </ul>
        </>
      )}

      {/* THE LIVE SESSIONS — where the worker is running, which is not the same question as who is
          polling, and fails independently of it. */}
      <div className="mt-2" data-testid={`actor-sessions-${actor}`}>
        <span className="text-[9px] uppercase tracking-wider text-muted-foreground">
          live sessions
        </span>
        {sessions.length === 0 ? (
          <p
            className="m-0 mt-1 text-[10.5px] italic text-muted-foreground"
            data-testid={`actor-sessions-none-${actor}`}
          >
            the Monitor has no pane running {actor}. A worker started outside kontra’s tmux has no
            session here — and so does a Machine the Monitor cannot reach.
          </p>
        ) : (
          <ul className="m-0 mt-1 list-none space-y-1 p-0">
            {sessions.map((s, i) => {
              const sessionHealth = chipState(s.health);
              return (
                <li
                  key={s.key}
                  data-testid={`actor-session-${actor}-${i}`}
                  data-state={sessionHealth}
                  className="flex flex-wrap items-center gap-1.5 font-mono text-[10px]"
                >
                  <span
                    aria-hidden="true"
                    className={`inline-flex size-[7px] shrink-0 items-center justify-center rounded-full border ${STATE_CLASS[sessionHealth]}`}
                  />
                  <span className="text-foreground">{s.session}</span>
                  <span className="text-muted-foreground">
                    {s.mode} · {s.node}
                  </span>
                  <span className="text-muted-foreground">{s.address}</span>
                  <span
                    className="text-muted-foreground"
                    title="tmux windows of this session running this Actor — one session can hold the actor and its handler"
                  >
                    {s.windows} {s.windows === 1 ? 'window' : 'windows'}
                  </span>
                </li>
              );
            })}
          </ul>
        )}
      </div>
    </section>
  );
}

/**
 * One poller, drawn.
 *
 * THE AGE IS ALWAYS SHOWN, for a serving worker too. On a stale one it is the evidence — "6m ago"
 * is what makes the state checkable rather than something the page asserts — and on a live one it
 * is what lets an operator see the window working at all.
 *
 * `formatAge` renders an undated poll as `never`, which is the honest word: Temporal listed the
 * identity without a last-access time, so no age is known. `widgets/format` holds that rule (a zero
 * instant is `never`, not `0s ago`) for every surface, and this is one more.
 */
function WorkerRow({
  actor,
  index,
  worker,
  now,
}: {
  actor: string;
  index: number;
  worker: WorkerReading;
  now: number;
}): JSX.Element {
  const serving = worker.state === 'serving';
  return (
    <li
      data-testid={`actor-worker-${actor}-${index}`}
      data-state={worker.state}
      data-value={worker.identity}
      className="flex flex-wrap items-center gap-1.5 font-mono text-[10px]"
      title={
        serving
          ? `${worker.identity} polled ${formatAge(worker.lastPoll, now)} — inside the freshness window, so it can take work now`
          : `${worker.identity} is still LISTED by Temporal but last polled ${formatAge(worker.lastPoll, now)}, outside the freshness window. Temporal keeps a poller listed for about five minutes after it stops, so this is what a killed worker looks like. It is not a dispatch target.`
      }
    >
      <span
        aria-hidden="true"
        className={`inline-flex items-center rounded border px-1 ${STATE_CLASS[serving ? 'ok' : 'bad']}`}
      >
        {STATE_GLYPH[serving ? 'ok' : 'bad']}
      </span>
      <span className={serving ? 'text-foreground' : 'text-muted-foreground line-through'}>
        {worker.host ?? worker.identity}
      </span>
      <span className={`text-[9.5px] ${serving ? 'text-emerald-500' : 'text-rose-500'}`}>
        {worker.state}
      </span>
      <span className="text-[9.5px] text-muted-foreground">
        last poll {formatAge(worker.lastPoll, now)}
      </span>
      {/* THE HOST IS THE READABLE HALF and the identity is the whole of it. When the identity does
          not name a host (a custom `Identity` is legal) the row above already prints it, so this
          would repeat it — and a row that printed the same string twice is how a reader stops
          reading either. */}
      {worker.host !== undefined && (
        <span className="truncate text-[9.5px] text-muted-foreground/70">{worker.identity}</span>
      )}
      {!serving && (
        <span className="text-[9.5px] text-rose-500/90">not a dispatch target</span>
      )}
    </li>
  );
}
