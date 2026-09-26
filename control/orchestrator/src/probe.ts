/**
 * THE ACTOR PROBE — start one Method call, from the Actors page (ADR 0033).
 *
 * The page collects a Batch and presses Run; this starts ONE execution of a kontra-owned workflow
 * that makes exactly ONE Method call over it, through the same Nexus operation production uses,
 * and publishes into an ordinary untagged Dataset. That is the whole feature.
 *
 * ── THIS IS NOT THE INTERPRETER COMING BACK ──────────────────────────────────────────────────
 *
 * ADR 0023 §12 removed a GENERAL interpreter that executed user-composed topologies: it owned
 * sharding, failure policy, materialization and the streaming cursor. The probe composes nothing,
 * decides no failure policy, owns no cursor and shards nothing — it is one dispatch a human asked
 * for by hand, of exactly the kind they would otherwise have written into a file and served
 * themselves.
 *
 * THE TEST A REVIEWER APPLIES IS A COUNT: *how many Methods can one request name?* One is a probe.
 * Two — in any spelling, however it is dressed — is a topology, and a server that executes a
 * topology is the interpreter, whatever the route is called. {@link probeRequest} is where that
 * count is enforced, and it enforces it by REFUSING EVERY FIELD THAT IS NOT ONE OF FIVE rather
 * than by validating a shape that could express two. A shape that CAN express a topology and
 * refuses one by a check will eventually have the check relaxed for a good reason, and the
 * interpreter returns with no commit that reads as reintroducing it.
 *
 * ── AND THE ORCHESTRATOR STILL DOES NOT EXECUTE BATCHES ──────────────────────────────────────
 *
 * The invariant this file has to respect is narrower than the sentence five comments in this repo
 * used to carry ("POST /api/runs 404s on purpose" — it has not, since 2026-08-15). The true one:
 * **the orchestrator starts workflows; it does not execute Batches.** This file starts a workflow.
 * The Batch is executed by the Actor, reached through the Actor's own registered Nexus endpoint,
 * by a worker this process does not host.
 *
 * ── WHY IT STARTS SOMETHING IT CANNOT SERVE ──────────────────────────────────────────────────
 *
 * A Nexus operation is scheduled by a WORKFLOW command in every SDK we have (ADR 0033 finding 1),
 * so a dispatch activity is not a cheaper design — it is not a design. And TypeScript has no
 * caller half to reuse (ADR 0023 §22), so hosting the probe here would mean a third independent
 * wire encoder and a sixth endpoint-name derivation in the one language with no peer to pin them
 * against. The workflow is therefore the SDK's (`sdk/python/kontra/probe.py`), served by a
 * Python worker on {@link PROBE_QUEUE}, and this module is the START and the four refusals that
 * come before it.
 */

import { randomUUID } from 'node:crypto';
import { ControlRefused } from './workflowControl';
import { endpointName, listEndpoints, sharedQueue } from './nexusRegistry';
import { describeQueue, pollIsFresh, temporalQueueDescriber, type QueueDescriber } from './pollers';
import { PROBE_WORKFLOW, probeQueue } from './queues';
import { NAMESPACE, getClient } from './temporalClient';
import { runWorkflowStore, type RunWorkflowStore } from './data/runWorkflows';
import { tenantAttributes } from './visibility';

/**
 * The five fields a probe request has, and there is no sixth.
 *
 * Everything ADR 0033 §1 lists as refused — a second Method, a second Actor, an output wired to
 * another input, a branch, a condition, a loop, a retry policy, a schedule, a fan-out width — is
 * refused by not being in this list. The peer is `kontra.probe.PROBE_FIELDS`; both sides refuse,
 * because the route is not the only way to reach the workflow (a caller can dial Temporal), and
 * the workflow is not the only place a refusal should cost nothing (a refused request must not
 * start an execution).
 */
export const PROBE_FIELDS = ['actor', 'version', 'method', 'units', 'dataset'] as const;

/**
 * What a probe's Run is CALLED (ADR 0033 §5). The probe is not a registered folder and has no
 * `workflow.json`, so `stampRunWorkflow` has no manifest identity to snapshot — it stamps this
 * through the same store instead, and a probe's output Dataset reads as a probe's rather than
 * falling back to ADR 0029's Actor-grain name (which is a supported path, not a defect).
 */
export const PROBE_IDENTITY = 'kontra-probe';

/** The probe workflow's OWN version — it moves when what a probe does changes, never when the
 *  Actor it called does. Stamped beside {@link PROBE_IDENTITY}; both are required non-empty. */
