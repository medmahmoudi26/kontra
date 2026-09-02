/**
 * The health chips (ADR 0020, slice 3).
 *
 * FOUR SIGNALS, FOUR ELEMENTS, NEVER ONE LIGHT. SSH reachable · session present · poller live ·
 * actor-host reload ratio. The case is the round-3 incident — one Machine failing 81 of 82 resource
 * loads while its run reported `completed` — where a single rolled-up indicator would have been
 * green, because three of the four signals were fine. Collapsing them is the bug; keeping them apart
 * is the feature.
 *
 * WHAT THIS ROW OWES A READER, AND WHAT IT DOES NOT. Three rules were added after an operator read a
 * real tile and found that four of its five chips told them nothing:
 *
 *   1. NOT APPLICABLE IS NOT UNKNOWN. An axis that cannot say anything about a pane in this mode is
 *      not a chip — `ssh: reachable` on a `local` pane is the page having loaded, not a health fact,
 *      and `loads` has no vmagent anywhere but the fleet. {@link signalApplies} owns the rule and
 *      states the evidence. Those axes are named in one muted `n/a` marker, so an operator can see
 *      that an axis was dropped and why; they are also still in the drawer, in full sentences.
 *
 *      THIS IS NOT THE COLLAPSE ADR 0020 FORBIDS, and the guard that keeps it honest is that
 *      omission is one-directional: a signal reading `bad` is rendered whatever its applicability
 *      says. The only thing this row can ever hide is a green or an unknown on an axis that is
 *      structurally incapable of being anything else. `unknown` is never rendered as `ok`.
 *
 *   2. SEVERITY LEADS. Chips are ordered `bad`, then `unknown`, then `ok`, stable within each group
 *      by {@link CHIP_SIGNALS}. A row where everything is fine reads as a quiet run of green; a row
 *      with a finding puts it first. It is also what makes rule 3 safe.
 *
 *   3. ONE LINE, ALWAYS. The row is `flex-nowrap` and clips, so a narrow tile can never wrap a lone
 *      chip onto a second row or leave a half-empty one — the wall packs many tiles and every pixel
 *      here is a line of output an operator cannot see. Clipping is only tolerable because of rule
 *      2: what falls off the right is always the least severe thing on the tile, and the container's
 *      `title` carries the whole reading.
 *
 * The SENTENCES moved out of the chips to make that possible. `HealthEntry.label` is `<signal>:
 * <state>`; `HealthEntry.text` is the sentence with the clause that says what to do, and it goes to
 * the tooltip and the drawer. The chips used to carry the sentence, which is how the row reached 91
 * characters and wrapped.
 *
 * `unknown` IS VISUALLY DISTINCT FROM `ok`, and not by shade. A lighter green is how "we don't know"
 * reads as "fine" at a glance across a wall of tiles, so the three states differ on four independent
 * channels at once — hue, fill, border style and glyph — which also means the distinction survives
 * greyscale, colour-blindness and a squinting operator. `heartbeat.ts` states the underlying rule:
 * "unknown" and "zero" must stay distinguishable.
 *
 * IT TAKES `health` AND RENDERS. No fetch, no socket, no state. The streamer is the only thing that
 * knows any of this, it arrives on `{t:'state'}` and `GET /api/panels/terminals`, and a component
 * that measured anything itself would be a second, disagreeing source of truth.
 *
 * The hooks are contract, per CONTRACT.md amendments 12 and 14: `data-testid="chip-<signal>"`,
 * `data-state` the normalized tri-state `ok|bad|unknown`, and `data-value` the RAW enum
 * (`present`, `no-tmux`, `none`, `failing`, …) so a spec can assert the specific failure without a
 * second vocabulary. Never select on a Tailwind class — restyling a chip must not read as a broken
 * stream.
 */

import {
  healthEntries,
  inapplicableReason,
  type HealthEntry,
  type HealthSignal,
  type TerminalHealth,
} from './panelsClient';

