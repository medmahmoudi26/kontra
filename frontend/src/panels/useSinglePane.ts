/**
 * One Terminal, streamed, outside the wall.
 *
 * WHY THIS IS NOT `DashboardPage`'s CONNECTION. The Monitor's socket carries a whole wall: it
 * reconciles a subscription set against a filter, tracks per-tile health, modes, elision counts and
 * a live-attach budget, and its `sync()` is a function of page state. All of that is right for sixty
 * tiles and is the wrong shape for one — a page that embeds a single pane beside an editor would be
 * importing a wall's worth of bookkeeping to draw a rectangle.
 *
 * IT SENDS `focus` NOW, AND THAT REVERSES A DOCUMENTED DECISION — deliberately, and narrowly.
 *
 * This used to be snapshot-only on ADR 0020's cost model: a live attach is an sshd session, a PTY
 * and a per-viewer tmux session ON THE MACHINE, the wall is snapshots, and an embedded pane that
 * quietly went live would spend that budget without anyone choosing to. That reasoning is about a
 * FLEET Machine and it still holds there.
 *
 * It does not describe this pane. The pane beside an editor is the worker on THIS host — a local
 * tmux client, no sshd, no droplet — and it is the one pane an operator is deliberately looking at,
 * not one of sixty on a wall. The budget the rule protects is not being spent.
 *
 * WHAT THE RULE COST: a snapshot is `capture-pane`, a dump of the remote screen at THE REMOTE
 * SCREEN'S size, so it has no geometry of its own. MEASURED in the workbench — a tile that had
 * measured 135×34 painting a ~62-column screen, wrapping mid-word, because 62 was the width on the
 * other end. Resizing the pane could never rewrap a line, and no amount of fixing the layout would
 * have changed that.
 *
 * ONE SOCKET PER MOUNTED PANE. That is a real cost and a bounded one: this surface shows the pane of
 * the ONE workflow being edited, so the count is zero or one. The streamer already serves several
 * browsers.
 */

import { useCallback, useEffect, useRef, useState } from 'react';
import { decodeTagged, fetchTicket, panelBase, panelSocketUrl } from './panelsClient';
import type { ServerMessage } from './panelsClient';
import { reconnectDelay } from './reconnect';
import type { TileMode } from './TerminalTile';

export interface SinglePane {
  /** The shape `TerminalTile` takes: register for this Terminal's bytes, get an unregister back. */
  subscribe(id: string, write: (payload: Uint8Array) => void): () => void;
  /** What the streamer last said this Terminal is. `snapshot` until it says otherwise. */
  mode: TileMode;
  /** Set when the pane cannot be streamed, in words that name what to fix. `null` while it works. */
  error: string | null;
  /** True once a frame has actually arrived — "connected" is not the same as "painting". */
  painting: boolean;
  /**
   * True while the socket has dropped and is being retried (slice 05). The pane says so rather than
   * freezing silently on its last frame; a reconnect reseeds from a fresh snapshot, so it neither loses
   * nor duplicates output.
   */
  reconnecting: boolean;
  /**
   * Ask for a LIVE attach at this many columns and rows, or re-ask at a new size.
   *
   * THIS IS WHAT MAKES A PANE REFLOW. A snapshot is `capture-pane` — a dump of the remote screen at
   * THE REMOTE SCREEN'S size — so there is no client and no size to change, and resizing the box
   * around it can never rewrap a line. MEASURED, in the workbench: a tile that had measured 135×34
   * was painting a ~62-column screen, wrapping mid-word, because 62 was the width of the pane on the
   * other end. Nothing was broken; a snapshot simply has no geometry of its own to move.
   *
   * A live attach does have one. The streamer `stty`s a PTY before `tmux attach`, and re-focusing at
   * a new size tears that attach down and opens another — see `panels/server.ts:onFocus`, which
   * already implements exactly this and was only ever called by the Monitor's "Go live" button.
   */
  focus(cols: number, rows: number): void;
}

/**
 * Stream one Terminal by id. `null` tears the socket down.
 *
 * The id is a WHOLE Terminal id (`local:host/session/window`), because that is what the streamer
 * addresses and what the tile renders. A caller that only knows a session name has to resolve it
 * against the inventory first — see `CatalogPage.usePane`, which is that resolution and existed
 * before this did.
 */
