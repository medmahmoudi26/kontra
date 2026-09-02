package tmux

// tmux.go — `--tmux`: run a worker inside a tmux session instead of in the foreground.
//
// WHY IT EARNS A FLAG. A worker is TWO processes (the actor and the handler) and the interesting
// output is interleaved between them. In the foreground both stream to one terminal, which is
// fine until something goes wrong: you cannot scroll one half without the other, you cannot
// leave it running when the ssh session drops, and on a fleet Machine you get the journal or
// nothing. A tmux session gives each half its own window, survives a detach, and is the same
// gesture locally and on a Machine — which is the point of the flag being spelled the same way
// in both places.
//
// It is NOT a supervisor. On a Machine, systemd still owns restart policy; `--tmux` there wraps
// the same two commands so they are attachable, and a crash still ends the window rather than
// being silently restarted behind your back.

import (
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"

	"github.com/medmahmoudi26/kontra/cli/internal/cliio"
)

// Session is the session name for one actor's worker: `<actor>-<version>`. Deterministic so
// `tmux attach -t webcrawl-0.2.0` works without looking anything up, and so a second `kontra serve
// --tmux` for the same build is detected as already-running rather than starting a rival pair on
// the same queue.
//
// NO `kontra-` PREFIX. It was the Monitor's discovery mechanism (`panels/local.ts`), and discovery
// is the `@kontra` tmux option now — which can also say what KIND of session this is, something a
// prefix never could.
//
// THE VERSION IS THE LOAD-BEARING HALF. `kontra-webcrawl` named the ACTOR and not the BUILD, so two
// versions running side by side collided on one name — and `kontra serve --tmux` refused the second
// as "already running" when it was a different Worker entirely.
//
// THE FALLBACK IS THE CONTRACT'S, not this function's convenience. A name that sanitises to ”
// or '_' cannot be attached to, and a pane looked up by it matches whatever in the inventory
// happens to have no name — so it is `actor`, which is what `panels/tmux.ts:actorSession` answers
// on the other side of the language boundary. This function used to return the unusable string;
// shared/conformance/queues.json §tmux_session is what found it and what holds the two together now.
// The `fleet` fallback in fleetSessionName is a DIFFERENT domain, and the corpus says why.
func Session(actor, version string) string {
	name := SafeName(actor)
	if version != "" {
		name = SafeName(actor + "-" + version)
	}
	if name == "" || name == "_" {
		return "actor"
	}
	return name
}

// SafeName is a session name as tmux will actually store it.
//
// MEASURED, and it is the reason this exists rather than a comment. tmux's `session_check_name()`
// rewrites every `.` and `:` to `_`, silently, at creation:
//
//	$ tmux new-session -d -s 'nscheck-0.1.0'; tmux list-sessions -F '#{session_name}'
//	nscheck-0_1_0
//
// Which matters the moment a session is `<actor>-<version>`, because every version has dots in it.
// Two things break and both are silent: `tmux attach -t nscheck-0.1.0` fails against a session that
// is right there, and the Monitor's probe compares the name it expects against the one `list-panes`
// reports, finds no match, and draws a Machine whose Worker is running perfectly as one with NO
// SESSION — the exact thing ADR 0020 says a tile may never say.
//
// Sanitised where the name is MINTED, so what kontra prints, what it looks for, and what tmux holds
// are the same string. `control/orchestrator/src/panels/tmux.ts:SafeName` is the peer.
func SafeName(name string) string {
	return strings.NewReplacer(".", "_", ":", "_").Replace(name)
}

// KontraSessionTag is the `@kontra` value for one Actor's Worker, byte-identical to
// `panels/tmux.ts:actorSessionTag`. Written independently on this side, like every other
// cross-language literal in this repo.
func KontraSessionTag(actor, version string) string {
	return "actor:" + actor + ":" + version
}

// KontraWorkflowTag is the `@kontra` value for a served caller workflow — `panels/tmux.ts:
// workflowSessionTag`.
func KontraWorkflowTag(name string) string { return "workflow:" + name }

