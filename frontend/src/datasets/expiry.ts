/**
 * When this Dataset's rows stop existing — the retention answer, on the Dataset's own page.
 *
 * WHY IT IS DRAWN AT ALL. A tag means KEEP (ADR 0029 §3) and the page has offered tagging for two
 * slices without ever saying what an UNTAGGED Dataset is heading for. "Untagged output expires after
 * the retention TTL" lived in a tooltip on a `+ tag` button, which is the one place an operator who
 * has not decided to tag will never read it.
 *
 * THE WORDS ARE THE CONTRACT with `backend/src/data/retention.ts`, which decides. Five of the
 * six dispositions here are ITS five, spelled the same, in ITS precedence: temporary first (a temp
 * is never swept on a clock — ADR 0028's ownership), then tagged, then open, then the age gate
 * against `TTL + GRACE` measured from the LAST WRITE. The sixth is what the sweep says by never
 * enumerating a Dataset at all — see {@link ExpiryDisposition}. Two implementations of one policy is a drift with no
 * loud failure mode, so the drift is made cheap to see: same words, same order, same constants, and
 * a comment on each side pointing at the other.
 *
 * "WOULD", NEVER "WILL", AND NEVER A DELETION TIME. Collection happens when a sweep runs, not on a
 * clock owned by this page: the sweep is a Temporal Schedule — armed at boot since issue 18, hourly,
 * and previewing rather than collecting until a deployment sets `KONTRA_RETENTION_COLLECT` — and a
 * Dataset past its cutoff sits there until the next firing. `would be collected` is true whether the
 * sweep runs hourly, daily, previews only, or is not armed at all, which is exactly why the word is
 * "would": this page states the POLICY's reading of a Dataset, and never a promise about a clock it
 * does not own. A line reading
 * "deleted at 14:07" would be a promise this seam cannot keep in either direction.
 *
 * THE CLOCK IS THIS DATASET'S OWN LAST WRITE; THE SWEEP'S IS THE RUN'S. `gatherCandidates` groups by
 * **Run** and takes the newest write across everything that Run wrote, so a Run still writing a
 * SECOND table keeps this one alive past what this line says. That direction is deliberate: this
 * reading can be EARLY, never late, so it never tells an operator something is safe that is not.
 */

/**
 * The untagged TTL and the grace behind it, from the contract both halves read.
 *
 * THEY USED TO BE A NAMED COPY, and the note here argued for it: they are compiled-in constants on
 * the server, and a round trip per listing row to learn a constant is a worse dependency than a
 * copy. The first half of that is still true and is why this is not fetched. The second half was
 * wrong in a way nothing could see — MEASURED 2026-08-27, changing the server's TTL and updating
 * the one test that pins its literal left this entire suite green with the two halves disagreeing,
 * and the Datasets page quietly telling operators the wrong expiry.
 *
 * `@contract` is types and constants with no imports of its own, which is what lets a browser
 * bundle read it. The server's `retention.ts` cannot be imported here — it pulls DuckDB and the
 * object store in behind it, and that is the fork's original excuse, now answered.
 */
export { DATASET_RETENTION_TTL_MS as DATASET_TTL_MS, DATASET_RETENTION_GRACE_MS as DATASET_GRACE_MS } from '@contract/datasets';
import { DATASET_RETENTION_TTL_MS, DATASET_RETENTION_GRACE_MS } from '@contract/datasets';
const DATASET_TTL_MS = DATASET_RETENTION_TTL_MS;
const DATASET_GRACE_MS = DATASET_RETENTION_GRACE_MS;

/**
 * The sweep's own vocabulary — `SweepDisposition`, verbatim — plus one reading the sweep expresses
 * by ABSENCE rather than by a word.
 *
 * `not-a-candidate` is that one. `gatherCandidates` never enumerates a standalone list (an operator
 * input: no Run, so nothing to key a record or a purge on) or an output row whose Run does not
 * resolve, so there is no disposition to borrow — but a page that said nothing there would leave the
 * one Dataset kind that is NEVER collected looking like the ones that are.
 */
export type ExpiryDisposition =
  | 'collect'
  | 'kept-tagged'
  | 'kept-open'
  | 'kept-temporary'
  | 'kept-fresh'
  | 'not-a-candidate';

/** What one Dataset's retention reading says. */
export interface ExpiryReading {
  disposition: ExpiryDisposition;
  /** Drawn beside the tags — the short form. */
  label: string;
  /** The sentence behind it, including which safeguard applies and what would change it. */
  title: string;
  /** TRUE when a sweep would collect this Dataset TODAY — the one reading that wants attention. */
  due: boolean;
}

/** The facts the decision needs, all of which a listing row already carries. */
export interface ExpiryFacts {
  /** A Run's temporary Dataset (ADR 0028) — deleted by asking, never swept. */
  temporary?: boolean;
  /** The tag set on the owning Run's record (ADR 0029 §1). One tag is enough to keep it. */
  tags?: readonly string[];
  /** The §11 lifecycle as `datasets/state.ts` read it. `open` is kept while a producer may write. */
  state?: string;
  /** When the newest data file under this Dataset was committed (epoch ms). 0 = the catalog
   *  reported none, which is unknown age and NOT infinite age. */
  updatedAt?: number;
  /** `standalone` is an operator-loaded list — never a sweep candidate, because no Run wrote it. */
  kind?: string;
  /** The **Run** the record is keyed on. Absent means no SINGLE Run resolved — see
   *  {@link ExpiryFacts.runs}, which is what decides whether the sweep can address this at all. */
  runId?: string;
  /**
   * EVERY **Run** known to have written this Dataset — the honest plural the listing row already
   * carries.
   *
   * IT IS WHAT MAKES A SHARED DATASET A CANDIDATE. The sweep keys its candidates on every
   * contributing Run, not on the singular {@link ExpiryFacts.runId} (which is deliberately absent the
   * moment two Runs share a partition), so a Dataset several Runs wrote IS swept — one decision per
   * Run. Reading `runId` alone here would draw "not swept" over exactly the largest, longest-lived
   * Datasets, which is the reading this seam must never get wrong.
   */
  runs?: readonly string[];
}

