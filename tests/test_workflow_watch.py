"""Watch mode re-registers on save, and a broken file is a state — instrument-panel slice 03.

`kontra workflow serve --watch` keeps the worker up and re-derives the contract on every save, so the
browser form tracks the editor. This pins the two properties that make that loop honest, from the
Python side that actually derives:

  • ONE DERIVATION PATH. The probe re-imports the served file in a fresh interpreter and pushes
    through `publish_workflow_catalog` — the SAME call boot made. No static parser reads the
    annotations a second way, so what the form shows cannot drift from what the runtime accepts.

  • A BROKEN FILE IS A STATE, NOT A SILENCE. When the re-import raises, there is no schema to derive;
    the probe posts the import error keyed by the type the manifest declared, so the panel says the
    file no longer imports instead of leaving the last good form standing.

  • THAT STATE, AND EVERY RE-REGISTRATION, BELONGS TO ONE FILE. A save re-derives each served file on
    its own, and a file describes only the workflows it DECLARES — so a typo in one file cannot store
    another file's healthy workflow as broken, and a workflow a file merely imports is not dragged
    onto this worker's queue. Both wrote a record for a workflow the edited file never defined, which
    is a start that routes to a queue nothing serves it on and reports running forever.

The reading side is control/orchestrator/src/catalog.test.ts (the route accepts `error`), orchestrator's repo
test (the store round-trips it) and frontend/src/panels/workflowContract.test.ts (the panel
draws it). The wire body is pinned against catalog.proto in test_workflow_catalog.py.
"""

import asyncio
import contextlib
import importlib.util
import json
import sys
import types
from pathlib import Path

import pytest

import internals.catalog as C
import internals.wfwatch as W
from internals.temporal import wfhost as H

ROOT = Path(__file__).resolve().parent.parent
SHIPPED = ROOT / "testdata" / "workflows"


def load_module(path: Path, name: str):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


class _Accepted:
    status = 200

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False


@pytest.fixture
def posted(monkeypatch):
    """The real emitter with the socket removed; every body it would have sent, collected."""
    bodies = []

    def fake_urlopen(req, timeout=None):
        bodies.append((req.full_url, json.loads(req.data)))
        return _Accepted()

    monkeypatch.setattr(C.urllib.request, "urlopen", fake_urlopen)
    monkeypatch.setenv("KONTRA_ORCHESTRATOR_URL", "http://orchestrator-api:8088")
    return bodies


# --- defn_classes: the one notion of "which classes are workflows" -----------------------------


def test_defn_classes_finds_the_workflows_and_skips_helpers(tmp_path):
    """The same predicate `serve()` is handed a list against and the catalog test uses — Temporal's
    own `from_class`, not a name convention — so watch mode and boot agree on what a workflow is."""
    src = '''
from temporalio import workflow


@workflow.defn
class A:
    @workflow.run
    async def run(self, req: dict) -> dict: ...


@workflow.defn
class B:
    @workflow.run
    async def run(self, req: dict) -> dict: ...


class Helper:  # not decorated — must be skipped
    pass
'''
    path = tmp_path / "workflow.py"
    path.write_text(src)
    module = load_module(path, "defn_classes_fixture")
    assert [c.__name__ for c in C.defn_classes(module)] == ["A", "B"]


# --- publish_workflow_broken: the broken state, on the wire ------------------------------------


def test_broken_posts_one_descriptor_per_type_with_the_error_and_queue(posted):
    """A file that will not import cannot be asked what it declares, so the caller passes the types
    the last good serve knew. Each gets the error verbatim and the queue the worker is STILL
    polling — the page keeps its poller signal while the form is replaced by the error."""
    C.publish_workflow_broken(["NsCheck"], "workflow.py no longer imports: SyntaxError", queue="wf-nscheck-abc")

    (url, body), = posted
    assert url.endswith("/api/workflows/catalog")
    assert body == {
        "name": "NsCheck",
        "error": "workflow.py no longer imports: SyntaxError",
        "queue": "wf-nscheck-abc",
    }


def test_broken_carries_no_schema_so_an_overwrite_clears_the_stale_form(posted):
    """The whole point of posting the broken state rather than staying silent: it has no input or
    output, so the store's upsert overwrites those slots to NULL and the last good form drops."""
    C.publish_workflow_broken(["Ping"], "boom", queue="")
    (_, body), = posted
    assert "input" not in body and "output" not in body
    # No queue was known, so none is sent — absent, not the empty string.
    assert "queue" not in body


