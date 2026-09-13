"""The two OPTIONAL author failure-signal exceptions.

Pure Python (no runtime import) so it is safe to import from the author surface (actor.py).
The engine (internals/engine.py) interprets them.

Failure model — required author error-handling is ZERO; these are optimisations:

  1. raise NOTHING (the happy path) — any exception isolates that ONE unit into the run's
     `failures` channel (a permanently-bad unit never sinks its siblings or the run). The
     host does NOT reload the session for a plain exception — a dead URL or a network blip
     can't be fixed by reopening the browser, and a reload would waste the healthy units.

  2. NonRetryableError — "this unit is permanently bad": the host isolates it immediately as
     a `terminal` failure, without probing the resource for liveness.

  3. SessionLost — "the shared resource is dead, reload it": the host nulls the session and
     lets the handler retry the same actor id, so @actor.load reopens a fresh resource
     and the batch resumes from its committed (step, unit) state. A unit that keeps forcing a
     reload is isolated after a few tries so one poison unit can't sink the batch.
"""

from __future__ import annotations


class NonRetryableError(Exception):
    """Raise from inside a step to isolate this unit immediately as a `terminal` failure,
    without a liveness probe — for a known-permanently-bad unit. OPTIONAL: a plain exception
    already isolates the unit; this just skips the probe."""


class SessionLost(Exception):
    """Raise from inside a step when the SHARED session resource is known-dead — the
    browser/client/connection that `@actor.load` opened is gone, so retrying the unit in
    place would reuse the corpse. The host nulls the session and lets the handler retry
    the same actor id, re-running `@actor.load` for a fresh resource; the committed (step,
    unit) state is skipped, so the batch resumes exactly once. A unit that keeps forcing a
    reload is isolated after a few tries (it can't loop the whole batch).

    A SIBLING of NonRetryableError, NOT a subclass — opposite semantics: SessionLost recovers
    (reload), NonRetryableError is terminal. OPTIONAL — without it a dead resource still
    degrades safely (units isolate); SessionLost RECOVERS in place instead. Only the author
    can tell 'browser crashed' (-> SessionLost) from 'wifi dropped' (-> plain isolate)."""
