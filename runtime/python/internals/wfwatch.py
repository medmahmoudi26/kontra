"""Watch a served workflow's source and RE-REGISTER on save — the form-is-current half of serve.

`kontra workflow serve --watch` sets `KONTRA_WORKFLOW_WATCH`, and `serve_workflows_async` then runs
this loop ALONGSIDE the worker. When a `.py` under the served folder changes, the loop re-derives the
workflow's descriptor and pushes it, so saving in your own editor regenerates the form in the browser
without a page reload — the loop the instrument-panel PRD is built around.

THREE THINGS MAKE THIS HONEST, and every one of them is the point of the slice:

  • ONE DERIVATION PATH. The re-derivation goes through `publish_workflow_catalog`, the SAME code
    that ran on boot, in a FRESH interpreter that imports the file exactly as the worker's boot did.
    There is no static parser reading the annotations a second way — a parser would derive the form's
    fields while the runtime derived its own from real Python types, and the two drift the first time
    somebody uses a type alias or a cross-module import. A subprocess re-import cannot drift from the
    runtime, because it IS the runtime.

  • A BROKEN FILE IS A STATE, NOT A SILENCE. When the re-import raises — a syntax error, a missing
    name — there is no schema to derive, and leaving the last good descriptor standing would paint
    "your contract is fine" over a file that will not load. So the probe posts the import error,
    keyed by the type the manifest declared (`publish_workflow_broken`), and the panel says so.
    Recovering re-imports cleanly and re-publishes, which clears the error and restores the form —
    WITHOUT the serve being restarted, because the worker never stopped: only a short-lived probe ran.

  • THAT STATE BELONGS TO ONE FILE. A save runs one probe PER SERVED FILE, so the SyntaxError in the
    file you just edited is posted against the types THAT file declares and nothing else. A serve
    spanning two files used to re-import the first and post the failure against all of them, which
    stored a healthy workflow as broken under another file's error — with its schemas NULLed and its
    queue replaced by the broken file's, the "starts and routes nowhere" failure `queue` prevents.

THE WORKER IS NEVER TOUCHED. The probe runs in its own process and the long-lived worker keeps
polling its original queue on its original code — which is why the queue the probe re-publishes is the
one the worker is STILL serving, not one re-derived from the edited (and now differently-digested)
folder. Watch mode keeps the FORM current as a liveness check on the operator's code; running the new
code is still an explicit re-serve, exactly as it is without --watch.

No new dependency: mtime polling over stdlib, the same discipline `internals/catalog.py` keeps for the
catalog client. A one-second poll against a handful of files is not a cost worth a watchdog for.
"""

from __future__ import annotations

import asyncio
import importlib.util
import os
import sys
import traceback
from pathlib import Path
from typing import Iterable, Sequence

# The module name the probe imports the served file UNDER. Never `__main__`, so importing the file
# does NOT run its `if __name__ == "__main__": catalog.serve(...)` block — the probe describes the
# workflows, it does not start a second worker.
_PROBE_MODULE = "__kontra_wfwatch_probe__"

_POLL_SECONDS = 1.0


# --- the loop (runs in the serve process) ------------------------------------------------------


def _py_signature(dirs: Iterable[str]) -> dict[str, float]:
    """Every `.py` under `dirs` mapped to its mtime — the thing a save changes.

    RECOMPUTED EACH TICK rather than snapshotted once, so a NEW file (a helper split out mid-edit)
    is watched the moment it appears and a DELETED one stops being watched. Missing files are simply
    absent from the map; a directory that goes away contributes nothing rather than raising.
    """
    sig: dict[str, float] = {}
    for d in dirs:
        try:
            for entry in Path(d).glob("*.py"):
                try:
                    sig[str(entry)] = entry.stat().st_mtime
                except OSError:
                    continue
        except OSError:
            continue
    return sig


async def watch_and_reprobe(
    *,
    targets: Sequence[tuple[str, Sequence[str]]],
    dirs: Sequence[str],
    queue: str,
    interval: float = _POLL_SECONDS,
    python: str = "",
) -> None:
    """Poll `dirs` for a `.py` change; on each one, run a probe PER SERVED FILE that re-derives and
    re-publishes.

    `targets` is one `(file, names)` pair per served source: the file the probe re-imports, and the
    types THAT FILE declared on the last good serve — which the probe posts as broken if that file's
    re-import fails. `queue` is the queue the worker is STILL polling, shared by every target because
    every one of them is served by this worker; the probe re-publishes that, not a queue re-derived
    from edited content.

    ONE PROBE PER FILE, and that is what scopes a failure to the file that produced it. A single
    probe over the first file plus every served name marked HEALTHY workflows broken with another
    file's SyntaxError — and, because the recovering re-import only re-publishes the classes in the
    file it read, that stale error and its NULLed schemas outlived the fix until the worker was
    restarted. Re-deriving every served file on every save is also what makes the loop's documented
    promise true for all of them: whatever still imports is republished clean on the next tick.

    Runs forever (until cancelled, when the worker stops). Never raises out: a save must not be able
    to crash the worker beside it, so every failure in here is printed and swallowed — per target,
    so a probe that cannot even be spawned for one file does not skip the rest of the tick.
    """
    python = python or sys.executable
    last = _py_signature(dirs)
    while True:
        try:
            await asyncio.sleep(interval)
            now = _py_signature(dirs)
            if now == last:
                continue
            last = now
            print(f"[wfwatch] change under {', '.join(dirs)} — re-deriving the contract", flush=True)
            for file, names in targets:
                try:
                    await _run_probe(python=python, file=file, queue=queue, names=names)
                except asyncio.CancelledError:
                    raise
                except Exception as e:  # noqa: BLE001 — one target's failure is not the tick's
                    print(f"[wfwatch] probe for {file} failed ({type(e).__name__}: {e})", flush=True)
        except asyncio.CancelledError:
            raise
        except Exception as e:  # noqa: BLE001 — the watch loop must never take the worker down
            print(f"[wfwatch] watch tick failed ({type(e).__name__}: {e})", flush=True)