/**
 * The five, in the order a tile shows them: the three an SSH round trip answers, then the two that
 * come from Temporal and VictoriaMetrics. `detail` is not a signal and is not a chip.
 *
 * `process` SITS BESIDE `session` because that is the pair an operator confuses. "The session is
 * present" is not "the Worker is running": `cli/tmux.go` holds a window open after its command
 * exits, on purpose, so a finished Worker leaves a present session, a full screen and a tile that
 * paints. On a clipped one-row chip strip these are the three that survive, which is the right three.
 */
export const CHIP_SIGNALS = ['reachable', 'session', 'process', 'poller', 'loads'] as const;
export type ChipSignal = (typeof CHIP_SIGNALS)[number];

/** The normalized tri-state. Three, because a two-state chip cannot show the difference between
 * "measured and fine" and "nobody looked". */
export type ChipState = 'ok' | 'bad' | 'unknown';

/**
 * `health.detail` carries one sentence per failing signal, joined by this separator on the server
 * (`panels/health.ts:DETAIL_SEPARATOR`). Splitting here is what keeps two failures from rendering as
 * one run-on paragraph.
 */
export const DETAIL_SEPARATOR = ' · ';

export function detailLines(detail: string | undefined): string[] {
  if (!detail) return [];
  return detail
    .split(DETAIL_SEPARATOR)
    .map((s) => s.trim())
    .filter((s) => s !== '');
}

/** The raw enum behind one signal — what `data-value` carries. `unknown` for a signal a streamer
 * older than this build did not send, never a blank attribute that a spec would read as absent. */
export function rawValue(health: TerminalHealth, signal: ChipSignal): string {
  return health[signal] ?? 'unknown';
}

export function chipState(ok: boolean | null): ChipState {
  // `null` FIRST and explicitly. `ok === null ? … : ok ? …` is the whole guard: a truthiness test
  // here would fold `unknown` into `bad`, and a boolean field would have folded it into `ok`.
  return ok === null ? 'unknown' : ok ? 'ok' : 'bad';
}

/**
 * Four channels of difference per state, so no two states are the same thing at different opacity.
 *
 * `unknown` is the one that matters: a DASHED border and no fill say "this is not a reading" in a way
 * that a colour cannot, because a wall of tiles is skimmed rather than read.
 *
 * EXPORTED so a second surface can say the same three states in the same four channels rather than
 * inventing a fourth palette — `panels/ActorWorkers.tsx` renders an Actor's rolled-up health as one
 * chip, and an `unknown` there that looked like a paler `ok` would be the exact collapse ADR 0020
 * forbids, reintroduced one page over.
 */
export const STATE_CLASS: Record<ChipState, string> = {
  ok: 'border-solid border-emerald-500/60 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300',
  bad: 'border-solid border-red-500/70 bg-red-500/15 text-red-700 dark:text-red-300',
  unknown: 'border-dashed border-zinc-400/80 bg-transparent text-zinc-500 dark:text-zinc-400',
};

/** Decorative, and marked so: the chip's words already say the state, and a screen reader reading
 * "black circle" adds nothing. */
export const STATE_GLYPH: Record<ChipState, string> = { ok: '●', bad: '▲', unknown: '?' };

/**
 * WHAT LEADS THE ROW. `bad` first because it is the only thing anyone has to act on, `unknown`
 * second because "we cannot see" is a weaker finding than "it is broken" and a stronger one than
 * "it is fine", `ok` last so a healthy tile is a quiet run of green.
 *
 * It is also the safety property behind the one-line rule: the row clips rather than wraps, and this
 * order is the proof that what gets clipped is always the least severe chip on the tile.
 */
const SEVERITY: Record<ChipState, number> = { bad: 0, unknown: 1, ok: 2 };

