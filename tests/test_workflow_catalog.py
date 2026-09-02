"""A workflow registers what it takes, returns and is for — the caller's half of the catalog.

An Actor self-registers on serve and a workflow did not, so `GET /api/workflows` could say a file
exists and nothing about its contract — while the answer was already written in the file, in the
`@workflow.run` annotations and the class docstring. This pins the derivation, the wire body, and
the two properties that make registration safe to have: it never blocks serving, and one workflow's
failure never costs another its registration.

REAL WORKFLOWS ARE THE FIXTURE. `testdata/workflows/{ping,nscheck}/workflow.py` are imported and
described here rather than mimicked, because a hand-written class that happens to match the emitter
proves only that the emitter agrees with itself.

THEY HAVE TO BE SHIPPED IN THIS REPO FOR THAT SENTENCE TO BE TRUE, and this has now been got wrong
twice in opposite directions. First they were read from `.kontra/workflows/` — the OPERATOR's
directory, gitignored on purpose because what lives there is theirs — which resolves on any machine
that has run the thing and on no fresh checkout, so the suite passed locally for everyone who wrote
it and failed the first time CI saw it, eleven tests at once, on a FileNotFoundError that says
nothing about workflows. Then they were read from `examples/python/workflows/`, and ADR 0038 moved
that tree to the kontra-workflows repository, which is the same failure with a different path.

SO THEY ARE COPIES NOW, AND THAT COSTS SOMETHING WORTH NAMING. `testdata/workflows/` holds a copy
of each, taken from kontra-workflows `python/`. Nothing checks that they are still current — if
`nscheck` changes there, this suite keeps passing against the old shape. What it still proves is
that the DERIVATION works on workflow source with the structure real workflows have, which is what
these tests are about; what it no longer proves is that the shipped examples themselves stay
describable. The same trade was made for the fixture actor (ADR 0038) and for kontra-console's
`workflowSource.test.ts`, for the same reason: a unit suite cannot require a second checkout.

BOTH READINGS ARE NOW OBSERVABLE FROM THE SHIPPED WORKFLOWS. `ping` still annotates `dict` on both
slots, which derives a schema with NO properties — "any object" — and that is what the console must
draw as *declares no fields* rather than as an empty table. `nscheck` now names its request
(`NsCheckInput`, instrument-panel slice 01), so its INPUT derives `properties` and its OUTPUT is
still `dict` — one workflow that carries both readings at once, which is exactly the pair slices 02
and 03 need to be visible instead of a correct no-op.

The peer on the reading side is backend/src/catalog.test.ts (what the route refuses) and
frontend/src/panels/workflowContract.test.ts (how the three answers are drawn).
"""

import importlib.util
import json
import sys
from dataclasses import dataclass
from pathlib import Path

import pytest
from temporalio import workflow

import internals.catalog as C

ROOT = Path(__file__).resolve().parent.parent
SHIPPED = ROOT / "testdata" / "workflows"


def load_module(path: Path, name: str):
    """Import a .py file as a module of its own name.

    BY PATH, because every workflow folder holds a file called `workflow.py` — importing them by
    module name would give the same module twice and describe one workflow under both names.
    """
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


def defns_in(module) -> list[type]:
    """Every `@workflow.defn` class in a module, in definition order."""
    from temporalio import workflow

    return [
        v
        for v in vars(module).values()
        if isinstance(v, type) and workflow._Definition.from_class(v) is not None
    ]


class _Accepted:
    """The 200 the orchestrator would have returned, with urlopen's context-manager shape."""

    status = 200

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False


@pytest.fixture
def posted(monkeypatch):
    """Run the real emitter with the socket removed; collect every body it would have sent."""
    bodies = []

    def fake_urlopen(req, timeout=None):
        bodies.append((req.full_url, json.loads(req.data)))
        return _Accepted()

    monkeypatch.setattr(C.urllib.request, "urlopen", fake_urlopen)
    monkeypatch.setenv("KONTRA_ORCHESTRATOR_URL", "http://orchestrator-api:8088")
    return bodies


# --- the shipped workflows, described ----------------------------------------------------------