// Available reports whether tmux is on PATH.
func Available() bool {
	_, err := exec.LookPath("tmux")
	return err == nil
}

// HasSession reports whether the session already exists.
func HasSession(name string) bool {
	return exec.Command("tmux", "has-session", "-t="+name).Run() == nil
}

// Proc is one process to run in its own window.
type Proc struct {
	Window string   // window name, e.g. "actor" / "handler"
	Dir    string   // working directory
	Env    []string // KEY=VALUE pairs to inject, a DELTA — see Start on why this is not inherited
	Argv   []string // command + args
}

// Start creates a DETACHED session with one window per process and returns the session name.
//
// Detached, not attached: `kontra serve --tmux` should hand the terminal back so the same shell can
// dispatch to the worker it just started. Attaching is one command and it is printed.
//
// Each window runs the command under `sh -c '<cmd>; …'` with a trailing read, so a process that
// exits leaves its output ON SCREEN with the exit status instead of the window vanishing — a
// window that disappears is the worst possible report of a crash-on-boot, which is exactly the
// failure this flag exists to make visible.
func Start(name, tag string, procs []Proc) error {
	if len(procs) == 0 {
		return fmt.Errorf("tmux: nothing to run")
	}
	if !Available() {
		return fmt.Errorf("tmux is not installed (apt-get install tmux), or drop --tmux to run in the foreground")
	}
	if HasSession(name) {
		// "a worker for this ACTOR" was wrong the moment a workflow could be served this way — the
		// same message reported a served workflow as an actor, and sent an operator looking for a
		// deployment that does not exist. `tag` is what this session IS.
		what := "worker"
		if strings.HasPrefix(tag, "workflow:") {
			what = "workflow worker"
		}
		return fmt.Errorf("tmux session %q already exists — a %s is already running there.\n"+
			"  attach:  tmux attach -t %s\n"+
			"  replace: tmux kill-session -t %s", name, what, name, name)
	}

	for i, p := range procs {
		// -e per variable, NOT the parent's environment.
		//
		// A tmux pane inherits the environment of the tmux SERVER, which is a daemon that
		// outlives any one client — so it is usually some other shell's environment from
		// whenever the server first started. Setting exec.Cmd.Env here configures the `tmux`
		// CLIENT and reaches the pane not at all. That failure is quiet and asymmetric: the
		// actor still starts (its config is baked into the binary's own defaults) while the
		// handler exits immediately on `KONTRA_ACTOR_NAME is required` — the actor-up,
		// handler-dead state, which reports as a worker that is running and invisible.
		args := []string{}
		for _, kv := range p.Env {
			args = append(args, "-e", kv)
		}
		var c *exec.Cmd
		if i == 0 {
			c = exec.Command("tmux", append([]string{
				"new-session", "-d", "-s", name, "-n", p.Window, "-c", p.Dir,
				"-x", strconv.Itoa(PaneCols), "-y", strconv.Itoa(PaneRows),
			}, append(args, Hold(p.Argv))...)...)
		} else {
			c = exec.Command("tmux", append([]string{"new-window", "-t", name + ":", "-n", p.Window, "-c", p.Dir}, append(args, Hold(p.Argv))...)...)
		}
		if out, err := c.CombinedOutput(); err != nil {
			_ = exec.Command("tmux", "kill-session", "-t", name).Run()
			return fmt.Errorf("tmux %s: %v: %s", p.Window, err, strings.TrimSpace(string(out)))
		}
	}

	// PIN THE GEOMETRY, so a VIEWER cannot reshape the operator's worker.
	//
	// tmux's default `window-size latest` means the most recently attached client's size wins for
	// the window — and it STICKS after that client leaves. The dashboard attaches a read-only
	// client whose size comes from a browser tile measuring itself, so a tile that measured before
	// layout (collapsed, or zero-width) resizes the real worker and leaves it there. Measured on
	// this box: one such attach took a 200x50 session to 12x5, and every log line in it wrapped at
	// twelve characters for a day. With `window-size manual` the identical attach left it at
	// 200x50.
	//
	// This is what read-only has to mean for geometry too, not just for keystrokes (ADR 0020): a
	// viewer renders what is there, and what is there does not move because someone looked at it.
	// The cost is that a browser tile narrower than PaneCols scrolls or scales instead of
	// reflowing, which is the right trade — the operator's own pane is the artifact.
	//
	// Best-effort, for the same reason the tag below is: a working Worker beats a cosmetic
	// guarantee, and `window-size` predates nothing we require.
	for _, p := range procs {
		target := name + ":" + p.Window
		if out, err := exec.Command("tmux", "set-option", "-w", "-t", target, "window-size", "manual").CombinedOutput(); err != nil {
			fmt.Fprintf(cliio.Stderr, "note: could not pin the size of tmux window %q (%v: %s) — it will "+
				"still run, but a dashboard viewer may resize it\n", target, err, strings.TrimSpace(string(out)))
		}
	}

	// MARK IT AS KONTRA'S, and say what it is. This is what the Monitor discovers by
	// (`panels/local.ts`), replacing the `kontra-` name prefix — so the session can be called what
	// an operator would call it and still appear on the wall.
	//
	// Best-effort on purpose. A tmux too old for user options (they arrived in 3.0) would fail
	// here, and a Worker that is running is worth more than a tile: it stays up, and the legacy
	// prefix match is what would have found it anyway. Failing the start over a label would trade a
	// working Worker for a cosmetic guarantee.
	if tag != "" {
		if out, err := exec.Command("tmux", "set-option", "-t", name, KontraOption, tag).CombinedOutput(); err != nil {
			fmt.Fprintf(cliio.Stderr, "note: could not tag tmux session %q (%v: %s) — it will still run, "+
				"and the Monitor falls back to matching the name\n", name, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// KontraOption is the tmux USER OPTION that marks a session as kontra's — `panels/tmux.ts:
// KONTRA_SESSION_OPTION`, written independently on this side and pinned by tmux_test.go.
const KontraOption = "@kontra"

// The geometry every kontra-created session is pinned to, in cells.
//
// Generous rather than clever: a worker's own log lines and a `htop`-shaped TUI both want room,
// and the size is FIXED so that nothing which merely LOOKS at the session can change it (see the
// `window-size manual` block above).
//
// 120x40, DOWN FROM 200x50. The 200 was picked to be "wider than any wall tile" — a defence against
// tmux reflowing the window to a viewer's size, which `window-size manual` above already prevents
// outright, so the width no longer has to win a race it is not in. What it cost instead was
// legibility: a dashboard tile scales the type so the whole pane fits (`fitSourceWidth` in
// TerminalTile.tsx), and 200 columns in a default ~794px tile lands at about 6.6px, which is
// technically unwrapped and practically unreadable. 120 lands near 11px in the same tile and still
// leaves half again the room of a default 80-column terminal.
//
// The height follows the same reasoning downward: rows are not scaled to fit — a terminal showing
// its last N lines is what a terminal has always been — so 40 is simply a deep enough scrollback
// for a Worker's output without pretending a tile will ever show all of it at once.
const (
	PaneCols = 120
	PaneRows = 40
)

// Hold wraps argv so the window survives the process exiting, showing the exit code.
//
// The variable is `kontra_status`, and the ugly name is the whole point. It was `status`, which is
// a READ-ONLY special parameter in zsh — and tmux runs a pane under `default-shell`, which on any
// host whose owner uses zsh is zsh. Assigning to it aborts the shell before the `printf` and the
// `read` ever run, so the window closes instantly and the session with it.
//
// That failure is invisible in the worst possible way. It only fires when the wrapped process
// EXITS — exactly the crash-on-boot this wrapper exists to keep on screen — and it takes the
// evidence with it: `kontra … --tmux` reports the session it just created, the session is gone a
// moment later, and what the process printed before dying is gone too. A worker that never starts
// is then indistinguishable from one that started and was cleaned up. Nothing about the wrapper
// looks shell-specific, which is why it survived: it works under sh, dash and bash.
// THE `trap` IS WHAT MAKES Ctrl-C SURVIVABLE, and without it the wrapper failed at the one moment
// somebody was deliberately using it. A pane's foreground PROCESS GROUP gets SIGINT, which is both
// the worker and this shell — so an unguarded `sh -c` died on the interrupt, the window closed, and
// the session went with it. Measured: `kontra workflow pause` killed the worker it was pausing, and
// an operator pressing Ctrl-C in an attached session lost the traceback they had just produced.
//
// `trap ':' INT` and not `trap ” INT`. A shell that IGNORES a signal passes SIG_IGN to everything
// it forks, so the empty form would make python ignore the interrupt too — the worker would keep
// running and only the report would change. A no-op HANDLER is reset to the default in children, so
// python still takes the KeyboardInterrupt and this shell still reaches the hold.
// THE PANE OPTION IS THE HALF A DASHBOARD CAN READ. The banner below is for a human attached to the
// session; `@kontra_exit` is the same fact in a field `tmux list-panes -F '#{@kontra_exit}'` returns,
// which is what the Monitor's probe already runs once per Machine.
//
// It exists because `pane_current_command` CANNOT answer "did this Worker finish". Measured on this
// box: the wrapper below does not put the wrapped command in its own process group, so the pane's
// foreground pgid stays this shell's and tmux reports `zsh` for a pane whose Go Worker is running
// perfectly — the same word it reports once that Worker has exited. A dashboard reading the command
// name would call every healthy local Worker dead. Writing the status where the probe can see it is
// what makes "a dead worker says so" true without guessing.
//
// Best-effort and silenced, exactly like the tag: `set-option -p` needs tmux 3.0, and a Worker whose
// exit is only visible in the banner is worth more than a start that failed over a label.
//
// `$TMUX` GUARDS IT AND `$TMUX_PANE` TARGETS IT, and both are load-bearing rather than defensive.
// Outside tmux there is no pane to mark, and `set-option -p` with no target would resolve "the
// current pane" from whatever session the server considers current — on a box with a live tmux that
// is SOMEBODY ELSE'S pane, which this would then label as exited. tmux sets both variables in every
// pane it starts, so inside one this is exact and outside one it does nothing.
func Hold(argv []string) string {
	return `trap ':' INT; ` + ShellJoin(argv) +
		`; kontra_status=$?` +
		`; [ -n "$TMUX" ] && tmux set-option -p -t "$TMUX_PANE" ` + KontraExitOption +
		` "$kontra_status" 2>/dev/null` +
		`; printf '\n[exited %s] press any key to close this window\n' "$kontra_status"; read -r _`
}

// KontraExitOption is the tmux PANE option carrying the wrapped command's exit status —
// `panels/tmux.ts:KONTRA_EXIT_OPTION`, written independently on this side and pinned by tmux_test.go.
const KontraExitOption = "@kontra_exit"

// ShellJoin single-quotes each argument for `sh -c`. Paths here come from the actor directory and
// the repo root, both of which may contain spaces.
func ShellJoin(argv []string) string {
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(out, " ")
}

// PrintHelp tells the operator how to reach what was just started. Window indices are read
// back from tmux rather than assumed: `base-index` is commonly set to 1, and a help line that
// says "0=actor" against a session whose first window is 1 sends you to the wrong pane.
func PrintHelp(w io.Writer, name string, windows []string) {
	fmt.Fprintf(w, "\nworker running in tmux session %q (%s)\n", name, strings.Join(windows, ", "))
	fmt.Fprintf(w, "  attach:  tmux attach -t %s\n", name)
	if idx, err := exec.Command("tmux", "list-windows", "-t", name,
		"-F", "#{window_index}=#{window_name}").Output(); err == nil {
		fmt.Fprintf(w, "  windows: Ctrl-b <n>   (%s)\n",
			strings.Join(strings.Fields(strings.TrimSpace(string(idx))), "  "))
	}
	fmt.Fprintf(w, "  detach:  Ctrl-b d\n")
	fmt.Fprintf(w, "  stop:    tmux kill-session -t %s\n", name)
}
