# Dashboard

A wall of **read-only Terminals** over the fleet's tmux, in the orchestrator web app. Pick selectors,
arrange the slots, and watch what every Worker is actually printing — without an `ssh` for each one.

Decided and justified in **[ADR 0020](../adr/0020-dashboard-read-only-terminals-screen-attach.md)**,
whose Context is six measurements; several of them killed a design that looked right on paper, so read
it before changing the transport.

## The two words

- **Terminal** — a read-only view of **one window of one Machine's tmux session**, identified
  `fleet:kf-crawl-01/kontra-webcrawl/actor`. Narrower than a tmux *pane*. (The two words it used to
  be confused with are both gone: **Monitor** was a node kind and is now the web surface this page
  describes, and `kontra console` was the REPL over the deleted dispatch routes.)
- **Dashboard** — a saved arrangement of Terminals. Grafana's boards are **Mission Control**; this is
  not that, and neither is a replacement for the other.

## What it is not

**A Terminal is not a record.** It is a screen: snapshots are lossy by construction, and a live attach
is lossier. The **Manifest**, the journal on the Machine and the lake are the record — a panel is
never evidence for what a Worker did. The page says so in its own header, deliberately.

It is also not metrics (that is Mission Control in Grafana) and not run state (that is
`kontra runs`, heartbeats and `GET /api/runs/:id/lifecycle`).

## You cannot type in it

There is no route, no message type and no code path that puts bytes on a channel to a Machine. That
is a property of *our* code, not of tmux: ADR 0020's finding (3) measured `run-shell` and `send-keys`
both executing through a **read-only** (`-r`) tmux client with no error at all. `-r` gates a client's
keystroke handling, not tmux commands, and `run-shell` is arbitrary command execution as root.

Copying text out is fine. Typing in is not, and not by configuration.

## How it works

```
browser ──1 multiplexed WebSocket──▶ streamer (forked child of orchestrator-infra)
   │                                    │
   └──POST /api/panels/ticket──▶ orchestrator-api        ssh / docker exec / local
      (same origin, no credential)      │                        │
                                        └── capture-pane ────────┴──▶ tmux on the target
                                            PTY + tmux attach -r  (only when focused)
```

- **The wall is snapshots.** One `capture-pane` exec per Machine per interval paints every tile on
  it. Only a **focused** or explicitly pinned Terminal becomes a live PTY attach, capped at four —
  because each live tile is an `sshd` session and a PTY on an `s-1vcpu-2gb` Machine.
- **Sessions are a converge**, not a deploy-time flag: `tmuxSessionWorkflow` on the infra queue, one
  writer per Machine. A Machine with no session is a *visible tile offering to create one*, never an
  absence. There is no polling reconciler — the read path is the drift detector.