@pytest.mark.parametrize(
    "folder,type_name", [("ping", "Ping"), ("nscheck", "NsCheck")]
)
def test_a_shipped_workflow_registers_its_type_and_both_schemas(folder, type_name):
    """The name is the TYPE a caller starts, not the file it lives in.

    `nscheck/workflow.py` declares `NsCheck`; a descriptor named for the file or the folder would
    name something Temporal will accept a start for and no worker has registered — a run that
    hangs with no error anywhere.
    """
    module = load_module(SHIPPED / folder / "workflow.py", f"shipped_{folder}")
    (cls,) = defns_in(module)
    d = C.workflow_descriptor(cls)
    assert d["name"] == type_name
    assert "input" in d and "output" in d


def test_a_dict_annotated_workflow_declares_a_schema_with_no_fields():
    """`dict` derives a schema that names NO properties, and that is not an empty schema.

    `ping` is the pure example — `dict | None` in, `dict` out — and the case the console has to get
    right: a field table with zero rows would read as "this workflow takes an object with no fields
    in it", where the schema says the opposite — anything at all fits. `nscheck` used to be the
    second such case and no longer is (its input is typed now); its OUTPUT still exercises this path
    and is checked in `test_nscheck_declares_a_typed_input_and_keeps_a_no_fields_output`.
    """
    module = load_module(SHIPPED / "ping" / "workflow.py", "shipped_fields_ping")
    (cls,) = defns_in(module)
    d = C.workflow_descriptor(cls)
    for slot in ("input", "output"):
        assert "properties" not in d[slot], f"{slot} was expected to name no fields: {d[slot]}"


def test_nscheck_declares_a_typed_input_and_keeps_a_no_fields_output():
    """The change instrument-panel slice 01 exists to make: `nscheck` names its request.

    Its `@workflow.run` takes `NsCheckInput` — a `TypedDict` naming the knobs the body already reads
    (`dataset`, `into`, `machines`, `sessions`, `size`) — so the registered descriptor's INPUT
    carries `properties` and the Workflows page draws a field table instead of *declares no fields*.
    The OUTPUT is still `dict`, so the no-fields reading and the fields reading coexist on ONE
    shipped workflow — which is what makes slices 02 and 03 observable rather than a correct no-op.

    None of the knobs is `required`: every one has a default in the body (`total=False`), so the
    form offers them without demanding them.
    """
    module = load_module(SHIPPED / "nscheck" / "workflow.py", "shipped_typed_nscheck")
    (cls,) = defns_in(module)
    d = C.workflow_descriptor(cls)
    assert sorted(d["input"]["properties"]) == ["dataset", "into", "machines", "sessions", "size"]
    assert "required" not in d["input"], "every knob has a default; none is required"
    assert "properties" not in d["output"], f"output still `dict`, expected no fields: {d['output']}"


@pytest.mark.parametrize("folder", ["ping", "nscheck"])
def test_a_shipped_workflow_never_registers_an_empty_description(folder):
    """Absent is absent, never "".

    Both shipped workflows put their prose in the MODULE docstring and their `description.md`, and
    carry no class docstring at all — so both register with no `description` key. An emitter that
    sent "" would make "the author wrote nothing" indistinguishable from "the author wrote a blank
    line", and the page draws those differently.
    """
    module = load_module(SHIPPED / folder / "workflow.py", f"shipped_desc_{folder}")
    (cls,) = defns_in(module)
    d = C.workflow_descriptor(cls)
    assert d.get("description", "unset") != ""


def test_the_body_names_only_fields_the_workflow_descriptor_defines(posted):
    """Every key sent is a field of `WorkflowDescriptor` in catalog.proto.

    The body is hand-written JSON (ADR 0002 — the wire is JSON, proto is the shared type
    definition), so nothing makes it agree with the contract except this. The last drift is the
    argument for it: `description` and `source` were added to the actor descriptor, to the route
    and to the table, and reached catalog.proto months later.
    """
    from kontra.v1 import catalog_pb2

    defined = {f.json_name for f in catalog_pb2.WorkflowDescriptor.DESCRIPTOR.fields}
    module = load_module(SHIPPED / "nscheck" / "workflow.py", "shipped_wire_nscheck")
    C.publish_workflow_catalog(defns_in(module))

    (url, body), = posted
    assert url.endswith("/api/workflows/catalog")
    assert set(body) <= defined, f"not in catalog.proto: {sorted(set(body) - defined)}"


# --- the derivation ----------------------------------------------------------------------------
#
# DECLARED AT MODULE LEVEL, not inside the tests that use them: `@workflow.run` refuses a local
# class outright ("Local classes unsupported"), because Temporal has to be able to reach the class
# by name. A workflow is a module-level class in the real world for the same reason.