export const PROBE_VERSION = '1';

/** A Method name, bounded the way the caller route bounds one: the Go SDK's `AddMethod` takes any
 *  non-empty string, so `dns-facts` must be legal — what is refused is everything a SECOND name
 *  could ride in (a comma, a pipe, an arrow, whitespace, a newline). */
const METHOD_RE = /^[A-Za-z0-9_][A-Za-z0-9._-]{0,63}$/;

/** A Dataset name. The probe writes an ORDINARY Dataset, so this is the ordinary rule. */
const DATASET_RE = /^[A-Za-z0-9_][A-Za-z0-9._-]{0,63}$/;

/** The longest a Dataset name may be — the bound above, counted. */
const DATASET_MAX = 64;

/** One Actor, one version, one Method, one Batch, and where the results go. */
export interface ProbeInput {
  actor: string;
  version: string;
  method: string;
  units: unknown[];
  /** Where the results publish. Minted by {@link startProbe} when the caller names none, because
   *  a probe's whole value is that it leaves evidence rather than a screenful. */
  dataset?: string;
}

/** What starting one probe answers with. */
export interface ProbeStarted {
  /** The Run — one execution of a caller's workflow, like any other (ADR 0033's first
   *  consequence). There is no "probe run" kind and there must not be one. */
  runId: string;
  actor: string;
  version: string;
  method: string;
  /** How many Units were sent, so an empty result has a denominator before the run returns. */
  units: number;
  /** The untagged Dataset the results land in. ADR 0029 §3 already disposes of it. */
  dataset: string;
  /** The kontra-owned queue the probe workflow was started on. */
  queue: string;
  /** The Actor's registered Nexus endpoint — the address the dispatch will go to. */
  endpoint: string;
  /** What was stamped as this Run's workflow identity (ADR 0033 §5), when the stamp landed. */
  workflow?: { name: string; version: string };
}

/**
 * Parse a probe request, or refuse it by name — the COUNT, enforced.
 *
 * AN UNKNOWN FIELD IS A REFUSAL, NOT A SHRUG, and that is the load-bearing half. Ignoring
 * `{ method: 'head', then: { method: 'tail' } }` would run one call and hand back a result the
 * caller reads as two, which is the worst of the three available outcomes — worse than running
 * both, because nothing on either side ever says so. Refusing names the field and says what it
 * would have made this.
 */
export function probeRequest(body: unknown, actor: string, version: string): ProbeInput {
  if (typeof body !== 'object' || body === null || Array.isArray(body)) {
    throw new ProbeRefused(
      `a probe takes one request object with ${PROBE_FIELDS.join(', ')} — got ${typeName(body)}`
    );
  }
  const req = body as Record<string, unknown>;
  const extra = Object.keys(req).filter((k) => !(PROBE_FIELDS as readonly string[]).includes(k));
  if (extra.length > 0) throw new ProbeRefused(extraFieldRefusal(extra.sort()));

  // The Actor and the version come from the REGISTERED FOLDER, not from the body — the route is
  // keyed by the folder's id, exactly as the caller route is. A body that names them anyway must
  // agree, because a probe that silently ignored `actor` would let a caller believe it had aimed
  // somewhere it had not.
  const named = oneString(req.actor, 'actor');
  if (named !== '' && named !== actor) {
    throw new ProbeRefused(
      `this probe is aimed at the registered folder for ${actor}, and the request names ${named} — ` +
        'a probe names ONE Actor (ADR 0033 §1). Probe the other from its own card.'
    );
  }
  const namedVersion = oneString(req.version, 'version');
  if (namedVersion !== '' && namedVersion !== version) {
    throw new ProbeRefused(
      `this folder is registered at version ${version || '(none)'}, and the request names ` +
        `${namedVersion} — a probe names ONE version (ADR 0033 §1).`
    );
  }

  const method = oneString(req.method, 'method');
  if (!METHOD_RE.test(method)) {
    throw new ProbeRefused(
      `${JSON.stringify(method)} is not one Method name. A probe calls exactly one Method ` +
        '(ADR 0033 §1); two Methods in one request — however they are spelled — is a topology, ' +
        'and the way to run two is to run two probes.'
    );
  }

  const raw = req.units ?? [];
  // A LIST, because a Batch is a list of Units. A bare object silently wrapped would teach the
  // shape wrong on the surface whose whole job is teaching it, and `len(batch)` on a dict is its
  // number of KEYS.
  if (!Array.isArray(raw)) {
    throw new ProbeRefused(
      `a Batch is a list of Units; got ${typeName(raw)} — wrap the Unit in a list`
    );
  }

  const dataset = oneString(req.dataset, 'dataset');
  if (dataset !== '' && !DATASET_RE.test(dataset)) {
    throw new ProbeRefused(`${JSON.stringify(dataset)} is not a Dataset name`);
  }

  return { actor, version, method, units: raw, ...(dataset === '' ? {} : { dataset }) };
}

