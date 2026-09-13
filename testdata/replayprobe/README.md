# replayprobe — the first replay fixture

**Shape:** the simplest history that can catch a real non-determinism — two activity calls in a
fixed order, completed successfully.

`history.json` was captured from a real execution with `kontra workflow history`, not hand-written.
A hand-written history proves that the replayer parses JSON; a captured one proves it replays what
Temporal actually recorded.

**What it catches:** reorder the two `execute_activity` calls in `workflow.py` and replay fails with

    [TMPRL1100] Nondeterminism error: Activity type of scheduled event 'replayprobe_first'
    does not match activity type of activity command 'replayprobe_second'

which is the edit that looks harmless in review, passes every unit test, and breaks every execution
that was already in flight when it deployed.

**What it does NOT catch:** anything inside an activity. Replay runs workflow code only and feeds
activity results from the history as recorded values — so a Method's body is never executed here.

## Adding another

    kontra workflow history <workflow-id> -o testdata/<name>/history.json

beside a `workflow.py` that declares the same workflow type, and a README saying what shape it
captures. A history fixture is large and unreadable; without the note, a failure six months from now
cannot be told apart from a fixture that was always wrong.