def test_broken_is_gated_on_the_orchestrator_url(monkeypatch):
    """The same gate every catalog call has: no URL means this installation has no catalog, so a
    broken post is a no-op rather than a crash on a save."""
    called = []
    monkeypatch.setattr(C, "register_workflow_catalog", lambda *a, **k: called.append(1))
    monkeypatch.delenv("KONTRA_ORCHESTRATOR_URL", raising=False)
    C.publish_workflow_broken(["NsCheck"], "boom")
    assert called == []


def test_broken_post_that_fails_is_printed_and_swallowed(monkeypatch, capsys):
    """A save must not become a crash. If the catalog is unreachable the last contract stands and
    the failure is printed, never raised — the same best-effort stance the register path takes."""
    monkeypatch.setenv("KONTRA_ORCHESTRATOR_URL", "http://orchestrator-api:8088")

    def boom(url, descriptor, *, timeout=5.0):
        raise OSError("connection refused")

    monkeypatch.setattr(C, "register_workflow_catalog", boom)
    C.publish_workflow_broken(["NsCheck"], "boom")  # must not raise
    assert "broken-state post failed" in capsys.readouterr().out


# --- the probe: the two states, through the runtime derivation ---------------------------------


def test_probe_reimports_and_republishes_through_the_catalog(posted, monkeypatch):
    """The good path: a fresh import of the served file, described through `publish_workflow_catalog`
    — the exact call boot made. The schema the page shows is the one the runtime derived, and there
    is no second derivation of it anywhere."""
    published = []
    real = C.publish_workflow_catalog

    def spy(classes, **kw):
        published.append(([c.__name__ for c in classes], kw.get("queue")))
        return real(classes, **kw)

    # Patched on `internals.catalog`, where the probe imports it from at call time.
    monkeypatch.setattr(C, "publish_workflow_catalog", spy)
    W.probe(str(SHIPPED / "nscheck" / "workflow.py"), "wf-nscheck-abc", ["NsCheck"])

    assert published == [(["NsCheck"], "wf-nscheck-abc")]
    (url, body), = posted
    assert body["name"] == "NsCheck"
    assert "properties" in body["input"], "the typed input came through the real derivation"
    assert "error" not in body, "a clean import posts no broken state"


def test_probe_posts_the_import_error_when_the_file_no_longer_imports(tmp_path, posted):
    """The broken path: a syntax error where a schema should be. The probe posts the error keyed by
    the type the manifest declared — not the last good form left standing as though nothing broke."""
    broken = tmp_path / "workflow.py"
    broken.write_text("def run(  # a save mid-edit: the parens never close\n")

    W.probe(str(broken), "wf-x-123", ["NsCheck"])

    (url, body), = posted
    assert body["name"] == "NsCheck"
    assert body["queue"] == "wf-x-123"
    assert "no longer imports" in body["error"]
    assert "SyntaxError" in body["error"]
    # The state REPLACES the contract: no schema rides with a broken descriptor.
    assert "input" not in body and "output" not in body


def test_probe_recovers_the_form_after_the_error_is_fixed(tmp_path, posted):
    """Recovering restores the form: the same file, first broken then fixed, re-imports clean and
    the second post carries the schema again with no error. This is the recovery the panel shows
    without the serve being restarted — the probe is a subprocess; the worker never stopped."""
    f = tmp_path / "workflow.py"
    f.write_text("def oops(  # broken\n")
    W.probe(str(f), "wf-x-123", ["Demo"])

    f.write_text(
        "from temporalio import workflow\n"
        "from typing_extensions import TypedDict\n"
        "\n"
        "class In(TypedDict, total=False):\n"
        "    n: int\n"
        "\n"
        "@workflow.defn\n"
        "class Demo:\n"
        "    @workflow.run\n"
        "    async def run(self, req: In) -> dict: ...\n"
    )
    W.probe(str(f), "wf-x-123", ["Demo"])

    (_, broken_body), (_, fixed_body) = posted
    assert "error" in broken_body and "input" not in broken_body
    assert "error" not in fixed_body
    assert fixed_body["name"] == "Demo"
    assert list(fixed_body["input"]["properties"]) == ["n"]


