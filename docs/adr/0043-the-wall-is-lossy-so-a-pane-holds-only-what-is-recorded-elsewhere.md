# 43. The wall is lossy, so a pane holds only what is recorded elsewhere

## Status

**Accepted.** Extends **0020**, which built the read-only Dashboard and arranged for panes to hold
journals; this names what that arrangement was buying and makes it a rule rather than a property of
what `machine.ts` happens to install. Touches **0037** (a **Warden** reports its panes), **0038**
(the data plane is not closed), and the **Tenant** definition **0036** fixed as one Temporal
namespace. Supersedes no ADR.

## Context

The want is concrete and reasonable: a caller's workflow opens an interactive agent — a `claude`
session — in tmux on a **Machine**, and tags it so the Monitor shows it. Watching an agent work is
exactly what a wall of terminals is for.

**It would work today, and that is the problem.** The mechanism is already shipped:
`tmux set-option -t <session> @kontra <tag>` marks a session as kontra's, with
`KONTRA_SESSION_OPTION` as its independently-written TypeScript peer, pinned by `tmux_test.go`. Tag
a session holding an agent and it appears on the wall.

The grammar that would refuse it does not run on that path. `SAFE.command` admits only

    journalctl [-n N] [--no-pager] -fu <unit>.service

and `assertSafe('command')` is called in **exactly one place**: `converge.ts:77`. So the restriction
governs what the converge workflow **creates**. Discovery reads `pane_current_command` from
`list-panes` and restricts nothing. **kontra tightly controls what it makes and admits whatever it
finds** — which is a defensible asymmetry for a Worker's own journal, and the wrong one here.

Three things make "just tag it" wrong, and none of them is that tmux is insecure.

**1. A pane can be destroyed by somebody else's browser tab.** 0020's finding (2) measured
`tmux: server[17770] segfault` from a stalled control-mode client — not OOM (`oom_kill 0`,
`systemd-oomd inactive`) — and a tmux server crash destroys *every session on that socket*. The
journals-only arrangement is what makes that survivable: journald still holds the bytes, so the loss
is a tile, not a record. An agent's session is not replaceable that way. Losing it costs work that
existed nowhere else, and it is lost through a failure the agent's owner does not control.

