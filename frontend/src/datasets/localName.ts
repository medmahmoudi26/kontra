/**
 * The derived Dataset name, with its instant rendered in the VIEWER'S time zone.
 *
 * ADR 0029 §2 fixes both halves of this and they are not in tension:
 *
 *   - THE STORED/DERIVED STRING IS UTC AND PATH-SAFE. "Two controllers exist (sfo3, nyc1); a
 *     local-time name would denote two different instants depending on which box wrote it." That
 *     string is the identity — `kontra dataset ls` prints it, the API returns it, a rename replaces
 *     it — and NOTHING here changes it. The server is still the only thing that spells it
 *     (`backend/src/data/datasetName.ts`), and this page never re-derives it.
 *   - "THE UI RENDERS LOCAL." `2026-08-19T14-49-20Z` is the correct thing to store and the wrong
 *     thing to read: an operator watching a run at 16:49 should not have to subtract two hours to
 *     recognise their own Dataset. So the datetime — and ONLY the datetime — is re-rendered here,
 *     at draw time, from the viewer's clock.
 *
 * A PURE FUNCTION OVER THE STRING, deliberately. The alternative was shipping a second field from
 * the server, which would mean the server deciding a browser's time zone — and would put two
 * spellings of one name on the wire. This takes the one name that arrived and formats it.
 *
 * IT RECOGNISES ONLY THE DERIVED SHAPE. A RENAME is an operator's own words (ADR 0029 §4) and has
 * no datetime to localise, so it passes through untouched — which is also why `localized` is
 * reported rather than assumed: a caller rendering a title needs to know whether there is a
 * canonical form worth showing.
 */

/** The derived name's datetime, as ADR 0029 §2 spells it: `--<YYYY-MM-DDTHH-MM-SS>Z--`. */
const DERIVED_INSTANT = /--(\d{4})-(\d{2})-(\d{2})T(\d{2})-(\d{2})-(\d{2})Z--/;

/** What to draw, and what the drawn thing is a rendering OF. */
export interface LocalDatasetName {
  /** The name to show: the derived name with a local, human instant, or the input unchanged. */
  text: string;
  /** The name as the server, the API and `kontra dataset ls` carry it — always the input. */
  canonical: string;
  /** Whether the instant was re-rendered. False for a rename, and for any string that is not a
   *  derived name (a Dataset written before the format, a future spelling this build predates). */
  localized: boolean;
}

/** Test seam: the environment's locale and zone are the default, and both are overridable so a
 *  test pins the format without depending on the machine it runs on. */
export interface LocalNameOptions {
  locale?: string;
  timeZone?: string;
}

/**
 * `19 Aug 2026 16:49:20` — assembled from parts rather than taken from `toLocaleString`.
 *
 * The ORDER is fixed here on purpose. A locale-ordered datetime would put the month first for one
 * viewer and the day first for another, inside a string whose whole job is to be recognised at a
 * glance beside the same Dataset on someone else's screen. What IS the viewer's is the zone (the
 * point of the exercise) and the month's spelling.
 */
function humanInstant(at: Date, opts: LocalNameOptions): string {
  const parts = new Intl.DateTimeFormat(opts.locale, {
    day: '2-digit',
    month: 'short',
    year: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
    ...(opts.timeZone ? { timeZone: opts.timeZone } : {}),
  }).formatToParts(at);
  const of = (type: Intl.DateTimeFormatPartTypes): string =>
    parts.find((p) => p.type === type)?.value ?? '';
  // `24` is how some locales spell midnight under hour12:false; the instant is the same and the
  // date part is already the correct day, so it is normalised rather than left to read as a 25th
  // hour.
  const hour = of('hour') === '24' ? '00' : of('hour');
  return `${of('day')} ${of('month')} ${of('year')} ${hour}:${of('minute')}:${of('second')}`;
}

/**
 * Render one Dataset name for a human. See the module header for why only the instant moves.
 *
 * The structure is kept — `wf-<workflow>-<version>--<instant>--<fragment>` — so the localised
 * string is recognisably THE name rather than a different label for it, and the caller shows
 * {@link LocalDatasetName.canonical} on hover for the string the CLI prints.
 */
export function localDatasetName(
  name: string | undefined,
  opts: LocalNameOptions = {}
): LocalDatasetName | undefined {
  if (!name) return undefined;
  const m = DERIVED_INSTANT.exec(name);
  if (!m) return { text: name, canonical: name, localized: false };
  const at = new Date(Date.UTC(+m[1]!, +m[2]! - 1, +m[3]!, +m[4]!, +m[5]!, +m[6]!));
  if (Number.isNaN(at.getTime())) return { text: name, canonical: name, localized: false };
  return {
    text: name.replace(DERIVED_INSTANT, `--${humanInstant(at, opts)}--`),
    canonical: name,
    localized: true,
  };
}