# --- the hook: --watch is opt-in, and a plain serve is unchanged -------------------------------


def _serve_once(monkeypatch, classes, *, watch_env, booted=None):
    """Run the real `serve_workflows_async` with Temporal replaced at its seams; return whether a
    watcher was started. `watch_env` is what KONTRA_WORKFLOW_WATCH is set to (None deletes it).

    `booted`, when given, collects `(class names, queue)` for the BOOT registration — the one call
    that is not watch mode, and the one that must stay exactly as it was."""
    import asyncio

    import temporalio.client
    import temporalio.worker

    from internals.temporal import wfhost

    class FakeClient:
        pass

    async def fake_connect(address, **kw):
        return FakeClient()

    class FakeWorker:
        def __init__(self, *a, **kw):
            pass

        async def run(self):
            # Yield once so a watcher task created beside this one gets to run before the finally
            # cancels it — a real `worker.run()` blocks, and this stands in for that suspension.
            import asyncio as _asyncio

            await _asyncio.sleep(0)
            return None

    monkeypatch.setattr(temporalio.client.Client, "connect", staticmethod(fake_connect))
    monkeypatch.setattr(temporalio.worker, "Worker", FakeWorker)
    def boot_publish(ws, **k):
        if booted is not None:
            booted.append(([getattr(w, "__name__", str(w)) for w in ws], k.get("queue")))

    monkeypatch.setattr(C, "publish_workflow_catalog", boot_publish)

    started = []

    async def fake_watch(**kw):
        started.append(kw)

    # Patched where wfhost imports it from at call time (`from internals.wfwatch import ...`).
    monkeypatch.setattr(W, "watch_and_reprobe", fake_watch)

    if watch_env is None:
        monkeypatch.delenv("KONTRA_WORKFLOW_WATCH", raising=False)
    else:
        monkeypatch.setenv("KONTRA_WORKFLOW_WATCH", watch_env)

    asyncio.run(wfhost.serve_workflows_async(classes, task_queue="scratch"))
    return started


def test_a_plain_serve_starts_no_watcher(monkeypatch):
    """Watch mode is opt-in: without KONTRA_WORKFLOW_WATCH the serve behaves exactly as before, with
    no watcher and no re-derivation loop."""
    module = load_module(SHIPPED / "nscheck" / "workflow.py", "watch_plain_nscheck")
    started = _serve_once(monkeypatch, C.defn_classes(module), watch_env=None)
    assert started == []


def test_watch_starts_the_loop_with_the_served_queue_and_types(monkeypatch):
    """--watch starts the watcher, and hands it the queue THIS worker is polling and one target per
    served file — so a re-publish keeps the queue the running code answers on, not one re-derived
    from the edited folder, and each file carries the types IT declared."""
    monkeypatch.setenv("KONTRA_ORCHESTRATOR_URL", "http://orchestrator-api:8088")
    module = load_module(SHIPPED / "nscheck" / "workflow.py", "watch_on_nscheck")
    classes = C.defn_classes(module)
    started = _serve_once(monkeypatch, classes, watch_env="1")
    assert len(started) == 1
    kw = started[0]
    assert kw["queue"] == "scratch"
    (file, names), = kw["targets"]
    assert names == ["NsCheck"]
    assert file.endswith("nscheck/workflow.py")


def test_watch_with_no_orchestrator_url_serves_without_the_loop(monkeypatch):
    """--watch with nowhere to publish is said out loud and served without a watcher: the loop's
    whole value is the form in a browser, and there is no catalog to reach."""
    monkeypatch.delenv("KONTRA_ORCHESTRATOR_URL", raising=False)
    module = load_module(SHIPPED / "nscheck" / "workflow.py", "watch_nourl_nscheck")
    started = _serve_once(monkeypatch, C.defn_classes(module), watch_env="1")
    assert started == []


# --- one save, one file: what a re-derivation is allowed to rewrite ----------------------------
#
# The two defects here compound, and both write the record of a workflow the edited file never
# defined. A serve spanning two files re-imported the FIRST one and posted its failure against
# EVERY served name, so a healthy workflow was stored broken under a SyntaxError from code it has
# nothing to do with — schemas NULLed by the store's overwriting upsert, queue replaced by the
# broken file's. And the good path scraped the re-imported module's whole namespace, so a workflow
# the file merely IMPORTS was re-registered on this file's queue. Either way a start lands on a
# queue nothing serves that type on and reports `running` forever — the failure `queue` exists to
# prevent. The fix is per-file: one probe per served file, each describing only what it declares.