/**
 * Start one probe.
 *
 * FOUR REFUSALS COME FIRST, and each of them is a hang this would otherwise be:
 *
 *   1. THE REQUEST NAMES MORE THAN ONE CALL — {@link probeRequest}, above. Not a hang; a topology.
 *   2. THE ACTOR HAS NO NEXUS ENDPOINT. A real, recorded state: registration creates the endpoint
 *      and `endpoint` is simply absent when the cluster could not be reached, "which is a state
 *      the Actors page can show and a later register can repair" (`nexusRegistry.ts`). Dispatching
 *      to a name nobody created is a Nexus call that waits, so the probe refuses with that
 *      sentence instead — the one thing ADR 0033 §4 says a probe must handle that a hand-written
 *      caller need not.
 *   3. NOTHING IS SERVING THE ACTOR. A dispatch onto a queue nobody drains reports as slow, and a
 *      probe is allowed to take minutes (the backing workflow retries `RunBatch` ten times with a
 *      two-minute heartbeat), so "slow" and "nobody is there" are genuinely indistinguishable from
 *      the outside. STALE COUNTS AS NOTHING: Temporal lists a poller for about five minutes after
 *      it stops, so `identities.length > 0` is not evidence a worker is alive — {@link pollIsFresh}
 *      is, and it is the same measured window the Actors page draws `serving` with.
 *   4. NOTHING IS SERVING THE PROBE WORKFLOW ITSELF. kontra's shipped containers are Node-only and
 *      the probe worker is Python (ADR 0033 §3 prices this deployment addition explicitly), so an
 *      installation that has not started it would otherwise get a Run that sits at `scheduled`
 *      forever. The refusal names the command.
 *
 * Then, and only then, one `client.workflow.start`. Nothing is persisted about the run's progress
 * — the Runs surface reads Temporal — and the one thing written after the fact is the identity
 * stamp (§5), best-effort for the reason `startRun`'s is: the run is already going by then, and
 * reporting a started Run as a failed start is the worst outcome available.
 */
export async function startProbe(input: ProbeInput, deps: ProbeDeps = {}): Promise<ProbeStarted> {
  const { actor, version, method, units } = input;
  const endpoint = endpointName(actor, version);
  const queue = probeQueue();
  const now = deps.now ?? Date.now;

  const known = await (deps.endpoints ?? listEndpoints)();
  if (!known.has(endpoint)) {
    throw new ProbeRefused(
      `${actor} has no Nexus endpoint on this cluster — ${endpoint} was never created, so a ` +
        'dispatch would wait on a name nobody registered. Register the folder again (registering ' +
        'is what creates the endpoint; forgetting is what removes it).'
    );
  }

  const describer = deps.describer ?? temporalQueueDescriber();
  const actorQueue = sharedQueue(actor, version);
  const serving = await describeQueue(describer, actorQueue);
  if (serving.error !== undefined) {
    throw new ProbeRefused(
      `cannot verify a worker is serving ${actor} on ${actorQueue}: ${serving.error} — a probe ` +
        'will not be dispatched onto a queue nobody can confirm is being polled.'
    );
  }
  const fresh = serving.workers.filter((w) => pollIsFresh(w.lastPoll, now()));
  if (fresh.length === 0) {
    throw new ProbeRefused(
      serving.workers.length === 0
        ? `nothing is serving ${actor} — queue ${actorQueue} has no pollers. Serve it first, or ` +
          'the dispatch will sit on a queue nobody polls and read as a slow run.'
        : `${serving.workers.length} poller(s) are still listed on ${actorQueue} but none has ` +
          'polled recently — Temporal lists a worker for about five minutes after it stops. ' +
          'Nothing there can run: serve this Actor again before probing it.'
    );
  }

  const probeWorkers = await describeQueue(describer, queue);
  if (probeWorkers.error !== undefined || probeWorkers.identities.length === 0) {
    throw new ProbeRefused(
      `the probe worker is not running — nothing polls ${queue}` +
        (probeWorkers.error === undefined ? '' : ` (${probeWorkers.error})`) +
        '. It is a separate process because a Nexus dispatch can only be made from a workflow ' +
        "and kontra's own containers are Node-only: start it with `docker compose up -d " +
        'orchestrator-probe`, or `python3 -m kontra.probe` from a checkout.'
    );
  }

  /* A FRESH RUN ID PER PROBE, AND THAT IS ADR 0033 §2 (see `sdk/python/kontra/probe.py`).
     `backingWorkflowID` falls through to `actor-<name>-<runID-nodeID>` when no key is bound, so
     the probe's own workflow id is what keeps two probes apart — and a probe is the one caller
     most likely to be fired twice in ten seconds. A seconds-granular id alone is NOT enough for
     that: two presses inside one second would collide, and Temporal would answer the second with
     AlreadyStarted rather than running it. The random half is what makes "fresh per probe" true
     at the granularity a human actually clicks at. */
  const stamp = Math.floor(now() / 1000);
  const nonce = randomUUID().replace(/-/g, '').slice(0, 8);
  const workflowId = `${PROBE_WORKFLOW.toLowerCase()}-${stamp}-${nonce}`;
  const dataset = input.dataset ?? probeDataset(actor, method, nonce);

  const start = deps.start ?? temporalProbeStarter();
  const runId = await start(PROBE_WORKFLOW, {
    taskQueue: queue,
    workflowId,
    // ONE ARGUMENT, the whole request. Positional arguments are how a second Method eventually
    // arrives as "just one more"; an object with five named fields has nowhere to put it.
    args: [{ actor, version, method, units, dataset }],
  });

  const workflow = await stampProbeIdentity(runId, deps.recorder);
  return {
    runId,
    actor,
    version,
    method,
    units: units.length,
    dataset,
    queue,
    endpoint,
    ...(workflow ? { workflow } : {}),
  };
}

