package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestTmuxSessionIsTheActorAndItsVersion(t *testing.T) {
	// Deterministic so `tmux attach -t webcrawl-0.2.0` works without looking anything up, and so a
	// second `kontra serve --tmux` for the same BUILD collides instead of starting a rival pair on
	// the same task queue.
	//
	// THE VERSION IS THE POINT. It was `kontra-<actor>`, which named the Actor and not the build —
	// so two versions running side by side collided on one name, and the second was refused as
	// "already running" when it was a different Worker entirely. The `kontra-` prefix went with it:
	// discovery is the `@kontra` tmux option now (`panels/local.ts`), which can also say what KIND
	// of session this is — something a prefix never could.
	// UNDERSCORES, not dots — tmux rewrites `.` and `:` to `_` when it creates a session, so a name
	// with a dot in it is a name the server does not have. Measured:
	//
	//	$ tmux new-session -d -s 'nscheck-0.1.0'; tmux list-sessions -F '#{session_name}'
	//	nscheck-0_1_0
	//
	// Unsanitised, `tmux attach -t webcrawl-0.2.0` fails against a session that is right there, and
	// the Monitor's probe looks for a name `list-panes` never reports — drawing a Machine whose
	// Worker is running perfectly as one with no session.
	if got := tmuxSession("webcrawl", "0.2.0"); got != "webcrawl-0_2_0" {
		t.Errorf("tmuxSession = %q, want webcrawl-0_2_0", got)
	}
	if got := tmuxSafeName("a.b:c"); got != "a_b_c" {
		t.Errorf("tmuxSafeName = %q, want a_b_c", got)
	}
	// An actor with no version in its manifest names the session after itself rather than trailing
	// a bare dash.
	if got := tmuxSession("webcrawl", ""); got != "webcrawl" {
		t.Errorf("tmuxSession = %q, want webcrawl", got)
	}
}

// The `@kontra` values are a cross-language contract with `backend/src/panels/tmux.ts`, which
// derives them independently — and a drift has the worst failure shape there is: the Worker starts
// and runs perfectly, and the Monitor never shows it.
func TestTheSessionTagSaysWhatKindOfSessionItIs(t *testing.T) {
	if got := kontraSessionTag("webcrawl", "0.2.0"); got != "actor:webcrawl:0.2.0" {
		t.Errorf("kontraSessionTag = %q", got)
	}
	if got := kontraWorkflowTag("enumerate_scope"); got != "workflow:enumerate_scope" {
		t.Errorf("kontraWorkflowTag = %q", got)
	}
	if kontraOption != "@kontra" {
		t.Errorf("the option name is a contract: %q", kontraOption)
	}
}

func TestShellJoinQuotesEveryArgument(t *testing.T) {
	// Paths come from the actor dir and the repo root; either may contain spaces, and an
	// unquoted one silently becomes two arguments — the actor would start with the wrong file.
	got := shellJoin([]string{"/opt/my dir/python3", "/opt/my dir/actor.py"})
	if got != `'/opt/my dir/python3' '/opt/my dir/actor.py'` {
		t.Errorf("shellJoin = %s", got)
	}
	if q := shellJoin([]string{"it's"}); q != `'it'\''s'` {
		t.Errorf("single quote not escaped: %s", q)
	}
}

func TestTmuxHoldKeepsTheWindowAfterAnExit(t *testing.T) {
	// A window that vanishes is the worst possible report of a crash-on-boot — which is exactly
	// the failure this flag exists to make visible.
	h := tmuxHold([]string{"/bin/false"})
	if !strings.Contains(h, "'/bin/false'") {
		t.Errorf("command missing: %s", h)
	}
	for _, want := range []string{"exited", "read -r _"} {
		if !strings.Contains(h, want) {
			t.Errorf("hold is missing %q: %s", want, h)
		}
	}
}

func TestTmuxHoldRunsUnderEVERYShellTmuxMightPick(t *testing.T) {
	// The test above passed for the entire time this was broken, because the shape of the string
	// was never the problem — the SHELL was. tmux runs a pane under `default-shell`, which is the
	// owner's login shell, and the wrapper used to assign to `status`: a read-only special
	// parameter in zsh. Assigning to it aborts a non-interactive zsh immediately, so the printf
	// and the read never ran, the window closed, and the session went with it. Under sh, dash and
	// bash the identical string is fine, which is why nothing caught it.
	//
	// So the assertion is behavioural: run the wrapper for real, under each shell present, and
	// require the exit line to actually come out. stdin is /dev/null so `read` sees EOF and
	// returns rather than blocking — by then the interesting part has already happened.
	h := tmuxHold([]string{"/bin/false"})
	ran := 0
	for _, sh := range []string{"sh", "dash", "bash", "zsh"} {
		bin, err := exec.LookPath(sh)
		if err != nil {
			continue
		}
		ran++
		cmd := exec.Command(bin, "-c", h)
		cmd.Stdin, _ = os.Open(os.DevNull)
		// The wrapper records its exit status as a tmux PANE option, and this test runs it for real.
		// The suite itself is frequently run from inside a tmux pane — so without this, running the
		// tests would mark the developer's own pane as a Worker that exited.
		cmd.Env = append(os.Environ(), "TMUX=", "TMUX_PANE=")
		out, _ := cmd.CombinedOutput()
		if !strings.Contains(string(out), "[exited 1]") {
			t.Errorf("%s: the hold never printed the exit line — the shell aborted before it.\n"+
				"  wrapper: %s\n  output: %q", sh, h, out)
		}
	}
	if ran == 0 {
		t.Skip("no shell found to run the wrapper under")
	}
}

