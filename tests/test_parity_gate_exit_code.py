"""The parity gate's own verdict must reach its exit code.

WHAT THIS PINS, AND IT IS THE WORST BUG OF THIS PHASE. `scripts/parity-gate.sh` ended with a bare
`exit 0` and its EXIT trap ended with `exit $rc`, so a run that printed

    PARITY GATE FAILS (9):
      · no worker is polling paritygate-0.1.0 — the containers are up and nothing is listening
      · the run did not complete (exit 1)
      · the run returned no counts
      · Dataset paritygate_… is not in `kontra dataset list`
      …

exited ZERO, and the CI check went green with nine recorded failures on the screen above it. Every
assertion in that file is aimed at "a run that did not do the work reporting success", and the file
itself was doing exactly that — which also meant five earlier findings in this phase were invisible,
because the one job that would have shown them reported a pass.

The gate needs a stack to do its real work, so what is checked here is only the CONTRACT: n recorded
failures exit non-zero, none exits zero, and the printed verdict agrees with the code. The gate's
`KONTRA_GATE_SELFTEST` hook records synthetic failures and leaves through the ordinary `report` and
trap, so this exercises the real path rather than a copy of it.
"""

from __future__ import annotations

import pathlib
import subprocess

import pytest

GATE = pathlib.Path(__file__).resolve().parents[1] / "scripts" / "parity-gate.sh"


def run_selftest(n: int) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        ["bash", str(GATE)],
        env={"PATH": "/usr/bin:/bin:/usr/local/bin", "KONTRA_GATE_SELFTEST": str(n), "HOME": "/tmp"},
        capture_output=True,
        text=True,
        timeout=300,
    )


def skip_if_refused(res: subprocess.CompletedProcess[str]) -> None:
    """The gate refuses to run beside a live stack, before the hook below is reached.

    Said rather than worked around: the refusal is itself the behaviour this file is about, and a
    test that quietly passed because nothing ran would be the same failure one level up.
    """
    out = res.stdout + res.stderr
    if "REFUSING" in out:
        assert res.returncode != 0, "a refusal that exits 0 is the bug this file is about"
        pytest.skip(
            "the gate refused before its selftest hook (a stack is attached to the `kontra` "
            "network, or a port is taken) — run `docker compose down` to exercise this"
        )


def test_recorded_failures_make_the_gate_fail() -> None:
    res = run_selftest(3)
    skip_if_refused(res)
    out = res.stdout + res.stderr
    assert "PARITY GATE FAILS (3)" in out, out[-2000:]
    assert res.returncode != 0, (
        "the gate printed FAILS and exited 0 — which is how nine real failures reached a green "
        f"CI check. stdout tail:\n{out[-2000:]}"
    )


def test_no_failures_still_passes() -> None:
    res = run_selftest(0)
    skip_if_refused(res)
    out = res.stdout + res.stderr
    assert "PARITY GATE PASSES" in out, out[-2000:]
    assert res.returncode == 0, f"a clean selftest must exit 0, got {res.returncode}\n{out[-2000:]}"


def test_the_body_does_not_hardcode_success() -> None:
    """A belt for the case the hook above cannot reach: the body must not say `exit 0` at the end.

    This is a text check and it is deliberately narrow — the verdict lives in the trap, so the one
    thing the body must never do is pre-empt it.
    """
    lines = [l.strip() for l in GATE.read_text().splitlines() if l.strip()]
    tail = lines[-6:]
    offenders = [l for l in tail if l == "exit 0"]
    assert not offenders, (
        "the last statements of the gate are "
        f"{tail!r} — a bare `exit 0` there overrides the verdict the trap computes"
    )