/** One chip's whole reading, before anything is decided about where it goes. */
interface Chip {
  signal: ChipSignal;
  state: ChipState;
  /** The chip's words — `<signal>: <state>`. */
  label: string;
  /** The full sentence, for the tooltip and the drawer. */
  text: string;
  value: string;
  applies: boolean;
  /** Present only for an axis that does not apply: why not, in one sentence. */
  naReason?: string;
}

/**
 * Every signal, read once — applicability, state, both sets of words.
 *
 * Exported and pure so the ORDER and the OMISSION are assertable without a DOM. A screenshot-shaped
 * test on this beats a green boolean, because the complaint that produced it was about what the row
 * looks like.
 */
export function chipsFor(health: TerminalHealth, mode = 'fleet'): Chip[] {
  const entries = healthEntries(health, mode);
  const bySignal = new Map<HealthSignal, HealthEntry>(entries.map((e) => [e.signal, e]));
  return CHIP_SIGNALS.map((signal) => {
    const entry = bySignal.get(signal);
    // Defensive rather than decorative: a streamer built before a signal existed sends a health
    // object without it, and the honest chip for that is `unknown`, not a missing one.
    const chip: Chip = {
      signal,
      state: chipState(entry ? entry.ok : null),
      label: entry?.label ?? `${signal}: unknown`,
      text: entry?.text ?? `${signal}: unknown (not reported)`,
      value: rawValue(health, signal),
      applies: entry?.applies ?? true,
    };
    const why = inapplicableReason(signal, mode);
    if (why !== undefined) chip.naReason = why;
    return chip;
  });
}

/**
 * Does this chip earn a place in the row?
 *
 * ONE-DIRECTIONAL BY CONSTRUCTION, and that is what keeps rule 1 inside ADR 0020 rather than beside
 * it: a `bad` reading is shown whatever its applicability says, so the only thing an omission can
 * ever hide is a green or an unknown on an axis that cannot be anything else. The round-3 shape —
 * one signal failing while the rest are fine — is un-hideable here.
 */
export function chipIsShown(chip: Pick<Chip, 'state' | 'applies'>): boolean {
  return chip.applies || chip.state === 'bad';
}

/** The row, in the order it renders: severity first, then {@link CHIP_SIGNALS}' own order, so two
 * tiles in the same state always read the same way. */
export function orderChips(chips: readonly Chip[]): Chip[] {
  return chips
    .filter(chipIsShown)
    .map((chip, i) => ({ chip, i }))
    .sort((a, b) => SEVERITY[a.chip.state] - SEVERITY[b.chip.state] || a.i - b.i)
    .map(({ chip }) => chip);
}

/** What one failing signal reads as, for a surface that has room for a finding but not for a row. */
export interface Finding {
  signal: ChipSignal;
  /** `<signal>: <STATE>` — the chip's own words, character for character. */
  label: string;
  /** The sentence with the clause that says what to do. */
  text: string;
}

/**
 * THE CHIP THIS TILE LEADS WITH, when that chip is a failing one — and `undefined` when nothing is.
 *
 * It exists so that `chrome/StatusLine` can stop being green while a chip on the same tile is red,
 * WITHOUT deciding for itself what "failing" means. This is `orderChips(chipsFor(…))[0]`: literally
 * the element the row renders first, so the bar and the chips cannot name different findings, use
 * different words for the same one, or disagree about which axis applies to a `local` pane.
 *
 * `unknown` IS NOT A FINDING. The bar is severity, not completeness — a tile whose `poller` was never
 * measured has nothing for an operator to act on, and an amber bar on every un-probed pane is how a
 * colour stops being read at all.
 */
export function leadingFinding(health: TerminalHealth, mode = 'fleet'): Finding | undefined {
  const first = orderChips(chipsFor(health, mode))[0];
  if (!first || first.state !== 'bad') return undefined;
  return { signal: first.signal, label: first.label, text: first.text };
}

