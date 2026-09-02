# 20. The Dashboard: read-only Terminals over the Fleet's tmux, screen-attached, in their own PID

## Status

**Accepted.** Extends ADR 0019: session existence on a Machine becomes a Temporal operation on the
same `kontra-infra` queue, and the process-isolation finding that ADR 0019 recorded as a comment is
here re-measured and made load-bearing. Revises `control/orchestrator/src/infra/programs/machine.ts` — the
`--tmux` systemd unit it installs is removed and replaced by a converge. Adds **Terminal** and
**Dashboard** to the Execution glossary. Supersedes no ADR.

## Context

A Worker on a fleet Machine is observable three ways today: metrics (vmagent → VictoriaMetrics →
Grafana's Mission Control boards), run state (`kontra runs`, heartbeats, the Manifest), and — if it
was deployed with `--tmux` — a tmux session on the Machine holding `journalctl -fu` for each half,
reachable only by `ssh` plus `tmux attach`. The third one is the only view of what a Worker is
actually printing, and it is the only one that requires a terminal, a key, and knowing the Machine's
address.

What is wanted is that view for the whole Fleet at once, in a browser, arranged by the operator.

Six measurements shaped the mechanism. They are recorded here because every one of them contradicts
a design that looks obviously correct on paper; the probes are described in
`.scratch/tmux-dashboard/NOTES.md`.

1. **tmux control mode (`tmux -C attach`) buffers without bound on the Machine — unless flow control
   is armed.** With the reading client stalled, the Machine's tmux server grew from 10 MB to 80 MB in
   five seconds at ~14 MB/s of pane output. The pane's writer was never throttled — this is
   retention, not backpressure: `%output` is a log of every byte, so a lagging client forces tmux to
   keep all of it. Fleet Machines are `s-1vcpu-2gb` running Chromium; the OOM killer's first choice
   there is the actor.

   **CORRECTED 2026-08-12, and the correction is mine.** This ADR originally recorded that
   `refresh-client -A … pause-after` did not bound the growth. That was a wrong-flag artifact:
   `-A` sets a *pane state* (`'<pane>:continue'`), while the threshold is a **client flag** set with
   **`-f`**. Read from `flplima/tmuxy` (`tmuxy-core/src/control_mode/monitor.rs:378`) and then
   measured on a private socket: with `refresh-client -f pause-after=1` armed, a 5-second stall grew
   the server by 760 KiB and **plateaued after the first second**, versus unbounded growth unarmed.
   So control mode is *tameable*, and finding (1) is no longer a disqualifier on its own. The
   transport decision stands on finding (3) instead — arming `pause-after` requires writing to the
   control client's stdin, so control mode mandates exactly the command channel that read-only is
   built on not having — plus the cost of replacing a shipped, tested PTY implementation.

2. **A stalled control-mode client crashed the tmux server.**
   `tmux: server[17770]: segfault at fffffffffffff07e` — not OOM (`oom_kill 0`, `systemd-oomd
   inactive`). A tmux server crash destroys every session on that socket. On tmux 3.3a; fleet
   Machines run 22.04's **3.2a**, which is older.

3. **`tmux attach -r` is not a security boundary.** Through a read-only control client, both
   `run-shell "touch …"` and `send-keys` into an interactive shell **executed**, and tmux returned
   no `%error`. `-r` gates a client's keystroke handling, not tmux commands, and `run-shell` is
   arbitrary command execution as the Machine's root.

4. **A screen-oriented client is bounded where a log-oriented one is not.** tmux owes an attached
   client only enough escape sequences to make a cols×rows screen correct, and can always satisfy a
   laggard with a full redraw. That is why a PTY + `tmux attach` cannot reproduce finding (1).

5. **Control mode is lossless at realistic rates and lossy under burst** — 0 of 1000 lines missing
   at ~100 lines/s; 21 of 5000 (0.4%) missing as fast as `sh` can write. Screen attachment is
   *more* lossy still, by design. Either way a Terminal is not a record.

6. **ADR 0019's process-global hazard is real on @pulumi/pulumi 3.256.0, and it fails silently.** An
   unhandled rejection, an uncaught throw, or an `EventEmitter` `'error'` with no listener, injected
   two seconds into an inline `up()`, made `up()` **throw** in every case, reporting the foreign
   error as `error: [runtime] Unhandled exception`. Listener counts go `0 → 1 → 0` around the
   update, confirming the mechanism. **The process survived every time** — so co-location does not
   crash a provisioner, it fails `kontra fleet deploy`, blames the streaming code in the
   provisioner's error, and leaves the provisioner reporting healthy.

## Decision

- **A Terminal is a read-only view of one window of one Machine's session**, identified
  `fleet:<machine>/<session>/<window>` — machine names are deterministic (`kf-<role>-NN`), so the id
  survives restarts and a saved **Dashboard** can point at it. A Dashboard is an arrangement of
  Terminals defined by *selectors* (role, campaign, actor, window) evaluated against the Fleet
  inventory, not a list of pinned hosts: `fleet up --count 10` must fill the wall without an edit.

- **No input path exists.** Enforcement is ours, not tmux's, per finding (3): the SSH command line
  is constructed entirely server-side, and no route, message type or code path can write to a
  session's channel. `-r` is kept as defence in depth for keystrokes only.

- **The Fleet owns session existence, as a Temporal converge.** A one-shot workflow,
  `workflowId = tmux-<machine>` on `INFRA_QUEUE`, SSHes in, ensures the session and its windows, and
  exits; signals recreate, kill, or add a window. It runs on the infra queue because only that
  process holds `KONTRA_SSH_KEY` and because creating a session on a Machine is Fleet authority —
  `CONTEXT-MAP.md` forbids a Run reaching a Machine.

  There is deliberately **no polling reconciler**: the read path is the drift detector. A Terminal
  that cannot attach reports "no session" and offers the converge, which costs nothing when nobody
  is looking and duplicates neither systemd nor the stack converge.

  Consequently `machine.ts` stops installing `kontra-tmux.service` and stops apt-installing tmux.
  That removes a silent failure — the unit was `ConditionPathExists=/usr/bin/tmux` and the installer
  tolerated apt failing, so a Machine could deploy "successfully" and never be viewable — and it
  means a Machine deployed **without** `--tmux` can be given a Terminal on demand, with no
  re-deploy. `--tmux` on `fleet deploy` degrades to "also converge a session after placement".

- **Bytes leave a Machine by PTY + `tmux attach -r` into a per-viewer grouped session**
  (`tmux new-session -d -s <viewer> -t <owner>`), never by control mode. Findings (1), (2) and (4)
  decide this; grouped sessions give each viewer its own selected window, size and zoom without
  disturbing the owner session or other viewers. The converge sets `window-size manual` with a fixed
  geometry — otherwise an operator running `tmux attach` on the box reflows every browser tile
  mid-stream — and raises `history-limit` above its 2000 default so a Terminal can be seeded with
  backlog on open.

- **The wall is snapshots; only a focused Terminal is a live attach.** One
  `capture-pane -p -e -S -<n>` exec per Machine, every few seconds over a `ControlMaster`'d SSH
  connection, paints every Terminal on that Machine at once. A live PTY attach happens on focus, or
  for a small number of explicitly pinned Terminals. Twelve Machines cost twelve periodic execs
  rather than twenty-four persistent PTYs and twenty-four `sshd` sessions on 2 GB boxes, and
  finding (1)'s hazard is confined to the few Terminals someone is actually watching.

- **The streamer is a forked child of `orchestrator-infra`, never the same PID**, per finding (6).
  Same container, same read-only key mount, same Fleet authority, separate process — the Pulumi
  handlers are process-global, not container-global. It is not `orchestrator-api`: that process
  binds `0.0.0.0`, is host-published, and is unauthenticated on most routes, and it must not hold
  the key that reaches every Machine in the campaign.

- **One multiplexed WebSocket per browser tab**, binary frames tagged with a Terminal id.
  Browsers cap ~6 concurrent HTTP/1.1 streams per origin, so per-tile SSE or streaming GETs
  deadlock a wall; one socket also puts backpressure accounting in one place. It is authenticated by
  a single-use, ~30-second ticket minted by `POST /api/panels/ticket` under a dedicated
  **`KONTRA_PANEL_TOKEN`**, failing closed with 503 exactly as `auth.ts` does. Deliberately not
  `KONTRA_STATE_TOKEN`: that token also authorises `POST /api/infra/stacks/:fqn/:op`, and a
  credential held in a browser must not be able to spend money.

- **Discovery is the Fleet inventory, decorated.** The streamer reads Pulumi stack outputs as a
  library (in-container, no HTTP, no token in the browser) for the Machine list with role, campaign,
  actor and version, then decorates each with a `list-panes` probe and Temporal poller liveness. A
  Machine that exists with no session is a visible tile, never an absence.

- **Four independent health signals per Terminal, never collapsed into one light**: SSH reachable,
  session present, poller live, and the actor host's failure ratio from VictoriaMetrics. The
  round-3 incident — 81 of 82 resource loads failing on one Machine while the run reported
  `completed` — is the case this exists for, and `heartbeat.ts` already establishes the rule that
  "unknown" and "zero" must stay distinguishable.

- **A Terminal is not a record.** Per finding (5) and by the nature of screen attachment, output on
  the wall is lossy. The Manifest, the journal on the Machine, and the lake are the record, and no
  UI copy may imply otherwise.

## Alternatives considered

- **tmux control mode over SSH, one connection per Machine.** This was the design for most of the
  interview, and its premise held: one connection does carry `%output` for every pane including
  non-active windows, several clients can read concurrently, and `kill-session` reports `%exit`
  cleanly. Rejected on findings (1), (2) and (3) — unbounded on-Machine buffering, a server crash,
  and a mandatory root command channel. Its remaining advantage, structured `%window-add` /
  `%layout-change` events, is replaced by the `list-panes` probe that discovery needs anyway.
  Revisiting it is now cheaper than this ADR first said: `refresh-client -f pause-after=<secs>`
  bounds finding (1), measured. What still argues against it is finding (3) — arming that flag means
  writing to the control client, and a channel that can carry `refresh-client` can carry `run-shell`,
  which executes as root. A read-only guarantee that rests on "we only ever write these two
  commands" is weaker than one that rests on "there is nothing to write to".

  **`flplima/tmuxy` (MIT) was evaluated as a replacement and declined**, not for correctness — it
  handles control mode properly, including `%pause`/resume flow control, which is where the
  correction above came from. It is declined because it is a six-crate **Rust** workspace with a
  Tauri app and a WASM target, against a TypeScript orchestrator in one already-1.12 GB image; it is
  **interactive by design** (`send-keys` in `control_mode`'s executor), which inverts the guarantee
  this ADR is built on; it streams **SSE**, which hits the browser's ~6-connection HTTP/1.1 cap that
  the multiplexed WebSocket exists to dodge; its remote support is **ssh-only**, where this design
  now has fleet/docker/local behind one transport seam; and it is `v0.0.10-alpha`. Worth reading
  again if control mode is ever revisited for structured events.

- **A per-Machine agent that dials the Controller** (the vmagent pattern, which is architecturally
  consistent and survives losing the SSH path or landing behind NAT). Rejected for v1 because it
  adds a binary to build, version, ship in the Bundle and supervise, plus a per-Machine credential,
  to solve a reachability problem the Controller does not currently have — it already SSHes to every
  Machine to place the actor. It remains the escape hatch if Machines ever lose that path.

- **Tail the journal directly and skip tmux** (`journalctl -n 200 -f -u kontra-actor`). Strictly
  better on fidelity and history, and it works whether or not a session exists. Rejected because
  tmux is what the operator already attaches to, because the same code path then serves ad-hoc
  sessions an operator made by hand, and because the converge makes session absence a solved
  problem rather than a reason to bypass it. The journal remains the record.

- **The streamer inside `orchestrator-infra`'s own process** (nothing new learns a secret) or
  **inside `orchestrator-api`** (one process, one origin, no CORS). Both rejected on finding (6) and
  on key scope respectively.

- **`Monitor` / `Console` / `Pane` as names.** All taken: `Monitor` is the node kind, and
  `CONTEXT.md` records that the CLI verb was renamed to `kontra runs` on 2026-08-07 specifically to
  end that collision; `kontra console` is the REPL; a tmux `pane` is a narrower thing than what a
  Terminal shows (a window).

## Consequences

- **Terminals are lossy and the docs must say so.** A wall that looks like a log invites someone to
  treat a Terminal as evidence for what a Worker did. Every place this surfaces — UI chrome, wiki,
  `kontra doctor` — states that the Manifest and the lake are the record.

- **Worker processes must never live inside a session a Terminal attaches to.** Finding (2) means a
  viewer can crash a Machine's tmux server; on the fleet that costs only the view, because
  `machine.ts` keeps the actor and handler under systemd and puts only journals in panes. This
  converts that file's existing comment from a preference into an invariant. It also makes the
  *local* `kontra serve --tmux` model the dangerous one — its panes hold the real processes — which is
  one reason local Terminals are out of scope in v1.

- **The `loads` signal was unbuildable as specified, and finding that out is part of this ADR's
  value.** The decision above says the fourth signal is "the actor host's failure ratio from
  VictoriaMetrics", which assumed the ratio existed. It did not.
  `kontra_isolated_units_total` is incremented on both hosts (`engine.py:273`, `engine.go:774`), but
  `kontra_resource_reloads_total` was incremented on the **Go host only** and
  `kontra_batches_total` — the denominator — was incremented **nowhere**: `countBatch()` and
  `count_batch()` were defined and called by tests alone. So the fleet dashboard's
  `rate(reloads) / clamp_min(rate(batches), 0.001)` was not a ratio. On a Go worker it evaluated to
  `1000 × rate(reloads)`, clamping to the panel's `max: 1` and showing red for one routine reload
  (`maxUnitReloads = 2`); on a Python worker it was a constant 0 and showed green forever — and the
  crawler that produced the round-3 incident is a Python actor. The panel that existed to catch that
  failure would have shown green while it recurred.

  Resolved by adding the three missing call sites (`count_batch()` in `run_batch`, `countBatch()` in
  `RunBatch`, `count_reload()` on Python's dead-resource path to match Go's) and by replacing the
  clamp with a guarded division that renders **no data** when no batches are running, because "no
  batches" is not "zero reloads per batch". Until those increments have been deployed long enough to
  have a window, the chip reports `failing` or `unknown` and never `ok` — a green chip derived from a
  counter nothing increments is the same class of lie one layer up.

- **The signals are FIVE, and the fifth one exists because a present session is not a running
  Worker.** The decision above says four, and four was one short in the way that matters: a session
  can be perfectly present while the process inside it has exited. `cli/tmux.go`'s hold keeps the
  window alive after the command returns — deliberately, so the exit status stays readable — so a
  finished Worker leaves a present session, a full screen, a tile that paints, and four green-ish
  chips. On this host, every kontra session was in exactly that state while the wall reported
  nothing wrong.

  `process` (`running | exited | unknown`) is therefore a signal of its own, beside `session`, and it
  keeps the rule the other four follow: never collapsed, and `unknown` never rendered as healthy.

  **The obvious detector is wrong in both directions, measured.** `pane_current_command` reports the
  hold SHELL for a pane whose Worker is running — the wrapper does not put the child in its own
  process group, so tmux reads the shell's pgid:

  ```
  $ tmux list-panes -a -F '#{session_name}:#{window_name} #{pane_pid} #{pane_current_command}'
  nscheck-0_1_0:actor 3535711 zsh
  $ ps -o pid,stat,comm --ppid 3535711
  3535713 Sl+  nscheck        # the Worker, up
  ```

  So "the command is a bare shell" would have called every healthy local Worker dead. `exited` is
  claimed only from `#{pane_dead}`, from the `@kontra_exit` pane option the hold shell now writes
  (`set-option -p -t "$TMUX_PANE"`, guarded by `$TMUX` — an untargeted `set-option -p` writes to
  whatever pane the server considers current, which was measured recording one window's exit status
  onto another window's pane), or from the `[exited N]` banner read off the snapshot the wall already
  takes. A shell with none of those is `unknown`, with the reason.

- **Every tile carries a status line, and the browser draws it.** tmux's own bar is drawn by the
  CLIENT, so `capture-pane` — which is what the wall runs — can never contain it: measured, a
  snapshot carries pane content only while a live attach carries the bar (`^[[30m^[[42m` — black on
  green) in its byte stream. The wall was therefore bar-less on most tiles and tmux-barred on the few
  that were live. The frontend now draws one bar for both feeds, saying what tmux's cannot — the
  pane's command, the pane's own geometry, and how old the frame is — and the live attach runs
  `set-option -t <viewer> status off` so a live tile does not end up with two. That option is set on
  the streamer's own grouped session; verified that the owner's `status` stays unset, so an operator
  attached on the box keeps their bar.

  **It sits at the FOOT of the tile, and its green is conditional.** Two corrections, both from the
  same reading: a bar drawn where tmux never draws one does not read as the object it is imitating,
  and a bar that is green regardless does not belong on a tile whose chips are severity-ordered.

  - *Position.* It was row 2 of the tile header; it is now the last element of the tile, edge to
    edge under the screen, styled `bg=green,fg=black` like tmux's default. `grid/wall.ts`'s
    `WALL_ROW_PX` is measured against the chrome a tile carries, so the header gave its second row
    back in the same change: the same element moved between two siblings of one flex column, so the
    sum of the chrome is unchanged by construction and the wall's packing did not move. The version
    that was not allowed is bar-at-the-bottom-with-a-two-row-header — a third row, which costs every
    tile on the wall a line of a Worker's output.
  - *Colour.* tmux's bar is green whatever is happening because tmux does not know; ours is on a
    tile that has already decided something is failing. So the tone is the worst of what the bar
    carries and what the chips found: red for the two facts that mean **this screen will not change
    again** (the process exited, the stream is dead), amber for a stalled feed **or any applicable
    health signal reading `bad`**, green only when nothing on the tile is failing. The failing
    signal is named on the bar in the chip row's own words — `HealthChips.leadingFinding` is
    literally `orderChips(chipsFor(…))[0]`, so the bar cannot name a different axis or use different
    words for the same one. `unknown` is not a finding: the bar is severity, not completeness, and
    an amber band on every un-probed pane is how a colour stops being read.

- **Hiding a pane is an UNSUBSCRIBE, not a `display:none`.** An operator can take one window off the
  wall. Three properties, and the ADR names them because the cheap implementation satisfies none of
  them:

  1. *Hidden must not look like gone.* A tile that vanishes silently is indistinguishable from a
     Worker that died — the confusion the ghosts, the `no session` scrim and the five-signal row all
     exist to end. A persistent `N panes hidden` strip above the wall names each one and is the way
     back; a hidden set rendered nowhere is a bug.
  2. *It stops the cost.* The hidden id is subtracted from the page's wanted set, so the socket sends
     `unsubscribe`. **Measured against the running streamer** — one client, one Terminal, twelve
     seconds either side of an `unsubscribe`: **5 binary frames while subscribed, 0 after**, and no
     further `{t:'state'}` for that id. The streamer's `snapshotRound` builds its `capture-pane`
     exec list from live subscriptions, per node-and-session, with the WINDOW set as an argument — so a hidden window
     leaves that list and the last window of a session takes the whole 3 s exec with it. `fanOut`
     and `announceHealth` stop for it, and a live attach is given up, freeing an `sshd` session, a
     PTY, a grouped tmux session and a `LIVE_BUDGET` slot. **What it does not stop** is the 30 s
     discovery-and-probe pass, which is per Machine and owes nothing to any subscription; hiding a
     pane is not a way to stop probing a Machine. A hidden pane also stops having its exit banner
     read, because that is read off the snapshots no longer being taken.
  3. *It survives a reload*, in `localStorage`, versioned in the value and failing OPEN — an
     unreadable document shows every pane, because the failure mode of guessing is a wall with holes
     in it.

  The grain is one WINDOW, which is what a tile is: a `kontra` session's two windows are an actor and
  its handler, and an operator silencing the noisy handler must keep the actor beside it. This is
  distinct from the filter bar, which deliberately does NOT unsubscribe — typing in a search box must
  not tear down and rebuild a PTY attach per keystroke.

- **`isHealthy` is read in the pane's own mode, like the chips.** The rollup that `ActorsPage` and
  `ActorCard` count "serving" with required `loads === 'ok'`, and `loads` comes from the vmagent only
  `infra/programs/machine.ts` installs — so a `local` or `docker` node could not satisfy it on any
  installation that has ever existed, and a healthy local actor was reported as **0 serving**. It now
  calls the same `signalApplies` the chip row does, derived from the same `healthEntries`, because
  two predicates disagreeing about what healthy means is worse than the bug. Omission stays
  one-directional: a `bad` reading disqualifies in every mode, and `unknown` on an axis that could
  have been measured is still not healthy. The sidebar tree's `rollupOne` was corrected with the same
  call, for the same reason.

- **A regression test now guards the PID split.** Injecting an unhandled rejection into the streamer
  while a converge is in flight must leave the converge succeeding. Without it, a future
  simplification that folds `panels.js` back into `infra.js` reintroduces finding (6), and the
  symptom will present as flaky fleet deploys.

- **The infra container grows a published port, a restart policy for the child, and headroom.**
  `mem_limit` moves from 1 g (sized for "~164 MB per concurrent update") to ~1.5 g: the streamer
  holds an `ssh` child (~5 MB) plus a 1 MB scrollback ring per live Terminal, and a panel flood must
  not OOM an apply — which is how Pulumi locks get wedged.

- **`kontra fleet deploy --tmux` changes meaning**, and Machines already carrying
  `kontra-tmux.service` keep it until they are re-converged. Teardown continues to stop the unit so
  those Machines drain cleanly.

- **Two origins in the browser.** The SPA is served by `orchestrator-api`; tickets and the
  WebSocket come from the streamer's own port, under a narrow CORS allow-list. This keeps panel
  bytes off the event loop that serves the CLI's DuckDB queries, at the cost of the SPA needing the
  streamer's URL injected rather than inferred.

- **Probes and tests that touch tmux must use a private socket (`tmux -L …`).** Establishing
  findings (1)–(3) on the default socket crashed the operator's own tmux server and destroyed
  sessions that had been running for a day. `tmux-resurrect` was not enabled; nothing was
  recoverable. That is a process consequence, not a footnote.

- **The upstream crash is worth reporting.** A reproducer exists (stall a control-mode client under
  ~14 MB/s of pane output); the fleet floor is tmux 3.2a.
