"""Emit an actor's contract as JSON, from the code on disk — no orchestrator, no deploy.

WHY THIS EXISTS. kontra already derives JSON Schema from an actor's declared types: a Method says
`takes=Target, emits=Page` and `kontra.schema.schema_of` turns those into Draft 2020-12 via pydantic.
That derivation is better founded than parsing source text, because the types ARE the declaration
rather than a guess at it — but it only ran at REGISTRATION. A Method being edited had no schema
until it was deployed, which is exactly backwards for the loop where the schema is most useful: a
form beside the editor, tracking the code in front of you.

THIS IS A THIN WRAPPER AND MUST STAY ONE. It calls `internals.loader.load_actor` and
`internals.catalog.operations_of` — the same two functions a booting worker calls to publish its
catalog. It derives nothing itself. A second derivation path would be a second answer to "what does
this Method accept", and the two would drift the moment one of them was fixed.

Run as:  python -m internals.schemadump <actor-dir> [--method NAME]
with PYTHONPATH covering sdk/python and runtime/python (what `kontra actor schema` sets).
"""

from __future__ import annotations

import argparse
import json
import sys
import traceback
from pathlib import Path


def contract_of(path: str, method: str | None = None) -> dict:
    """The actor's identity plus one entry per Method, schemas included.

    Raises `SystemExit` with a message naming the file when the module cannot be imported — an
    import error is a fact about the author's code, and reporting it as "no schema" would send
    someone looking for a missing decorator when the real problem is a missing dependency.
    """
    from internals.catalog import operations_of
    from internals.loader import load_actor

    loaded = load_actor(path)
    registry = loaded.registry

    ops = operations_of(registry)
    if method is not None:
        ops = [op for op in ops if op.get("name") == method]
        if not ops:
            known = ", ".join(sorted(op.get("name", "") for op in operations_of(registry))) or "none"
            raise SystemExit(f"no Method named {method!r} in {path} — this actor declares: {known}")

    return {
        "actor": {
            "name": getattr(registry, "actor_name", "") or "",
            "version": getattr(registry, "version", "") or "",
        },
        "dir": str(Path(loaded.actor_dir).resolve()),
        # A Method that declares neither `takes` nor `emits` appears here with no `input`/`output`
        # key at all — omitted, never null. That is `operations_of`'s existing contract and it is
        # meaningful: undeclared is dispatchable, it simply advertises nothing.
        "methods": ops,
    }


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(prog="kontra actor schema", add_help=True)
    ap.add_argument("actor_dir", help="the actor directory (or its actor.py)")
    ap.add_argument("--method", default=None, help="narrow to one Method by name")
    args = ap.parse_args(argv)

    try:
        out = contract_of(args.actor_dir, args.method)
    except SystemExit:
        raise
    except Exception as exc:  # noqa: BLE001 — the traceback IS the useful output here
        # NAMING WHAT FAILED, on stderr, with the traceback. An actor that cannot be imported is
        # the common case during editing (a half-written import, a missing package), and the author
        # needs the real error, not this tool's summary of it.
        print(f"could not load the actor at {args.actor_dir}: {exc}", file=sys.stderr)
        traceback.print_exc()
        return 1

    json.dump(out, sys.stdout, indent=2, sort_keys=False)
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