/**
 * How long until the cutoff, in the coarse grain a retention answer deserves. Hours and days, never
 * minutes: the TTL is a day long and a countdown to the second would imply a precision the sweep
 * schedule does not have.
 */
export function untilText(ms: number): string {
  if (ms <= 0) return 'now';
  const h = Math.floor(ms / 3_600_000);
  if (h < 1) return 'under an hour';
  if (h < 48) return `${h}h`;
  return `${Math.round(h / 24)}d`;
}

/**
 * The retention reading for one Dataset.
 *
 * Total: every input, including a Dataset with no recorded write time, produces a sentence. There is
 * no "unknown" disposition, because the SWEEP has none — an unrecorded last write is `kept-fresh`
 * there (kept rather than guessed at), and reporting anything else here would be this page inventing
 * a policy the server does not implement.
 */
export function datasetExpiry(
  facts: ExpiryFacts,
  now: number,
  opts: { ttlMs?: number; graceMs?: number } = {}
): ExpiryReading {
  const ttlMs = opts.ttlMs ?? DATASET_TTL_MS;
  const graceMs = opts.graceMs ?? DATASET_GRACE_MS;
  if (facts.temporary) {
    return {
      disposition: 'kept-temporary',
      label: 'never swept',
      title:
        'A temporary Dataset is a Run’s staging table (ADR 0028): the retention sweep never collects ' +
        'one, tagged or not. It goes when somebody deletes it — the button on its listing row, or ' +
        '`kontra dataset rm`.',
      due: false,
    };
  }
  // NEVER A CANDIDATE, so no clock applies. Checked before the age gate rather than after it: an
  // old standalone list would otherwise read `would be collected`, which is the one direction this
  // module must never be wrong in — a Dataset the sweep cannot even enumerate would look doomed.
  // A Dataset is a candidate when ANY Run can be named for it — the same set the sweep keys its
  // candidates on. One resolved Run is enough; several is still enough.
  const runs = knownRuns(facts);
  if (facts.kind === 'standalone' || runs === 0) {
    return {
      disposition: 'not-a-candidate',
      label: 'not swept',
      title:
        facts.kind === 'standalone'
          ? 'An operator-loaded list has no Run behind it, and retention is keyed by the Run — the ' +
            'sweep never enumerates one, so no TTL applies to it.'
          : 'No Run resolved for this Dataset, and retention is keyed by the Run: the sweep can ' +
            'neither read a tag for it nor purge it, so it never becomes a candidate. That is a gap ' +
            'in what is KNOWN about this Dataset, not a promise that it is kept forever.',
      due: false,
    };
  }
  /** What a shared Dataset's reading is really about — appended to every sentence below when the
   *  answer is not the same for every Run that wrote it. */
  const perRun =
    runs > 1
      ? ` ${runs} Runs wrote this Dataset and retention decides one Run at a time (the record is ` +
        'Run-grain), so this reading is the newest Run’s — another Run’s rows may be kept or ' +
        'collected on their own terms.'
      : '';
  if ((facts.tags?.length ?? 0) > 0) {
    return {
      disposition: 'kept-tagged',
      label: 'kept · tagged',
      title:
        'Tagged output is KEPT (ADR 0029 §3). The tag is on the Run, so everything that Run wrote is ' +
        'kept with it. Removing every tag puts this Dataset back on the TTL.' + perRun,
      due: false,
    };
  }
  if (facts.state === 'open') {
    return {
      disposition: 'kept-open',
      label: 'kept · still open',
      title:
        'A Dataset a producer may still be appending to is never collected, however old its last ' +
        'write. Once its caller seals or abandons it, the untagged TTL starts to apply.',
      due: false,
    };
  }
  const lastWrite = facts.updatedAt ?? 0;
  if (!lastWrite) {
    return {
      disposition: 'kept-fresh',
      label: 'kept · no write time',
      title:
        'The catalog reports no commit time for this Dataset. An unknown age is not an infinite age, ' +
        'so the sweep keeps it rather than guessing — the same refusal the unit sweep makes for an ' +
        'object whose store reported no mtime.',
      due: false,
    };
  }
  const cutoff = now - (ttlMs + graceMs);
  if (lastWrite < cutoff) {
    return {
      disposition: 'collect',
      label: 'would be collected',
      title:
        'Untagged, not open, and its last write is past the retention TTL plus grace — a sweep would ' +
        'collect this Dataset. Tag it to keep it. Collection happens when a sweep runs, not on a ' +
        'clock this page owns.' + perRun,
      due: true,
    };
  }
  const left = lastWrite - cutoff;
  return {
    disposition: 'kept-fresh',
    label: `would be collected in ${untilText(left)}`,
    title:
      `Untagged output is collected once its LAST WRITE is older than the retention TTL (${untilText(ttlMs)}) ` +
      `plus grace (${untilText(graceMs)}). Writing to it again restarts that clock; tagging it stops ` +
      'the clock entirely.' + perRun,
    due: false,
  };
}

/** How many **Runs** are known to have written this Dataset — the singular key and the plural set as
 *  one number, because either is enough for the sweep to address it. */
function knownRuns(facts: ExpiryFacts): number {
  const runs = new Set<string>(facts.runs ?? []);
  if (facts.runId) runs.add(facts.runId);
  return runs.size;
}