@dataclass
class Req:
    dataset: str
    machines: int = 4


@dataclass
class Report:
    checked: int


@workflow.defn
class Sweep:
    """Check every domain's delegation.

    The second paragraph is for whoever maintains this, not for a caller.
    """

    @workflow.run
    async def run(self, req: Req) -> Report: ...


@workflow.defn
class Wide:
    """Resolve each domain's NS set,
    including the ones that delegate to nothing.
    """

    @workflow.run
    async def run(self) -> None: ...


@workflow.defn
class Bare:
    @workflow.run
    async def run(self, req: dict) -> dict: ...


@workflow.defn
class Untyped:
    @workflow.run
    async def run(self, req): ...


@workflow.defn(name="Sweep2")
class DnsSweepImpl:
    @workflow.run
    async def run(self, req: dict) -> dict: ...


class Helper:
    """Not decorated."""


def test_the_schemas_come_from_the_run_methods_annotations():
    d = C.workflow_descriptor(Sweep)
    assert sorted(d["input"]["properties"]) == ["dataset", "machines"]
    assert d["input"]["required"] == ["dataset"]
    assert sorted(d["output"]["properties"]) == ["checked"]


def test_the_description_is_the_class_docstrings_first_paragraph():
    """The first paragraph is what the workflow is FOR; the rest is for whoever maintains it. The
    same rule a Method's description already follows (`MethodRegistration.description`)."""
    assert C.workflow_descriptor(Sweep)["description"] == "Check every domain's delegation."


def test_a_docstring_indented_to_its_class_loses_that_indentation():
    """Every line but the first of a docstring carries the indentation of the `class` it sits
    under. Sent verbatim, those spaces land in a surface that renders it as one line."""
    assert C.workflow_descriptor(Wide)["description"] == (
        "Resolve each domain's NS set,\nincluding the ones that delegate to nothing."
    )


def test_an_undocumented_workflow_omits_the_key_rather_than_sending_empty():
    assert "description" not in C.workflow_descriptor(Bare)


def test_an_unannotated_run_method_registers_with_no_schemas():
    """A workflow that declares no types still registers. Refusing it would leave a workflow that
    serves perfectly and appears nowhere — the same bug the actor catalog had, wearing a hat."""
    assert C.workflow_descriptor(Untyped) == {"name": "Untyped"}


def test_an_unannotated_slot_and_one_annotated_none_are_different_answers():
    """`-> None` is a workflow that returns null; no annotation is a workflow that never said.
    Collapsing them advertises "returns nothing" about a workflow whose author simply did not
    write a type."""
    assert C.workflow_descriptor(Wide)["output"] == {"type": "null"}
    assert "input" not in C.workflow_descriptor(Wide)


def test_the_name_is_the_registered_type_not_the_class():
    """`@workflow.defn(name=…)` is what Temporal routes on. A descriptor keyed off the class would
    name a type nothing registered, and a start against it sits on the queue forever."""
    assert C.workflow_descriptor(DnsSweepImpl)["name"] == "Sweep2"


def test_a_class_that_is_not_a_workflow_describes_nothing():
    """None rather than a raise: `serve()` takes a list an author wrote, and a stray helper class
    in it must be reported and skipped, not take the whole worker's registration down."""
    assert C.workflow_descriptor(Helper) is None


# --- several workflows in one file -------------------------------------------------------------


TWO_IN_A_FILE = '''
"""A module docstring, which is NOT either class's description."""
from temporalio import workflow


@workflow.defn
class First:
    """The first one."""

    @workflow.run
    async def run(self, req: dict) -> dict: ...


@workflow.defn
class Second:
    """The second one."""

    @workflow.run
    async def run(self, req: dict) -> dict: ...
'''


def test_every_defn_in_a_file_registers(tmp_path, posted):
    """One POST per class. `serve([First, Second])` is an ordinary spelling and Temporal routes on
    the type, so a file that registered only its first class would leave the second startable and
    undescribed."""
    path = tmp_path / "workflow.py"
    path.write_text(TWO_IN_A_FILE)
    module = load_module(path, "two_in_a_file")

    C.publish_workflow_catalog(defns_in(module))

    assert [body["name"] for _, body in posted] == ["First", "Second"]
    assert [body["description"] for _, body in posted] == ["The first one.", "The second one."]


