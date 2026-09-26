/**
 * The Worker NAME — `<actor>-<version>`, sanitised — and nothing else any more.
 *
 * THIS FILE WAS THE MONITOR'S AND IS NOW TWO FUNCTIONS. It carried the whole read side of the wall:
 * the `list-panes` format string and its parser, the pane-process verdict, the capture-pane command
 * builder, the snapshot geometry, ADR 0043's session-kind vocabulary and the `@kontra` tag writers.
 * All of it went with the Monitor. What stayed is the naming rule, because the naming rule is not
 * about tmux even though tmux is why it looks the way it does.
 *
 * KEEPING THE NAME `tmux.ts` IS DELIBERATE FOR NOW. `shared/conformance/queues.json` §tmux_session
 * names this path as one of the three arms that derive the Worker name, and the Go peer is still
 * `cli/internal/tmux/tmux.go`. Renaming both, plus the corpus section and its drivers in four
 * languages, is a change to make when the serve path stops using tmux — not in the same commit that
 * deletes a feature. See the note on {@link tmuxSafeName}.
 */

/**
 * A Worker name with the two bytes that cannot survive folded out.
 *
 * MEASURED, and it is the reason this function exists rather than a comment. tmux's
 * `session_check_name()` rewrites every `.` and `:` in a session name to `_`, silently, at creation:
 *
 *     $ tmux new-session -d -s 'nscheck-0.1.0' ; tmux list-sessions -F '#{session_name}'
 *     nscheck-0_1_0
 *
 * Which matters the moment a name is `<actor>-<version>`, because every version has dots in it. Two
 * things broke, and both were silent: `tmux attach -t nscheck-0.1.0` failed against a session that
 * was right there, and the probe compared the name it expected against the name `list-panes`
 * reported, found no match, and rendered a Machine whose Worker was running perfectly as one with
 * NO SESSION — the exact thing ADR 0020 says a tile may never say.
 *
 * THE RULE OUTLIVED THE REASON, and that is why this is still here with tmux gone. The folding is
 * now an inherited naming scheme: three languages derive it, `shared/conformance/queues.json`
 * §tmux_session pins them against each other, and changing it renames every Worker at once. A
 * colon is also not a legal byte in a container name, so the `:` half is still load-bearing on its
 * own terms. `cli/internal/tmux/tmux.go:tmuxSafeName` is the peer.
 */
export function tmuxSafeName(name: string): string {
  return name.replace(/[.:]/g, '_');
}

/**
 * An Actor's Worker, named: `<actor>-<version>`, sanitised.
 *
 * THE ONLY TYPESCRIPT COPY, and it is here because it was two. `control/orchestrator/src/actorControl.ts`
 * minted the name when the Serve button started a worker and the browser derived it again —
 * byte-identical, fallback and all, one calling {@link tmuxSafeName} and the other re-inlining
 * `.replace(/[.:]/g, '_')`. Two writers on the same side of a language boundary is not a contract,
 * it is a copy. What crosses a real boundary — this and `cli/internal/tmux/tmux.go:tmuxSession` — is
 * held by `shared/conformance/queues.json` §tmux_session, which every side executes.
 *
 * THE VERSION IS THE LOAD-BEARING HALF: `probe` alone names the ACTOR and not the BUILD, so two
 * versions served side by side would collide on one name.
 *
 * THE FALLBACK IS `actor`. It differed from the fleet Machine's `fleet` on purpose — that one named
 * a MACHINE, which had Terminals even with no Actor placed on it — and the corpus still carries
 * both columns because `cli/fleet.go` still derives the Machine's. A name of `''` or `'_'` cannot be
 * attached to or addressed, and anything looked up by it would match whatever else happens to have
 * no name.
 */
export function actorSession(actor: string, version: string): string {
  const name = tmuxSafeName(version ? `${actor}-${version}` : actor);
  return name === '' || name === '_' ? 'actor' : name;
}