/**
 * What a probe's output Dataset is called: `probe-<actor>-<method>-<nonce>`.
 *
 * AN ORDINARY NAME, AND ORDINARY IS THE POINT (ADR 0033 §5). No tag, no probe flag, no
 * probe-specific expiry — untagged, ADR 0029 §3's sweep already disposes of it after the repo's
 * TTL, and an operator who finds a probe's output worth keeping tags it, at which point
 * "tagged: kept" applies unchanged. A second lifetime for the same object is the shape ADR 0029's
 * temp-versus-sweep clarification exists to prevent.
 *
 * THE NONCE IS LAST AND SURVIVES TRUNCATION. Two probes of one Method must not write into one
 * Dataset — that is the same confusion §2 refuses a key over, arriving through the output side —
 * so the halves that can be long are trimmed and the unique half never is.
 */
export function probeDataset(actor: string, method: string, nonce: string): string {
  const safe = (s: string) => s.replace(/[^A-Za-z0-9._-]/g, '-').replace(/^[.-]+/, '');
  const name = `probe-${safe(actor).slice(0, 24)}-${safe(method).slice(0, 24)}-${nonce}`;
  return name.slice(0, DATASET_MAX);
}

/**
 * Snapshot the probe's own identity for the Run it just started (ADR 0033 §5).
 *
 * SWALLOWS ITS FAILURE, for the reason `stampRunWorkflow` does: the Run is already going, and
 * rethrowing would report a started Run as a failed start — the caller retries, Temporal refuses
 * the duplicate id, and an operator concludes the probe never began. What is lost is bounded and
 * documented: the Dataset renders ADR 0029's Actor-grain name instead, which is a supported path.
 */
async function stampProbeIdentity(
  runId: string,
  recorder?: Pick<RunWorkflowStore, 'record'>
): Promise<{ name: string; version: string } | undefined> {
  try {
    await (recorder ?? runWorkflowStore()).record(runId, PROBE_IDENTITY, PROBE_VERSION);
    return { name: PROBE_IDENTITY, version: PROBE_VERSION };
  } catch {
    return undefined;
  }
}

/** A request that is not a probe — a `ControlRefused` so every route's existing catch answers it
 *  400, and its own class so a caller can tell "fix the request" from "the start failed". */
export class ProbeRefused extends ControlRefused {}

/** What {@link startProbe} needs from the world, injected so a test never dials Temporal and
 *  never opens a database — the same seam `startRun` takes its describer and recorder through. */
export interface ProbeDeps {
  describer?: QueueDescriber;
  endpoints?: () => Promise<Set<string>>;
  start?: ProbeStarter;
  recorder?: Pick<RunWorkflowStore, 'record'>;
  now?: () => number;
}