// The hold records the exit status where a DASHBOARD can read it, not only where a human can.
//
// `pane_current_command` cannot answer "did this Worker finish": the wrapper does not put the
// wrapped command in its own process group, so tmux reports the hold shell (`zsh`) for a pane whose
// Worker is running AND for one whose Worker is over. Measured on this box, both directions. So the
// status is written into a pane option the Monitor's existing `list-panes` probe returns, and
// `backend/src/panels/tmux.ts:KONTRA_EXIT_OPTION` is the peer that reads it.
func TestTheHoldRecordsItsExitStatusForTheMonitor(t *testing.T) {
	if kontraExitOption != "@kontra_exit" {
		t.Errorf("the option name is a cross-language contract: %q", kontraExitOption)
	}
	got := tmuxHold([]string{"/bin/false"})
	if !strings.Contains(got, `tmux set-option -p -t "$TMUX_PANE" @kontra_exit "$kontra_status"`) {
		t.Errorf("the exit status is not recorded where the Monitor can read it:\n%s", got)
	}
	// TARGETED AND GUARDED. `set-option -p` with no target resolves "the current pane" from whatever
	// session the tmux server thinks is current — which outside a pane is somebody else's, and this
	// would then label a stranger's Worker as exited. The guard is what makes the wrapper safe to run
	// anywhere, including in this suite.
	if !strings.Contains(got, `[ -n "$TMUX" ] &&`) {
		t.Errorf("the pane option is not guarded by $TMUX — outside tmux it would mark another pane:\n%s", got)
	}
	// The status still has to be CAPTURED before anything else runs, or the option records tmux's
	// exit code rather than the Worker's.
	if i, j := strings.Index(got, "kontra_status=$?"), strings.Index(got, "set-option"); i < 0 || i > j {
		t.Errorf("the exit status is read after another command has already run:\n%s", got)
	}
}

func TestPrintTmuxHelpNamesTheSession(t *testing.T) {
	var b bytes.Buffer
	printTmuxHelp(&b, "kontra-webcrawl", []string{"actor", "handler"})
	out := b.String()
	for _, want := range []string{"tmux attach -t kontra-webcrawl", "kill-session -t kontra-webcrawl", "Ctrl-b d"} {
		if !strings.Contains(out, want) {
			t.Errorf("help is missing %q:\n%s", want, out)
		}
	}
	// The window indices are read back from tmux rather than assumed: base-index is commonly 1,
	// and "0=actor" against a session whose first window is 1 sends you to the wrong pane. When
	// tmux is absent the line is simply omitted, never guessed.
	if strings.Contains(out, "(0=actor)") {
		t.Error("help hardcodes a window index instead of reading it back")
	}
}

// A pane must survive Ctrl-C, and the worker in it must not.
//
// MEASURED, and it is why the trap is there. `kontra workflow pause` sends C-c to the pane, which
// SIGINTs the whole foreground process group — the worker AND the wrapping shell. Without a trap
// the shell died on the interrupt, the window closed, and the session went with it: the command
// that exists to PAUSE a worker killed it instead. An operator pressing Ctrl-C in an attached
// session lost the traceback they had just produced, for the same reason.
func TestTheHoldSurvivesAnInterrupt(t *testing.T) {
	got := tmuxHold([]string{"python3", "actor.py"})
	if !strings.HasPrefix(got, "trap ':' INT; ") {
		t.Fatalf("no INT trap — Ctrl-C will take the window with it:\n%s", got)
	}
	// `trap ''` and `trap ':'` differ in the one way that matters. A shell that IGNORES a signal
	// passes SIG_IGN to everything it forks, so the empty form would make the worker ignore the
	// interrupt too — it would keep running and only the report would change. A no-op HANDLER is
	// reset to the default in children.
	if strings.Contains(got, `trap '' INT`) {
		t.Error("`trap '' INT` makes the worker inherit SIG_IGN — it would not stop")
	}
	// And the hold itself is still there: the exit status, and a window that waits.
	if !strings.Contains(got, "kontra_status=$?") || !strings.Contains(got, "read -r _") {
		t.Errorf("the hold is gone:\n%s", got)
	}
}