def test_a_module_docstring_is_not_a_workflows_description(tmp_path, posted):
    """The description is the CLASS's. A file's docstring describes the file — and with two
    workflows in it, using the module's would give both the same words."""
    path = tmp_path / "workflow.py"
    path.write_text(TWO_IN_A_FILE)
    module = load_module(path, "two_in_a_file_module_doc")

    C.publish_workflow_catalog(defns_in(module))

    for _, body in posted:
        assert "module docstring" not in body["description"]


# --- best effort -------------------------------------------------------------------------------


def test_a_registration_that_fails_is_printed_and_swallowed(monkeypatch, capsys):
    """A worker that refused to start because a catalog was unreachable would make the catalog a
    dependency of running code, which it is not. Printed, because a swallowed exception with no
    line is how the actor side stayed invisible for months."""
    monkeypatch.setenv("KONTRA_ORCHESTRATOR_URL", "http://orchestrator-api:8088")

    def boom(url, descriptor, *, timeout=5.0):
        raise OSError("connection refused")

    monkeypatch.setattr(C, "register_workflow_catalog", boom)
    module = load_module(SHIPPED / "ping" / "workflow.py", "shipped_besteffort_ping")

    C.publish_workflow_catalog(defns_in(module))  # must not raise

    assert "registration failed" in capsys.readouterr().out


def test_one_workflows_failure_does_not_cost_the_others_their_registration(tmp_path, posted, monkeypatch):
    """Each class gets its own try. A file declaring several is one import, and a single bad
    annotation would otherwise take every workflow in it out of the catalog."""
    path = tmp_path / "workflow.py"
    path.write_text(TWO_IN_A_FILE)
    module = load_module(path, "two_in_a_file_one_bad")
    first, second = defns_in(module)

    real = C.workflow_descriptor

    # `**kw` FORWARDED, NOT SWALLOWED. This double stands in for the real descriptor, so it has to
    # accept what the real one accepts — `queue=` among it. A double with a narrower signature
    # raises TypeError inside the per-class try, which this function catches and prints, so BOTH
    # registrations vanish and the test reports the exact failure it exists to rule out.
    def only_second_works(cls, **kw):
        if cls is first:
            raise TypeError("pydantic cannot describe this")
        return real(cls, **kw)

    monkeypatch.setattr(C, "workflow_descriptor", only_second_works)
    C.publish_workflow_catalog([first, second])

    assert [body["name"] for _, body in posted] == ["Second"]


def test_no_orchestrator_url_still_means_no_call(monkeypatch):
    """The same gate the actor path has: unset means this installation has no catalog to talk to,
    which is a legitimate way to run a worker."""
    called = []
    monkeypatch.setattr(C, "register_workflow_catalog", lambda *a, **k: called.append(1))
    monkeypatch.delenv("KONTRA_ORCHESTRATOR_URL", raising=False)
    module = load_module(SHIPPED / "ping" / "workflow.py", "shipped_nourl_ping")

    C.publish_workflow_catalog(defns_in(module))

    assert called == []


# --- the hook ----------------------------------------------------------------------------------


def test_serving_registers_before_the_worker_runs(monkeypatch):
    """The wire that makes all of the above reach anything: `serve_workflows_async` publishes.

    Asserted through the REAL function, with Temporal replaced at its two seams, because the
    derivation being right is worth nothing if nobody calls it — which is exactly how the actor
    catalog's own client sat written, documented and tested with no caller in the running system.
    """
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
            self.kwargs = kw

        async def run(self):
            return None

    monkeypatch.setattr(temporalio.client.Client, "connect", staticmethod(fake_connect))
    monkeypatch.setattr(temporalio.worker, "Worker", FakeWorker)

    # Patched on `internals.catalog`, not on `wfhost`: the host imports the function inside the
    # coroutine, so the binding it uses is resolved at call time from this module.
    # The queue is captured, not ignored: it is the reason this call has a keyword at all, and a
    # double that dropped it would let the host stop passing it without a test noticing.
    published = []
    monkeypatch.setattr(
        C, "publish_workflow_catalog", lambda ws, **k: published.append((list(ws), k.get("queue")))
    )

    module = load_module(SHIPPED / "ping" / "workflow.py", "shipped_hook_ping")
    classes = defns_in(module)
    asyncio.run(wfhost.serve_workflows_async(classes, task_queue="scratch"))

    # The queue the worker is about to poll rides with the registration. Without it a caller has
    # the type and not the address — a run that starts, routes to a queue nobody serves, and
    # reports `running` forever.
    assert published == [(classes, "scratch")]