/** Start the workflow and answer with its id. A function rather than the Temporal client, so the
 *  test double is three lines and cannot accidentally exercise the SDK. */
export type ProbeStarter = (
  type: string,
  options: { taskQueue: string; workflowId: string; args: unknown[] }
) => Promise<string>;

function temporalProbeStarter(): ProbeStarter {
  return async (type, options) => {
    const client = await getClient();
    // Stamped HERE and not in `ProbeStarter`'s type, so the test double stays three lines: the
    // attribute is a property of how THIS starter reaches Temporal, not of what a probe is. See
    // `tenantAttributes` for why every start in this control plane carries it.
    const handle = await client.workflow.start(type, {
      ...options,
      typedSearchAttributes: tenantAttributes(NAMESPACE),
    });
    return handle.workflowId;
  };
}

/** One string, or the refusal that says a list of them is a topology.
 *
 *  THE LIST FORM IS THE INTERESTING FAILURE. `{ method: ['head', 'tail'] }` is exactly how a second
 *  Method arrives when somebody is being helpful, and `String(['head','tail'])` would have
 *  dispatched to a Method called `head,tail` — refused at the actor's registry, minutes later,
 *  with a sentence about a name nobody wrote. */
function oneString(value: unknown, field: string): string {
  if (value === undefined || value === null) return '';
  if (Array.isArray(value)) {
    throw new ProbeRefused(
      `a probe names ONE ${field}, not ${value.length} — one Actor, one version, one Method, one ` +
        'Batch (ADR 0033 §1). Run a second probe for the second.'
    );
  }
  if (typeof value !== 'string') {
    throw new ProbeRefused(`${field} must be a string, got ${typeName(value)}`);
  }
  return value.trim();
}

/** Name the field, and say what it would have made this. */
function extraFieldRefusal(extra: string[]): string {
  if (extra.includes('key')) {
    /* NOT an oversight and not a field to add later (ADR 0033 §2). A keyed dispatch ATTACHES to
       the execution already holding that key and returns THAT batch's results, so two probes ten
       seconds apart would silently be one — and `backingWorkflowID` carries no Method to tell them
       apart. Probing a keyed object's durable state is a real need with its own ADR ahead of it. */
    return (
      'a probe does not take a key. A keyed dispatch ATTACHES to the execution already holding ' +
      "that key and returns THAT batch's results (ADR 0033 §2), so two probes ten seconds apart " +
      'would silently be one — the probe dispatches unkeyed, into a private anonymous Session.'
    );
  }
  const named = extra.map((k) => JSON.stringify(k)).join(', ');
  return (
    `a probe request has no ${extra.length === 1 ? 'field' : 'fields'} ${named}. It takes ` +
    `${PROBE_FIELDS.join(', ')} and nothing else: one Actor, one version, one Method, one Batch ` +
    '(ADR 0033 §1). A second Method, a branch, a loop, a retry policy or a fan-out width would ' +
    'make this a topology, and a server that executes a topology is the interpreter ADR 0023 §12 ' +
    'removed.'
  );
}

function typeName(value: unknown): string {
  if (value === null) return 'null';
  if (Array.isArray(value)) return 'a list';
  return typeof value;
}

/* ─────────────────────────────── reading one back ─────────────────────────────── */

/**
 * What one probe answered with — the workflow's own return value (`kontra.probe.probe_result`).
 *
 * `isolated` AND `done` ARE HERE BESIDE `results`, and that is ADR 0028 §4 carried through to the
 * surface. A Method that dropped every Unit and one that legitimately found nothing both return
 * zero rows; a probe UI drawing only `results` reproduces the failure mode that let a 15,814-target
 * run report `completed` in seven minutes having scanned almost nothing.
 */
export interface ProbeResult {
  actor: string;
  version: string;
  method: string;
  /** How many Units were sent — the denominator, so an empty result is readable. */
  units: number;
  results: number;
  /** How many Units the Method PERMANENTLY dropped. Free: it rides the returned ref's meta. */
  isolated: number;
  /** False when the Method returned before covering its input. */
  done: boolean;
  /** Which Machine ran it, or '' — unrecorded is a real answer, never a borrowed value. */
  machine: string;
  dataset: string;
}

/** One probe, as the page reads it back while it runs. */
export interface ProbeReading {
  runId: string;
  /** Temporal's own word — `RUNNING`, `COMPLETED`, `FAILED`, `TIMED_OUT`, … Not folded into a
   *  boolean: "still going" and "failed" send an operator to two different places. */
  status: string;
  /** Present once the workflow returned. */
  result?: ProbeResult;
  /** The failure's own sentence, when there is one. */
  failure?: string;
}

