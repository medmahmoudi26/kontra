/**
 * WebSocket admission: single-use, ~30-second tickets (ADR 0020).
 *
 * A browser cannot put an `Authorization` header on a WebSocket handshake, so the token has to
 * reach the socket some other way. A token in the query string is a token in every access log,
 * every `Referer` and every proxy buffer — so the token buys a TICKET over an ordinary POST, and
 * the ticket is what appears in the URL: opaque, one connection, thirty seconds.
 *
 * Deliberately NOT `KONTRA_STATE_TOKEN`. That token also authorises
 * `POST /api/infra/stacks/:fqn/:op`, and a credential a browser holds must not be able to spend
 * money. `KONTRA_PANEL_TOKEN` mints tickets and nothing else.
 */

import { randomBytes, timingSafeEqual } from 'node:crypto';

/** How long a minted ticket is good for. Long enough to survive a page's own latency, short
 * enough that a leaked URL is worthless by the time it is read. */
export const TICKET_TTL_MS = 30_000;

/** Refuse to hold more than this many unredeemed tickets. A ticket mint is authenticated, so this
 * is not a DoS guard so much as a leak guard: unbounded growth here would be a slow one. */
export const MAX_OUTSTANDING = 512;

export interface Ticket {
  ticket: string;
  expiresAt: number;
}

export class TicketBook {
  private live = new Map<string, number>();

  constructor(
    private readonly now: () => number = () => Date.now(),
    private readonly ttlMs: number = TICKET_TTL_MS
  ) {}

  get outstanding(): number {
    return this.live.size;
  }

  mint(): Ticket {
    this.sweep();
    if (this.live.size >= MAX_OUTSTANDING) {
      // Drop the oldest rather than refuse: the caller is authenticated, and a stuck book would
      // lock a legitimate operator out of the wall.
      const oldest = this.live.keys().next();
      if (!oldest.done) this.live.delete(oldest.value);
    }
    // 32 bytes of CSPRNG. base64url so it survives a query string untouched.
    const ticket = randomBytes(32).toString('base64url');
    const expiresAt = this.now() + this.ttlMs;
    this.live.set(ticket, expiresAt);
    return { ticket, expiresAt };
  }

  /**
   * Spend a ticket. True at most once per ticket, and never after it expires.
   *
   * Constant-time compare against each candidate: the map lookup alone would leak, through timing,
   * whether a guessed prefix was getting warmer.
   */
  redeem(candidate: string | undefined | null): boolean {
    this.sweep();
    if (!candidate) return false;
    for (const [ticket, expiresAt] of this.live) {
      if (!timingSafeEqualStr(ticket, candidate)) continue;
      this.live.delete(ticket);
      return expiresAt > this.now();
    }
    return false;
  }

  sweep(): void {
    const t = this.now();
    for (const [ticket, expiresAt] of this.live) {
      if (expiresAt <= t) this.live.delete(ticket);
    }
  }
}

function timingSafeEqualStr(a: string, b: string): boolean {
  const ab = Buffer.from(a);
  const bb = Buffer.from(b);
  return ab.length === bb.length && timingSafeEqual(ab, bb);
}