ALPHA_SRC = '''
from temporalio import workflow
from typing_extensions import TypedDict

from delta import Delta   # a workflow ANOTHER worker serves; this file only starts it


class AlphaIn(TypedDict, total=False):
    a: int


@workflow.defn
class Alpha:
    """Alpha, declared here."""

    @workflow.run
    async def run(self, req: AlphaIn) -> dict:
        return {"child": Delta.__name__}
'''

BETA_SRC = '''
from temporalio import workflow
from typing_extensions import TypedDict


class BetaIn(TypedDict, total=False):
    b: str


@workflow.defn
class Beta:
    """Beta, declared in its own file and served by the same worker."""

    @workflow.run
    async def run(self, req: BetaIn) -> dict: ...
'''

DELTA_SRC = '''
from temporalio import workflow
from typing_extensions import TypedDict


class DeltaIn(TypedDict, total=False):
    d: int


@workflow.defn
class Delta:
    """Delta, served by a DIFFERENT worker on a different queue."""

    @workflow.run
    async def run(self, req: DeltaIn) -> dict: ...
'''


class _Catalog:
    """The catalog with the socket removed, plus the `workflows` table as the store writes it.

    `Repo.upsertWorkflow` (control/orchestrator/src/db/repo.ts) is `ON CONFLICT(name) DO UPDATE SET` over
    EVERY column, so a re-registration REPLACES the row: a field the worker omits is written NULL,
    not left alone. That is why a wrongly-addressed post is not a harmless duplicate — it is what
    turns a healthy workflow's schemas into NULL and moves its queue.
    """

    def __init__(self):
        self.posts: list[dict] = []
        self.rows: dict[str, dict] = {}

    def accept(self, body: dict):
        self.posts.append(body)
        self.rows[body["name"]] = {
            "queue": body.get("queue"),
            "input": body.get("input"),
            "output": body.get("output"),
            "error": body.get("error"),
        }
        return _Accepted()

    def names(self) -> list[str]:
        return [b["name"] for b in self.posts]


@pytest.fixture
def catalog(monkeypatch):
    store = _Catalog()

    def fake_urlopen(req, timeout=None):
        return store.accept(json.loads(req.data))

    monkeypatch.setattr(C.urllib.request, "urlopen", fake_urlopen)
    monkeypatch.setenv("KONTRA_ORCHESTRATOR_URL", "http://orchestrator-api:8088")
    return store


@pytest.fixture
def served(tmp_path, monkeypatch):
    """One worker serving `[Alpha, Beta]` out of two files, beside another worker's `Delta`.

    `wf/` is this worker's folder — `alpha.py` and `beta.py`, one queue, the two files a single
    `serve()` spans. `shared/` holds `delta.py`, which `alpha.py` imports to start as a child and
    which a DIFFERENT worker serves on its own queue; it is on `sys.path` the way the probe
    inherits `PYTHONPATH` from the worker, and it is deliberately NOT one of the watched folders.
    """
    wf = tmp_path / "wf"
    wf.mkdir()
    shared = tmp_path / "shared"
    shared.mkdir()
    (shared / "delta.py").write_text(DELTA_SRC)
    (wf / "beta.py").write_text(BETA_SRC)
    (wf / "alpha.py").write_text(ALPHA_SRC)

    monkeypatch.syspath_prepend(str(shared))
    monkeypatch.syspath_prepend(str(wf))
    loaded = ["delta", "beta", "alpha"]
    delta = load_module(shared / "delta.py", "delta")
    beta = load_module(wf / "beta.py", "beta")
    alpha = load_module(wf / "alpha.py", "alpha")
    yield types.SimpleNamespace(
        root=tmp_path,
        dir=str(wf),
        alpha_py=str(wf / "alpha.py"),
        beta_py=str(wf / "beta.py"),
        Alpha=alpha.Alpha,
        Beta=beta.Beta,
        Delta=delta.Delta,
    )
    for name in loaded + [W._PROBE_MODULE]:
        sys.modules.pop(name, None)