async def _run_probe(*, python: str, file: str, queue: str, names: Sequence[str]) -> None:
    """Run one probe in a FRESH interpreter, inheriting this process's environment.

    A subprocess, not an in-process re-import, on purpose: a fresh interpreter's `sys.modules` starts
    empty, so a cross-module edit the running process has already cached is picked up. It inherits
    `KONTRA_ORCHESTRATOR_URL`, `PYTHONPATH` and the codec env the worker was given, so the probe
    reaches the same catalog the boot registration did.
    """
    proc = await asyncio.create_subprocess_exec(
        python, "-m", "internals.wfwatch", "--probe", file, queue, *names
    )
    await proc.wait()


# --- the probe (runs in its own subprocess) ----------------------------------------------------


def _import_fresh(file: str):
    """Import `file` the way the worker's boot did — absolute path, its folder on `sys.path`.

    `python <file>` puts the file's directory on `sys.path[0]`, which is how a workflow that imports
    a sibling module resolves it; the probe reproduces that so a re-import sees exactly what boot saw.
    Imported under `_PROBE_MODULE`, never `__main__`, so the file's serve-block does not fire.
    """
    file = os.path.abspath(file)
    folder = os.path.dirname(file)
    if folder not in sys.path:
        sys.path.insert(0, folder)
    spec = importlib.util.spec_from_file_location(_PROBE_MODULE, file)
    if spec is None or spec.loader is None:
        raise ImportError(f"{file}: not importable as a module")
    module = importlib.util.module_from_spec(spec)
    # Registered before exec so a class that names its own module (Temporal's `from_class` reaches
    # for it) resolves during the import rather than failing halfway through.
    sys.modules[_PROBE_MODULE] = module
    spec.loader.exec_module(module)
    return module


def _import_error(file: str, exc: BaseException) -> str:
    """The import failure as the panel should show it: the file no longer imports, and why.

    The last frame is the one that matters — a `SyntaxError` names its own line, an `ImportError` its
    missing name — so the message leads with the exception and appends a short traceback tail for the
    operator to place it. This is a liveness readout of their own code, so it is verbose on purpose.
    """
    tail = "".join(traceback.format_exception(type(exc), exc, exc.__traceback__)).strip()
    return f"{Path(file).name} no longer imports: {type(exc).__name__}: {exc}\n\n{tail}"


def probe(file: str, queue: str, names: Sequence[str]) -> None:
    """Re-derive and re-publish `file`'s workflows, or post the import error as the broken state.

    The good path is `publish_workflow_catalog` over the FRESH classes — the exact call boot made, so
    the schema the page shows is the one the worker derived and there is no second derivation of it.
    `defn_classes` gives the classes THIS FILE DECLARES, not everything in its namespace, so a
    workflow the file merely imports keeps the queue of the worker that actually serves it.
    The bad path is `publish_workflow_broken` for `names` — the types THIS FILE declared on the last
    good serve, passed in because a file that will not import cannot be asked what it declares. The
    caller groups them per file (`_watch_targets`); a name from another file posted here would be a
    healthy workflow stored broken under an error from code it never touched.
    """
    from internals.catalog import (
        defn_classes,
        publish_workflow_broken,
        publish_workflow_catalog,
    )

    try:
        module = _import_fresh(file)
    except Exception as exc:  # noqa: BLE001 — the whole point: an import failure is a state to post
        publish_workflow_broken(names, _import_error(file, exc), queue=queue)
        return
    publish_workflow_catalog(defn_classes(module), queue=queue)


def _probe_main(argv: Sequence[str]) -> None:
    # argv is `--probe <file> <queue> [names...]`; the queue may be the empty string.
    if len(argv) < 3 or argv[0] != "--probe":
        raise SystemExit("usage: python -m internals.wfwatch --probe <file> <queue> [names...]")
    _, file, queue, *names = argv
    probe(file, queue, names)


if __name__ == "__main__":
    _probe_main(sys.argv[1:])