/** What {@link readProbe} needs of Temporal. Injected so a test never dials one. */
export interface ProbeHandles {
  describe(runId: string): Promise<{ type: string; status: string }>;
  result(runId: string): Promise<unknown>;
}

/**
 * Read one probe's answer back.
 *
 * IT REFUSES A RUN THAT IS NOT A PROBE, and that is not tidiness. A workflow's return value is
 * whatever its author put there — a caller's own workflow could return credentials, rows, a
 * customer's data — so a route that handed back `handle.result()` for any id would be a general
 * read of every workflow's output on the cluster, reached through a surface nobody would think to
 * audit. The type check is the bound: this reads the return value of the ONE workflow type kontra
 * owns and whose shape kontra defines.
 *
 * AND IT IS NOT A "PROBE RUN" KIND (ADR 0033's first consequence). The Run is an ordinary Run,
 * listed and read by every ordinary run surface; this reads one workflow type's return value, which
 * no general surface has a shape for.
 */
export async function readProbe(runId: string, handles?: ProbeHandles): Promise<ProbeReading> {
  const h = handles ?? temporalProbeHandles();
  const desc = await h.describe(runId);
  if (desc.type !== PROBE_WORKFLOW) {
    throw new ProbeRefused(
      `${runId} is a ${desc.type || 'unknown'} run, not a probe — this reads the answer of the one ` +
        'workflow type kontra owns, and a workflow returns whatever its author put there.'
    );
  }
  const reading: ProbeReading = { runId, status: desc.status };
  // STILL GOING IS NOT AN ANSWER AND MUST NOT BE DRESSED AS ONE. A probe legitimately takes minutes
  // — the backing workflow retries `RunBatch` ten times with a two-minute heartbeat — so `RUNNING`
  // is reported as itself, with no counts invented to fill the space.
  if (desc.status === 'RUNNING') return reading;
  try {
    // The shape is the Python peer's, and it is read defensively rather than trusted: a worker
    // older than a field simply omits it, and a missing count must read as absent, never as zero.
    const answer = await h.result(runId);
    if (desc.status === 'COMPLETED') reading.result = asProbeResult(answer);
  } catch (err) {
    /* A TERMINAL RUN THAT IS NOT `COMPLETED` HAS TO SAY WHY, and this is the only place the reason
       exists: `describe` reports the STATUS and nothing about the cause, so a failed probe read
       from the status alone would render as an empty report beside the word "failed" — which is
       indistinguishable, on screen, from a probe still going. `result()` is what raises the
       failure, so it is called for a failed run too, purely to be caught. */
    reading.failure = failureText(err);
  }
  return reading;
}

/**
 * The sentence a failed workflow actually carries.
 *
 * `WorkflowFailedError.message` is "Workflow execution failed" for every failure there has ever
 * been; the author's own sentence — `ProbeRefused`'s refusal, or the Method's exception — is one
 * level down in `cause`. Reporting the wrapper would replace the one useful string with a constant.
 */
function failureText(err: unknown): string {
  const e = err as { message?: string; cause?: { message?: string } };
  return e?.cause?.message || e?.message || String(err);
}

function asProbeResult(value: unknown): ProbeResult | undefined {
  if (typeof value !== 'object' || value === null) return undefined;
  const r = value as Record<string, unknown>;
  const num = (k: string) => (typeof r[k] === 'number' ? (r[k] as number) : 0);
  const str = (k: string) => (typeof r[k] === 'string' ? (r[k] as string) : '');
  return {
    actor: str('actor'),
    version: str('version'),
    method: str('method'),
    units: num('units'),
    results: num('results'),
    isolated: num('isolated'),
    // ABSENT MEANS DONE, matching `Batch.from_ref`'s own reading of the meta: only an explicit
    // `false` says the producer returned before covering its input.
    done: r.done !== false,
    machine: str('machine'),
    dataset: str('dataset'),
  };
}

function temporalProbeHandles(): ProbeHandles {
  return {
    describe: async (runId) => {
      const client = await getClient();
      const desc = await client.workflow.getHandle(runId).describe();
      return { type: String(desc.type ?? ''), status: String(desc.status.name) };
    },
    result: async (runId) => {
      const client = await getClient();
      return client.workflow.getHandle(runId).result();
    },
  };
}