/**
 * The first applicable signal that was never MEASURED — a different answer from a failing one, and
 * one the wall now has to carry itself.
 *
 * ADR 0020's rule is that `unknown` must never render as healthy. That used to be discharged by the
 * chip row: `poller: unknown` sat on the tile in its own tri-state, whatever the bar below it did.
 * With the row gone the bar is the only health reading left on the wall, and the bar asked only
 * `leadingFinding`, which reports `bad` and nothing else — so a pane with two unmeasured signals
 * rendered a solid tmux green, indistinguishable from one where all four had been checked and were
 * fine. That is exactly the collapse the ADR forbids, reintroduced by removing the row.
 *
 * Ranked BELOW a failure, because "this is broken" outranks "this was not checked" — and reported
 * separately rather than folded into `Finding`, so the bar can colour the two differently. An
 * operator needs to tell "go fix the handler" from "this number is not being collected".
 *
 * `process` IS EXCLUDED, and that exclusion is the difference between a useful signal and a second
 * useless one. A kontra Worker runs from a hold shell so that a crash leaves its exit status on
 * screen, and that shell owns the pane's foreground process group — so `pane_current_command`
 * reports `zsh` whether the Worker is alive or finished. `process: unknown` is therefore not a gap
 * in collection, it is the permanent and correct reading for every pane kontra starts. Treating it
 * as unmeasured would turn every tile on the wall slate forever, which tells an operator exactly as
 * little as the always-green bar this was written to fix — and would bury `poller` and `loads`,
 * the two that genuinely mean "nobody is collecting this". A process that has EXITED is a different
 * matter and `barTone` already makes that bar red.
 */
const NEVER_MEASURABLE = new Set(['process']);

export function leadingUnmeasured(health: TerminalHealth, mode = 'fleet'): Finding | undefined {
  const first = orderChips(chipsFor(health, mode)).find(
    (c) => c.state === 'unknown' && !NEVER_MEASURABLE.has(c.signal)
  );
  if (!first) return undefined;
  return { signal: first.signal, label: first.label, text: first.text };
}

export interface HealthChipsProps {
  health: TerminalHealth;
  /** The Terminal this describes, when there is more than one on screen — it makes the test hooks
   * unique per tile, which slice 4's wall needs. Omitted, the hooks stay `chip-<signal>`, which is
   * what slice 1's specs and the stub fixture already select. */
  terminalId?: string;
  className?: string;
  /**
   * THE EXECUTION MODE — `fleet | docker | local`, from `Terminal.mode` or the id's first segment.
   *
   * It is what decides which axes apply ({@link signalApplies}) and what the transport is called, so
   * a mode-less caller is told everything about everything. Defaults to `fleet`, the mode where all
   * five axes are real, so forgetting it can only ever add chips, never drop one.
   */
  mode?: string;
  /**
   * A tile with no room for prose.
   *
   * The chips are never dropped for density — they are the tri-state this component exists for; the
   * row is one line by construction and clips its least severe end. What `dense` clips is the
   * DETAIL, and only visually: both sentences stay in the DOM and the whole text moves to the
   * container's `title`, so an operator hovering, the drawer, and a spec all still get the full
   * reading. MEASURED: on a two-row tile the detail took 34 px of about 150 px of terminal, which is
   * a quarter of the output for prose that repeats on every tile of the same Machine.
   */
  dense?: boolean;
}