export function useSinglePane(id: string | null): SinglePane {
  const [mode, setMode] = useState<TileMode>('snapshot');
  const [error, setError] = useState<string | null>(null);
  const [painting, setPainting] = useState(false);
  const [reconnecting, setReconnecting] = useState(false);

  /**
   * The tile's writer, held in a ref.
   *
   * A ref rather than state for the same reason the wall uses one: frames arrive faster than React
   * should re-render, and this is a hand-off to an imperative canvas. It is also what lets the
   * socket effect run ONCE per id while the tile mounts and unmounts freely inside it.
   */
  const writerRef = useRef<((payload: Uint8Array) => void) | null>(null);

  /**
   * The open socket, and the last size asked for.
   *
   * The socket is a ref because `focus` is called from a `ResizeObserver` inside the tile — outside
   * React's world — and must reach whatever connection is current without re-running the effect that
   * owns it. The size is remembered so a repeated measurement of the SAME geometry does not re-ask:
   * a re-focus at a new size tears down an sshd session, a PTY and a grouped tmux session and builds
   * three more, so asking twice for 135×34 would cost a reconnect and change nothing.
   */
  const socketRef = useRef<WebSocket | null>(null);
  const askedRef = useRef<{ cols: number; rows: number } | null>(null);

  const focus = useCallback(
    (cols: number, rows: number): void => {
      const ws = socketRef.current;
      // A pane that has not laid out measures 0×0, and a remote screen sized 0×0 is a blank tile
      // with no error anywhere — the same refusal `TerminalTile.measureNow` makes.
      if (!ws || ws.readyState !== WebSocket.OPEN || !id || cols <= 0 || rows <= 0) return;
      const asked = askedRef.current;
      if (asked && asked.cols === cols && asked.rows === rows) return;
      askedRef.current = { cols, rows };
      ws.send(JSON.stringify({ t: 'focus', id, cols, rows }));
    },
    [id]
  );

  useEffect(() => {
    setMode('snapshot');
    setError(null);
    setPainting(false);
    setReconnecting(false);
    askedRef.current = null;
    if (!id) return;

    let live = true;
    let socket: WebSocket | null = null;
    let attempt = 0;
    let timer: ReturnType<typeof setTimeout> | null = null;

    /**
     * Retry a dropped socket on the shared backoff (slice 05, criterion 3).
     *
     * Only ever from the SOCKET's own drop — a socket that existed and went away — never from the
     * ticket-mint failure, which is "the streamer is switched off" and keeps its verbatim error. A
     * reconnect reseeds from a fresh snapshot (`applyFrame` resets on a repaint), so it neither loses
     * nor duplicates: the browser holds no bytes across the gap.
     */
    const scheduleReconnect = (): void => {
      if (!live || timer !== null) return;
      setReconnecting(true);
      setPainting(false);
      timer = setTimeout(() => {
        timer = null;
        attempt += 1;
        void open();
      }, reconnectDelay(attempt));
    };

    const open = async (): Promise<void> => {
      try {
        const { ticket } = await fetchTicket();
        if (!live) return;
        const ws = new WebSocket(panelSocketUrl(panelBase(), ticket));
        ws.binaryType = 'arraybuffer';
        socket = ws;
        socketRef.current = ws;
        // A new connection has asked for nothing yet, so the next measurement must be sent even if
        // it repeats the last one this pane made on the previous socket.
        askedRef.current = null;

        ws.onopen = () => {
          if (!live) {
            ws.close();
            return;
          }
          // A clean open: the backoff resets and the reconnecting notice clears.
          attempt = 0;
          setReconnecting(false);
          setError(null);
          // cols/rows are ignored by `subscribe` — a snapshot is whatever size the remote pane is —
          // so nothing is measured before asking, and a tile that has not laid out yet still paints.
          ws.send(JSON.stringify({ t: 'subscribe', id, cols: 0, rows: 0 }));
        };

        ws.onmessage = (event: MessageEvent<unknown>) => {
          if (!live) return;
          if (event.data instanceof ArrayBuffer) {
            const frame = decodeTagged(event.data);
            // A frame carries its id: one socket could in principle be told about others, and a
            // pane must never paint another Terminal's screen.
            if (frame.id !== id) return;
            setPainting(true);
            writerRef.current?.(frame.payload);
            return;
          }
          if (typeof event.data !== 'string') return;
          const msg = JSON.parse(event.data) as ServerMessage;
          if (msg.t === 'state' && msg.id === id) {
            setMode(msg.mode);
            if (msg.mode === 'error') setError('the streamer cannot stream this pane');
          } else if (msg.t === 'error' && (!msg.id || msg.id === id)) {
            setError(msg.message);
          }
        };

        ws.onerror = () => {
          if (!live) return;
          setError(
            `the pane socket failed (${panelBase()}). Is the streamer's port published, and is this ` +
              'origin in KONTRA_PANEL_ORIGIN?'
          );
          scheduleReconnect();
        };
        ws.onclose = () => {
          if (socketRef.current === ws) socketRef.current = null;
          askedRef.current = null;
          if (!live) return;
          setPainting(false);
          // Say so and come back on our own, rather than freezing silently on the last frame.
          scheduleReconnect();
        };
      } catch (err) {
        // The ticket mint is where an unconfigured deployment surfaces, and its message names the
        // process to fix. Kept verbatim rather than replaced with "could not connect", and NOT
        // reconnected — a mint that 503s is switched off, not a dropped socket.
        if (live) setError(err instanceof Error ? err.message : String(err));
      }
    };

    void open();

    return () => {
      live = false;
      if (timer !== null) clearTimeout(timer);
      // Closed on unmount, ALWAYS. A socket nobody holds is one the streamer keeps taking
      // snapshots for — one exec per cadence, per abandoned pane, forever.
      socket?.close();
    };
  }, [id]);

  const api = useRef<SinglePane>({
    subscribe: (_id, write) => {
      writerRef.current = write;
      return () => {
        if (writerRef.current === write) writerRef.current = null;
      };
    },
    mode: 'snapshot',
    error: null,
    painting: false,
    reconnecting: false,
    focus: () => undefined,
  });
  // The handle is stable (the tile registers once) while the reported facts follow state.
  api.current.mode = mode;
  api.current.error = error;
  api.current.painting = painting;
  api.current.reconnecting = reconnecting;
  api.current.focus = focus;
  return api.current;
}