def _fresh_probe(file: str, queue: str, names, root: Path) -> None:
    """The real probe, with the fresh-interpreter property the subprocess gives it for free.

    A probe normally runs in its own process, so `sys.modules` starts empty and a cross-module edit
    the parent already cached is picked up. In-process, dropping the fixture's modules first is what
    reproduces that — without it a second probe would describe the file's FIRST contents.
    """
    for name, mod in list(sys.modules.items()):
        if str(getattr(mod, "__file__", "") or "").startswith(str(root)):
            del sys.modules[name]
    W.probe(file, queue, names)


def _watch_tick(monkeypatch, served, targets, queue, save) -> list[tuple[str, list[str]]]:
    """One save, driven through the REAL watch loop with only the process boundary replaced.

    `_run_probe` is the only seam faked — spawning `python -m internals.wfwatch --probe` per target
    is what a tick would otherwise cost, and the probe's own behaviour is the thing under test. What
    it returns is `(file, names)` per probe the tick actually ran, in order.
    """
    ran: list[tuple[str, list[str]]] = []

    async def fake_run_probe(*, python, file, queue, names):
        ran.append((file, list(names)))
        _fresh_probe(file, queue, names, served.root)

    monkeypatch.setattr(W, "_run_probe", fake_run_probe)

    async def drive():
        task = asyncio.create_task(
            W.watch_and_reprobe(
                targets=targets, dirs=[served.dir], queue=queue, interval=0.01
            )
        )
        await asyncio.sleep(0.05)   # the loop takes its mtime snapshot, then sleeps
        save()
        for _ in range(200):
            await asyncio.sleep(0.01)
            if len(ran) >= len(targets):
                break
        task.cancel()
        with contextlib.suppress(asyncio.CancelledError):
            await task

    asyncio.run(drive())
    return ran


def _boot(catalog, served):
    """Both workers register, then the store is snapshotted — the state a save has to survive."""
    C.publish_workflow_catalog([served.Delta], queue="wf-delta-ddd")
    C.publish_workflow_catalog([served.Alpha, served.Beta], queue="wf-mixed-abc")
    before = {name: dict(row) for name, row in catalog.rows.items()}
    catalog.posts.clear()
    return before


# --- the two halves, at their source ------------------------------------------------------------


def test_defn_classes_skips_a_workflow_the_file_only_imports(served):
    """`vars(module)` holds what a file IMPORTED as readily as what it DECLARED, so scraping it
    re-registered another worker's workflow under this file's queue. `__module__` is what the class
    body recorded when it ran, so it names the file that declared it however far it travels."""
    module = W._import_fresh(served.alpha_py)

    assert "Delta" in vars(module), "the fixture is only meaningful if the name IS in the namespace"
    assert [c.__name__ for c in C.defn_classes(module)] == ["Alpha"]


def test_watch_targets_groups_each_name_under_the_file_that_declared_it(served):
    """The root of the other half: this returned the FIRST served file plus the names of ALL of
    them, so one file's import failure was posted against every type the worker serves."""
    targets, dirs = H._watch_targets([served.Alpha, served.Beta])

    assert targets == [(served.alpha_py, ["Alpha"]), (served.beta_py, ["Beta"])]
    assert dirs == [served.dir]


def test_watch_targets_drops_a_class_whose_source_cannot_be_located(monkeypatch, served):
    """A name with no file is exactly the name that used to ride along on another file's failure:
    there is nothing to re-import for it, so it contributes no target rather than a floating name.

    A class from a module with no `__file__` — defined in a REPL, or generated at import time — is
    the honest way to get one: `inspect.getfile` raises for it, which is the case the old code
    swallowed while still keeping the name."""
    module = types.ModuleType("kontra_watch_no_file")
    monkeypatch.setitem(sys.modules, module.__name__, module)
    exec(compile(BETA_SRC.replace("Beta", "Nowhere"), "<generated>", "exec"), module.__dict__)

    targets, _ = H._watch_targets([served.Alpha, module.Nowhere])

    assert targets == [(served.alpha_py, ["Alpha"])]