export default function HealthChips({
  health,
  terminalId,
  className,
  mode = 'fleet',
  dense = false,
}: HealthChipsProps): JSX.Element {
  const chips = chipsFor(health, mode);
  const shown = orderChips(chips);
  const na = chips.filter((c) => !chipIsShown(c));
  /**
   * PROSE ONLY WHEN THERE IS SOMETHING TO DO ABOUT IT.
   *
   * `health.detail` carries a sentence per signal that is not `ok`, which on a perfectly ordinary
   * tile means two or three paragraphs explaining why two axes are unknown — on every tile of every
   * node, forever. That is the noise that trains an operator to stop reading the row. When nothing
   * is failing the sentences ride in each chip's `title` and in the drawer, where a reader goes when
   * they want them; when something IS failing they are on the tile, unclipped, under the red chip.
   */
  const failing = shown.some((c) => c.state === 'bad');
  const lines = failing ? detailLines(health.detail) : [];
  const suffix = terminalId ? `-${terminalId}` : '';
  // The whole reading, for the row that clips. Every signal, applicable or not, in its full sentence.
  const rowTitle = chips
    .map((c) => c.naReason ?? c.text)
    .concat(health.detail ? [health.detail] : [])
    .join('\n');

  return (
    <div className={className} data-testid={`health${suffix}`} title={rowTitle}>
      {/*
        ONE LINE, ALWAYS — `flex-nowrap` + `overflow-hidden`. A wrapping row put a lone chip on a
        second line and left the dead space beside it; on a wall of tiles those rows are the pixels
        that were meant to be output. Safe only because `orderChips` puts severity first: the end
        that falls off is the healthy end, and the container's `title` still has all of it.
      */}
      <ul className="flex flex-nowrap gap-1.5 overflow-hidden text-xs">
        {shown.map((chip) => (
          <li
            key={chip.signal}
            data-testid={`chip-${chip.signal}${suffix}`}
            // The normalized tri-state specs assert on (amendment 12).
            data-state={chip.state}
            // The raw enum, riding alongside (amendment 14), so a spec can tell `absent` from
            // `no-tmux` without a second chip vocabulary.
            data-value={chip.value}
            // The signal's OWN sentence first, then the server's prose. `unknown` gets it too: "why
            // don't we know" is as actionable as "why is it broken". This is where the clause that
            // used to sit in the chip's words went — the label says WHAT, the tooltip says what to do.
            title={
              chip.state === 'ok'
                ? undefined
                : health.detail
                  ? `${chip.text}\n${health.detail}`
                  : chip.text
            }
            className={`inline-flex shrink-0 items-center gap-1 whitespace-nowrap rounded border px-1.5 py-0.5 ${STATE_CLASS[chip.state]}`}
          >
            <span aria-hidden="true">{STATE_GLYPH[chip.state]}</span>
            <span>{chip.label}</span>
          </li>
        ))}

        {na.length > 0 && (
          /*
            THE AXES THIS PANE HAS NO ANSWER FOR, named rather than silently missing.

            ADR 0020 permits omitting an inapplicable axis; it does not permit making one
            uninspectable. This marker is how the row itself says which axes it dropped and, on
            hover, why — the drawer says the same thing in full. Deliberately the quietest thing in
            the row and deliberately last: it is the first thing a narrow tile should lose.

            `data-state="n/a"` is a FOURTH word, not a fourth tri-state value: no spec that selects
            `[data-state="ok"]` can ever match it, which is what keeps "unknown never renders as
            healthy" true of this element too.
          */
          <li
            data-testid={`chip-na${suffix}`}
            data-state="n/a"
            data-value={na.map((c) => c.signal).join(',')}
            title={na.map((c) => c.naReason ?? `${c.signal}: not applicable`).join('\n')}
            className="inline-flex shrink-0 items-center gap-1 whitespace-nowrap rounded border border-transparent px-1.5 py-0.5 text-muted-foreground"
          >
            <span aria-hidden="true">–</span>
            <span>n/a: {na.map((c) => c.signal).join(', ')}</span>
          </li>
        )}
      </ul>

      {lines.length > 0 && (
        // One <li> per sentence, never a joined paragraph: the signals stay separate in the prose
        // for the same reason they stay separate in the chips.
        <ul
          data-testid={`health-detail${suffix}`}
          data-dense={dense ? 'true' : undefined}
          title={dense ? lines.join(' ') : undefined}
          className={`mt-1 space-y-0.5 text-xs text-muted-foreground ${
            dense ? 'max-h-4 overflow-hidden' : ''
          }`}
        >
          {lines.map((line, i) => (
            <li key={`${i}-${line}`} data-testid={`health-detail-line${suffix}`}>
              {line}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