- **Execution mode is first-class**: `fleet` (ssh), `docker` (`docker exec`), `local` (this host's
  tmux server, which is kontra's dev environment). One transport seam, three implementations, and the
  mode is part of a Terminal's id so a Dashboard slot can select on it.

## The five health signals

Never collapsed into one light, because `unknown` and `ok` rendering the same is the bug this
project has already paid for (see `heartbeat.ts`, and the round-3 incident in ADR 0020):

| signal | `ok` | what a failure means |
|---|---|---|
| **reachable** | the last `list-panes` probe reached the Machine | the Machine did not answer at all |
| **session** | present | `absent` (converge it) or `no-tmux` (the image or Machine has none) |
| **process** | `running` | `exited` — the Worker finished and the hold shell is keeping the window open |
| **poller** | live | `none` = *registered but nothing polling* — the trap `kontra workers list` exists for |
| **loads** | ok | `failing` = units are being isolated, i.e. the run is losing work |

**`process` is the fifth signal, and it is not `session` again.** A session can be perfectly present
while the Worker inside it has exited — the hold keeps the window open on purpose so the exit status
stays readable, so "finished" looks exactly like "running" from every other angle: the session is
there, the screen is full of output, the tile paints. `unknown` is common here and honest, because
`pane_current_command` reports the hold *shell* whether the Worker is running or finished; `exited`
is only ever claimed from `pane_dead`, from the `@kontra_exit` the hold shell writes, or from the
exit banner on screen.

Every failing signal carries a sentence, and `unknown` is a first-class value that is never drawn as
healthy. **The row drops the axes a pane's mode cannot answer** and leads with the one that is
failing — a `docker` or `local` pane is not told its metrics backend is missing, and a node the fleet
never scraped is not told its `loads` are bad. Note that **`loads` cannot say `ok` yet**: the ratio it
needs was unbuildable until the missing metric call sites landed, so until those have a real window
behind them it reports `failing` or `unknown`. That story is in ADR 0020's Consequences.

## The anatomy of a tile

The tile is **header, screen, status bar — in that order**, and the bar is at the **foot**, where
tmux draws it. That ordering is a height budget as much as a preference: `grid/wall.ts`'s
`WALL_ROW_PX` is measured against the chrome a tile carries, so a third chrome row would cost every
tile on the wall a line of a Worker's output. The header gave its second row back when the bar
arrived.

**There is no chip row.** The tile does not draw one at all — the band at the foot carries the
finding the chips used to, in the same words, and goes amber and *names* the failing signal rather
than showing green beside red chips. It is green only when there is nothing to report, and an
**unmeasured signal is never rendered as a healthy one**. A bare tile has no bar, for the same reason
it has no banner.

A snapshot pane **fills its tile** — the type scales, so nothing wraps and no column is dead. Panes
are pinned at 120×40 (`cli/tmux.go`), down from 200×50; a viewer never resizes the Worker it is only
looking at.

## Hiding a pane, and scrollback

- **A pane can be hidden**, and hiding one **stops paying for it** rather than merely not drawing it:
  the hidden pane drops out of the wanted set and the socket sends an `UNSUBSCRIBE`. The grain is the
  **window**, not the session it lives in. The wall says how many are hidden, says *what* they are,
  says they stopped costing anything, and every name is the way back — with one control to restore
  them all. A hidden pane the streamer has stopped reporting is still listed, and marked. It reads as
  a qualifier, not as a failure. The set is stored per-browser, versioned in the *value* so clearing a
  key by hand is not the migration path, and **fails open** on anything it cannot read.
- **The frontend holds its own scrollback ring**, capped at a stated **2,000 lines** — a concrete
  number, not an unbounded buffer — and the page says what survives a reconnect, which is the design
  question tmux does not answer for a read-only viewer.

## Usable without tmux shortcuts

The wall is driven **by mouse alone**. It is a read-only viewer for people who do not have tmux
keybindings in their fingers, so nothing on it requires a prefix key.

## Using it

```bash
kontra up                       # the control plane, including the streamer
open http://localhost:8088      # → Dashboard
kontra panels list              # the same inventory, four health columns, in a terminal
kontra doctor                   # prints the Dashboard's URL and probes the streamer
```

`KONTRA_PANEL_TOKEN` must be set for `orchestrator-api`, which mints the browser's WebSocket ticket
**same origin** so the page holds no credential of its own. With no token every panel route answers
503 and serves nothing — fail-closed, the same way `auth.ts` treats the state token. Do not point
`KONTRA_STATE_TOKEN` at this: that one also authorises `POST /api/infra/stacks/:fqn/:op`, and a
credential a browser can reach must not be able to spend money.

| var | default | |
|---|---|---|
| `KONTRA_PANEL_PORT` | `8090` | the streamer's port |
| `KONTRA_PANEL_TOKEN` | unset | required; unset ⇒ 503 everywhere |
| `KONTRA_PANEL_MODES` | `fleet,docker,local` | which discoveries run |
| `KONTRA_PANEL_SNAPSHOT_MS` | `3000` | wall repaint cadence |
| `KONTRA_PANEL_DISCOVER_MS` | `30000` | inventory + health refresh |
| `KONTRA_PANEL_TERM` | `xterm` | the attach's `TERM`; unset upstream means tmux refuses to attach |

## If a tile is blank

Read the status band at the foot before the pane — it names which of four identical-looking blanks
you are looking at:

- **ssh fails** → the Machine is gone or unreachable; the wall is not lying, the box is.
- **session absent** → converge it from the tile.
- **session `no-tmux`** → that target has no tmux at all (a worker *container* normally does not).
- **poller `none`** → something is registered and nothing is polling; the pane may be quiet because
  the Worker is not working, which is exactly the state a green run can hide.