def test_the_tick_runs_one_probe_per_served_file(monkeypatch, catalog, served):
    """One probe per served file is what scopes a failure to the file that produced it — and what
    keeps the loop's promise for every served workflow rather than only the one file it re-read."""
    targets, _ = H._watch_targets([served.Alpha, served.Beta])
    ran = _watch_tick(
        monkeypatch, served, targets, "wf-mixed-abc",
        lambda: Path(served.beta_py).write_text(BETA_SRC + "\n# saved\n"),
    )

    assert ran == [(served.alpha_py, ["Alpha"]), (served.beta_py, ["Beta"])]


# --- the compound case: one save of one file, two records that must not move --------------------


def test_a_broken_file_leaves_every_other_workflow_record_untouched(monkeypatch, catalog, served):
    """The compound failure, in one save. A typo in `alpha.py` used to (1) post `Beta` BROKEN under
    alpha's SyntaxError — NULLing its schemas and taking alpha's queue — and, on the good path,
    (2) re-register `Delta`, which alpha only imports, onto alpha's queue away from the worker that
    actually serves it. Neither workflow is declared in the file that was saved, and neither record
    may move: `Beta` is re-derived clean from its own file, `Delta` is not written at all."""
    before = _boot(catalog, served)
    targets, _ = H._watch_targets([served.Alpha, served.Beta])

    _watch_tick(
        monkeypatch, served, targets, "wf-mixed-abc",
        lambda: Path(served.alpha_py).write_text("def broken(  # a save mid-edit\n"),
    )

    assert catalog.names() == ["Alpha", "Beta"], "Delta is not this file's to re-register"
    alpha = catalog.rows["Alpha"]
    assert "no longer imports" in alpha["error"] and "SyntaxError" in alpha["error"]
    assert alpha["input"] is None, "the broken state replaces the form for the file that broke"
    assert alpha["queue"] == "wf-mixed-abc", "the worker is still polling its queue"
    # Everything the save did not declare is byte-for-byte what it was.
    assert catalog.rows["Beta"] == before["Beta"]
    assert catalog.rows["Delta"] == before["Delta"]
    assert catalog.rows["Delta"]["queue"] == "wf-delta-ddd", "still addressed to its own worker"


def test_the_broken_file_recovers_on_the_next_save_without_a_worker_restart(
    monkeypatch, catalog, served
):
    """The loop's documented promise, kept: a clean re-save clears the error and restores the form,
    with no serve restarted — the worker never stopped, only short-lived probes ran. It used to hold
    for the edited file alone, because the recovering re-import republishes only the classes IN that
    file, so anything else it had marked broken kept a stale error and a NULL schema until restart."""
    before = _boot(catalog, served)
    targets, _ = H._watch_targets([served.Alpha, served.Beta])

    _watch_tick(
        monkeypatch, served, targets, "wf-mixed-abc",
        lambda: Path(served.alpha_py).write_text("def broken(  # a save mid-edit\n"),
    )
    assert catalog.rows["Alpha"]["error"], "precondition: the save did break it"

    _watch_tick(
        monkeypatch, served, targets, "wf-mixed-abc",
        lambda: Path(served.alpha_py).write_text(ALPHA_SRC),
    )

    assert catalog.rows["Alpha"] == before["Alpha"], "the form is back, the error is gone"
    assert catalog.rows["Beta"] == before["Beta"]
    assert catalog.rows["Delta"] == before["Delta"]


# --- the boot path is not watch mode, and does not change ---------------------------------------


def test_boot_registers_the_serve_list_verbatim(monkeypatch):
    """Neither defect can reach boot: `serve()` publishes the explicit list its author handed over,
    and never asks a module what is in it. This pins that the list arrives unfiltered."""
    module = load_module(SHIPPED / "nscheck" / "workflow.py", "boot_verbatim_nscheck")
    booted: list = []

    _serve_once(monkeypatch, [module.NsCheck], watch_env=None, booted=booted)

    assert booted == [(["NsCheck"], "scratch")]


def test_boot_still_registers_a_workflow_the_served_file_imports(catalog, served):
    """The other side of the same coin. Narrowing `defn_classes` must not narrow what an AUTHOR can
    serve: a file that imports a workflow and lists it in `serve()` registers it, on this worker's
    queue, because this worker really is about to poll for it."""
    C.publish_workflow_catalog([served.Alpha, served.Delta], queue="wf-mixed-abc")

    assert catalog.names() == ["Alpha", "Delta"]
    assert catalog.rows["Delta"]["queue"] == "wf-mixed-abc"