**2. A pane shows everything, and nothing filters it.** There is no redaction anywhere in
`control/orchestrator/src/panels/`; `capture-pane` bytes reach xterm as they are, which is correct
for a service's own journal and very different for an agent, whose screen carries whatever it read —
file contents, environment, tool output, credentials it happened to encounter. `/api/panels/ticket`
is unauthenticated and says so (`routes/panels.ts`: *"Anyone who can reach `:8088` can still mint a
ticket"*), so the audience for that screen is currently "anyone who can reach the API".

**3. A Terminal id carries no scope.** It is `<mode>:<node>/<session>/<window>` — no **Tenant**
segment and no Workspace segment. One minted ticket shows every pane on every node. A Workspace
bounds who may *edit* code, through Unix permissions on a directory; it bounds nothing at runtime.
Two engineers sharing one Tenant already see each other's Runs and Datasets, which is the intended
trade — but a journal of a service is boring and an agent's screen is not, so this arrangement puts
weight on a limitation that was previously harmless.

## Decision

**A pane may hold only what is recorded elsewhere.** The wall is lossy on purpose — 0020 already
says *"the Manifest, the journal on the Machine, and the lake are the record"* — and this makes the
converse a rule: anything whose only copy is on a screen does not belong on the wall.

Six consequences of that one sentence.

1. **An agent runs under a supervisor and its journal goes in the pane.** The same shape the actor
   and the handler already have. `claude` writes to its journal; the pane tails it; the session is
   tagged and monitored exactly as a Worker's is. The wall stays lossy, the agent's state is never on
   the wall's critical path, and no new mechanism is introduced.

2. **Discovery gains the opt-in that converge already has.** A session whose panes are not journals
   is admitted only when something says so explicitly. The rule that holds where kontra creates a
   pane must also hold where it finds one, or it is documentation rather than an invariant.

3. **Interaction is a separate surface, if it is ever built.** The read-only guarantee rests on there
   being nothing to write to, and 0020's finding (3) is why that phrasing matters: *"a channel that
   can carry `refresh-client` can carry `run-shell`, which executes as root. A read-only guarantee
   that rests on 'we only ever write these two commands' is weaker than one that rests on 'there is
   nothing to write to'."* An agent you can watch but not answer is half a product, and the pressure
   to add a reply box will be real — so it gets its own surface, its own admission and its own audit,
   and the Monitor never grows a write path.

4. **Agent output does not reach a pane before the panel routes are authenticated.** The ordering is
   the decision. Until `/api/panels/ticket` requires a principal, tagging an agent turns an
   unauthenticated route into a live feed of that agent's context.

5. **Wanting separation is answered by a second Tenant, never by a second Workspace.** Stated because
   the intuition runs the other way: a Workspace looks like an isolation boundary and is a directory.
   The Temporal namespace is the only authorisation boundary there is.

6. **xterm's defaults are pinned by a test rather than inherited.** Pane bytes flow into a terminal
   emulator unfiltered, so what that emulator will act on is part of this boundary. `allowProposedApi`
   is unset today, which is right; nothing currently fails if it stops being.

## Considered options

**Tag it and accept the risk.** The honest version of doing nothing, and it survives contact with a
laptop demo. It fails on the first stalled viewer, and it fails silently — the operator sees a tile
go blank, not that an agent's session was destroyed along with every other session on that socket.

**Relax `SAFE.command` to "no shell metacharacters".** The obvious generalisation, and it gives up
the property that makes the current grammar worth having. `SAFE.command`'s narrowness is not about
injection — the value is already a single argv element handed to a shell on the far side — it is
about *authority*: a window's command runs as root on a Machine, and the grammar limits that
authority to "which unit's journal to follow" rather than "what to run". A metacharacter filter
permits arbitrary programs with tidy syntax.

**Make the wall durable instead: persist scrollback so a crash loses nothing.** Attacks the right
problem and is the wrong layer. It means the Monitor becomes a system of record, which is precisely
what 0020 decided it must not be, and it would have to be durable for *every* pane to be trustworthy
for one. Journald already is durable, for free, on the Machine.

**Give the wall a write path so an operator can answer the agent.** Rejected here rather than
deferred, because the moment it exists every argument above about output becomes an argument about
input, and there is no second line of defence: 0020 measured that `tmux attach -r` is not a boundary
— `run-shell` and `send-keys` both executed through a read-only client, as root.

**Add a Workspace or Tenant segment to the Terminal id.** Attractive and premature. It would make the
wall scopeable, but scoping to a Workspace is theatre while one Tenant holds them all, and scoping to
a Tenant is meaningful only once more than one exists. Revisit when a second Tenant does.

## Consequences

- **The feature is available immediately, in the supervised form.** Nothing has to be built to watch
  an agent work: supervise it, tail its journal, tag the session. What this ADR removes is the
  shortcut, not the capability.

- **`SAFE.command` needs a peer on the discovery path,** and that is new work — decision 2 is the
  only item here that is not already true. It is small and it is the difference between an invariant
  and a paragraph.

- **This raises the cost of leaving the panel routes open.** `hosted-readiness/03` was already
  blocking; agent panes make it blocking for a feature somebody wants now rather than for a
  marketplace that does not exist yet.

- **The single-Tenant limit becomes visible earlier than planned.** Two engineers who trust each
  other still share every pane. That is the stated design and the stated place to draw "contact us to
  scale" — but it should be said out loud in the Workspace documentation rather than discovered by
  someone watching a colleague's agent read a file they did not expect to see.

- **Nothing here is specific to Claude.** A REPL, a debugger, a long-running interactive scan, a
  `tmux` window somebody attached to by hand: the rule is about whether a pane's contents exist
  anywhere else, and the answer for all of them is the same.
