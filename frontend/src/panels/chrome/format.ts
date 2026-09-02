/**
 * The words the chrome puts on numbers (slice 7a).
 *
 * Here rather than inline in three components because the status bar, the tile header and the sidebar
 * all say "how old" and "how many bytes", and three inline formatters is how a wall ends up reporting
 * `4s`, `4 sec` and `00:04` for the same measurement.
 */

/**
 * How long ago, in the coarsest unit that is still honest.
 *
 * NEVER `0s`. A snapshot that arrived 200 ms ago reads as `just now`, because `0s` invites the reading
 * "no time has passed", which on a page whose whole subject is staleness is the wrong end of the stick.
 * Above a minute the seconds are dropped: the operator's question at that point is "has this stopped?",
 * not "was it 71 or 79 seconds".
 */
export function ageWords(ms: number | null): string {
  if (ms === null) return 'never';
  if (ms < 1000) return 'just now';
  const seconds = Math.floor(ms / 1000);
  if (seconds < 60) return `${seconds}s ago`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  return `${Math.floor(hours / 24)}d ago`;
}

/** Bytes, in binary units, because this counts a byte cap and the cap is written in KiB. */
export function byteWords(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(bytes < 10 * 1024 ? 1 : 0)} KiB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MiB`;
}
