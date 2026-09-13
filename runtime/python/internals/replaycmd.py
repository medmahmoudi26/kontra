"""The command line behind `kontra workflow replay` and `kontra workflow history`.

Kept separate from `replay.py` so the library is importable by a test (and by CI's determinism
guard) without dragging argument parsing along.

Run as:  python -m internals.replaycmd replay  <workflow-file> [--run-id ID | --history FILE]
         python -m internals.replaycmd history <run-id> [-o FILE]
"""

from __future__ import annotations

import argparse
import asyncio
import json
import sys
import traceback

from internals.replay import (
    Outcome,
    fetch_history,
    load_history_file,
    replay_history,
    workflow_classes,
)


def _cmd_history(args: argparse.Namespace) -> int:
    try:
        history = asyncio.run(fetch_history(args.workflow_id, run_id=args.run_id))
    except Exception as exc:  # noqa: BLE001
        print(f"could not fetch the history for {args.workflow_id}: {exc}", file=sys.stderr)
        return 1
    body = json.dumps(history, indent=2)
    if args.out:
        with open(args.out, "w") as f:
            f.write(body + "\n")
        print(f"{args.out} — {len(history.get('events', []))} events", file=sys.stderr)
    else:
        sys.stdout.write(body + "\n")
    return 0


def _cmd_replay(args: argparse.Namespace) -> int:
    # THE CLASSES FIRST. An import error here is the operator's own file failing, and it must be
    # reported as that rather than as a replay outcome — they are different problems with different
    # fixes, and this is the one that comes with a traceback worth reading.
    try:
        workflows = workflow_classes(args.workflow_file)
    except Exception as exc:  # noqa: BLE001
        print(f"could not import {args.workflow_file}: {exc}", file=sys.stderr)
        traceback.print_exc()
        return 2

    try:
        history = (
            load_history_file(args.history)
            if args.history
            else asyncio.run(fetch_history(args.workflow_id or args.run_id, run_id=args.run_id))
        )
    except SystemExit as exc:
        # `load_history_file` raises SystemExit, whose default code is 1 — and 1 is reserved here
        # for "the code is non-deterministic". A missing file reported that way sends somebody to
        # rewrite a workflow because of a typo in a path. Setup problems are ALWAYS 2.
        print(str(exc), file=sys.stderr)
        return 2
    except Exception as exc:  # noqa: BLE001
        print(f"could not obtain a history: {exc}", file=sys.stderr)
        return 2

    result = asyncio.run(replay_history(history, workflows))
    if args.json:
        json.dump(result.as_json(), sys.stdout, indent=2)
        sys.stdout.write("\n")
    else:
        names = ", ".join(getattr(w, "__name__", str(w)) for w in workflows)
        print(f"{result.workflow_type or '(unknown type)'} — replayed against {names}")
        if result.outcome is Outcome.OK:
            print("OK — the history replays cleanly against this code")
        else:
            print(f"{result.outcome.value.upper()}: {result.detail}")

    # THREE OUTCOMES, THREE EXIT CODES, because CI branches on them and a human reads them:
    #   0 clean · 1 the code is non-deterministic against this history · 2 it could not be run
    return {Outcome.OK: 0, Outcome.NONDETERMINISTIC: 1, Outcome.UNRUNNABLE: 2}[result.outcome]


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(prog="kontra workflow")
    sub = ap.add_subparsers(dest="cmd", required=True)

    h = sub.add_parser("history", help="save a run's history as JSON")
    h.add_argument("workflow_id", help="the workflow id")
    h.add_argument("--run-id", dest="run_id", default="", help="pin one attempt")
    h.add_argument("-o", "--out", default="")
    h.set_defaults(fn=_cmd_history)

    r = sub.add_parser("replay", help="replay a history against workflow code on disk")
    r.add_argument("workflow_file")
    r.add_argument("--run-id", default="")
    r.add_argument("--workflow-id", default="")
    r.add_argument("--history", default="", help="replay a saved history instead of fetching one")
    r.add_argument("--json", action="store_true")
    r.set_defaults(fn=_cmd_replay)

    args = ap.parse_args(argv)
    if args.cmd == "replay" and not args.run_id and not args.history:
        ap.error("replay needs --run-id <id> or --history <file>")
    return int(args.fn(args))


if __name__ == "__main__":
    raise SystemExit(main())
