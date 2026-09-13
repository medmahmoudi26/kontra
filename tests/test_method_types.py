"""Types are declared on the Method, and the catalog carries one operation per Method.

ADR 0023 §9 made an Actor hold many Methods, each with its own signature — `fetch` takes a
URL and emits a page, `title` takes a page and emits a title. One Actor-level input/output
pair cannot describe that, so Actor-level typing is not merely verbose, it is wrong.

The output type cannot be inferred from a return annotation: §18 made emit a call, so a
Method returns nothing. The declaration is explicit and per-Method.
"""

from dataclasses import dataclass
from pathlib import Path
from types import SimpleNamespace

import internals.catalog as C
from kontra import ActorRegistry


@dataclass
class Target:
    host: str


@dataclass
class Page:
    url: str
    status: int


def registry_with_methods():
    reg = ActorRegistry()
    reg.actor_name, reg.version, reg.actor_dir = "demo", "1.2.3", Path(".")
    reg.params_type = None

    @reg.method(takes=Target, emits=Page)
    async def fetch(self, batch): ...

    @reg.method(takes=Page, emits=Page)
    async def title(self, batch): ...

    return reg


def capture(monkeypatch):
    """Stand in for the HTTP POST and hand back what was sent."""
    seen = {}

    def fake(url, m, operations, *, timeout=5.0):
        seen.update(url=url, name=m.name, version=m.version, operations=operations)
        return 200

    monkeypatch.setattr(C, "register_actor_catalog", fake)
    monkeypatch.setenv("KONTRA_ORCHESTRATOR_URL", "http://orchestrator-api:8088")
    return seen


def test_a_method_carries_the_types_it_takes_and_emits():
    reg = registry_with_methods()
    assert reg.methods["fetch"].takes is Target
    assert reg.methods["fetch"].emits is Page


def test_the_catalog_gets_one_operation_per_method(monkeypatch):
    seen = capture(monkeypatch)
    C.publish_catalog(registry_with_methods())
    assert [o["name"] for o in seen["operations"]] == ["fetch", "title"]


def test_each_operation_carries_its_own_methods_schemas(monkeypatch):
    # The point of §9: two Methods of one Actor have DIFFERENT signatures, and a single
    # Actor-level pair would advertise one of them as both.
    seen = capture(monkeypatch)
    C.publish_catalog(registry_with_methods())
    fetch, title = seen["operations"]
    assert sorted(fetch["input"]["properties"]) == ["host"]
    assert sorted(fetch["output"]["properties"]) == ["status", "url"]
    assert sorted(title["input"]["properties"]) == ["status", "url"]


def test_an_actor_that_declares_no_types_still_registers(monkeypatch):
    """The gate that made an untyped actor invisible is gone.

    It served fine but never appeared in `kontra workers list` and could not be dispatched
    by name, because both resolve through the catalog — so an untyped actor was a worker
    nobody could reach.
    """
    seen = capture(monkeypatch)
    reg = ActorRegistry()
    reg.actor_name, reg.version, reg.actor_dir = "bare", "0.1.0", Path(".")
    reg.params_type = None

    @reg.method
    async def probe(self, batch): ...

    C.publish_catalog(reg)
    assert seen["name"] == "bare"
    assert [o["name"] for o in seen["operations"]] == ["probe"]


def test_an_untyped_method_contributes_an_operation_with_no_schemas(monkeypatch):
    seen = capture(monkeypatch)
    reg = ActorRegistry()
    reg.actor_name, reg.version, reg.actor_dir = "bare", "0.1.0", Path(".")
    reg.params_type = None

    @reg.method
    async def probe(self, batch): ...

    C.publish_catalog(reg)
    op = seen["operations"][0]
    assert op["name"] == "probe"
    assert "input" not in op and "output" not in op


def test_a_load_only_actor_registers_its_identity_with_no_operations(monkeypatch):
    seen = capture(monkeypatch)
    reg = ActorRegistry()
    reg.actor_name, reg.version, reg.actor_dir = "loader", "0.1.0", Path(".")
    reg.params_type = None
    C.publish_catalog(reg)
    assert seen["name"] == "loader" and seen["operations"] == []


def test_params_stay_actor_level(monkeypatch):
    """Run-wide config, not a per-Method signature — so it rides once, not per operation."""

    @dataclass
    class Params:
        depth: int

    seen = capture(monkeypatch)
    reg = registry_with_methods()
    reg.params_type = Params
    C.publish_catalog(reg)
    assert all(sorted(o["params"]["properties"]) == ["depth"] for o in seen["operations"])


def test_no_orchestrator_url_still_means_no_call(monkeypatch):
    called = []
    monkeypatch.setattr(C, "register_actor_catalog", lambda *a, **k: called.append(1))
    monkeypatch.delenv("KONTRA_ORCHESTRATOR_URL", raising=False)
    C.publish_catalog(registry_with_methods())
    assert called == []


# --- what a Method is FOR ------------------------------------------------------------------


def test_the_catalog_carries_the_authors_docstring():
    """A Method advertised a name and two schemas and nothing else.

    So every surface that listed one could say what it TAKES and never what it DOES, and an
    operator composing one Actor's Method into another's had nothing to compose from. The author
    had already written the answer; nothing carried it.
    """
    reg = ActorRegistry()
    reg.actor_name, reg.version, reg.actor_dir = "demo", "1.0.0", Path(".")
    reg.params_type = None

    @reg.method(takes=Target, emits=Page)
    async def fetch(self, batch):
        """Fetch each host once.

        The second paragraph is for whoever maintains this, not for a caller — see below.
        """

    ops = C.operations_of(reg)
    assert ops[0]["description"] == "Fetch each host once."


def test_only_the_first_paragraph_reaches_a_caller():
    """A docstring's later paragraphs are for whoever maintains the Method; the first is what it
    is for. The surfaces that read this have room for a line, not a page."""
    reg = ActorRegistry()
    reg.actor_name, reg.version, reg.actor_dir = "demo", "1.0.0", Path(".")
    reg.params_type = None

    @reg.method()
    async def crawl(self, batch):
        """Crawl one page.

        Implementation notes nobody dispatching this needs: the retry budget is per host and
        the browser is shared across the Batch.
        """

    assert C.operations_of(reg)[0]["description"] == "Crawl one page."


def test_a_docstring_indented_to_its_def_is_not_carried_with_its_indentation():
    """Every line but the first of a docstring carries the indentation of its `def`. Sending that
    verbatim puts leading spaces into a UI that renders it as one line."""
    reg = ActorRegistry()
    reg.actor_name, reg.version, reg.actor_dir = "demo", "1.0.0", Path(".")
    reg.params_type = None

    @reg.method()
    async def wide(self, batch):
        """Resolve each domain's NS set,
        including the ones that delegate to nothing.
        """

    got = C.operations_of(reg)[0]["description"]
    assert got == "Resolve each domain's NS set,\nincluding the ones that delegate to nothing."
    assert "        " not in got


def test_a_method_with_no_docstring_carries_NO_description():
    """Absent, never an empty string. A blank description and a missing one render differently and
    mean different things: one says the author wrote nothing, the other says they wrote nothing
    USEFUL, and only the first is actionable by the person reading it."""
    reg = ActorRegistry()
    reg.actor_name, reg.version, reg.actor_dir = "demo", "1.0.0", Path(".")
    reg.params_type = None

    @reg.method()
    async def bare(self, batch): ...

    assert "description" not in C.operations_of(reg)[0]


# ---------------------------------------------------------------------------------------------
# Congruence with catalog.proto. The peers are runtime/handler/internal/wire/wire_congruence_test.go and
# the compile-time guard orchestrator/catalog.contract.ts.
#
# This body is HAND-WRITTEN JSON (ADR 0002 — the wire is JSON, proto is only the shared type
# definition), so nothing makes it agree with the contract except a test. And the last drift is
# the argument for one: `description` and `source` were added here, to POST /api/actors and to
# the `actors` table, and neither reached catalog.proto for months — nothing outside `_gen/`
# loads an ActorDescriptor, so a contract two fields behind the thing it defines broke nothing.
# ---------------------------------------------------------------------------------------------


class _Accepted:
    """The 200 the orchestrator would have returned, with urlopen's context-manager shape."""

    status = 200

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False


def _posted_descriptor(monkeypatch, reg):
    """Run the real emitter with the socket removed, and hand back the JSON it would have sent."""
    import json

    sent = {}

    def fake_urlopen(req, timeout=None):
        sent["body"] = json.loads(req.data)
        return _Accepted()

    monkeypatch.setattr(C.urllib.request, "urlopen", fake_urlopen)
    monkeypatch.setenv("KONTRA_ORCHESTRATOR_URL", "http://orchestrator-api:8088")
    C.publish_catalog(reg)
    assert "body" in sent, "the emitter never POSTed — publish_catalog swallows and prints"
    return sent["body"]


def test_the_registration_body_names_only_fields_the_descriptor_defines(monkeypatch):
    """Every key sent is a field of ActorDescriptor, compared by proto3's JSON name.

    `json_name`, not `name`: this body spells the fourth field `schemaVersion`, which is what
    proto3's JSON mapping makes of `schema_version` — unlike EntryInput, whose keys stay
    snake_case on the wire (tests/test_workflows_client.py compares against `f.name`). Two wires,
    two conventions, and each is only pinned where it is used.

    A subset rather than an equality: `digest` rides only when the worker KNOWS one
    (KONTRA_ACTOR_DIGEST), and this registry runs with none — an unpinned actor omits the key
    rather than posting "", because the catalog keeps a previously registered digest only when the
    key is absent. The full field set, every field exercised, is pinned against the cross-SDK
    fixture in tests/test_catalog_conformance.py.
    """
    from kontra.v1 import catalog_pb2

    defined = {f.json_name for f in catalog_pb2.ActorDescriptor.DESCRIPTOR.fields}
    body = _posted_descriptor(monkeypatch, registry_with_methods())

    assert set(body) <= defined, f"not in catalog.proto: {sorted(set(body) - defined)}"
    # Named rather than left to the subset check: `source` is the one whose absence is silent.
    # A catalog without it lists an actor and offers no way to reach the code that was run.
    assert "source" in body


def test_every_operation_key_is_a_field_of_the_operation_message(monkeypatch):
    """Same check, one level down — and the union across Methods, not one Method's keys.

    A Method that declares nothing omits `input`/`output` entirely and an undescribed one omits
    `description`, so no single operation carries the whole field set. Checking only the first
    would pass while a Method that actually declared something sent a key the contract does not
    define, which the orchestrator would store and no reader would ever ask for.
    """
    from kontra.v1 import catalog_pb2

    defined = {f.json_name for f in catalog_pb2.ActorOperation.DESCRIPTOR.fields}

    reg = ActorRegistry()
    reg.actor_name, reg.version, reg.actor_dir = "demo", "1.2.3", Path(".")
    reg.params_type = Target  # actor-level config: rides every operation

    @reg.method(takes=Target, emits=Page)
    async def fetch(self, batch):
        """Fetch one page per host."""

    @reg.method()
    async def bare(self, batch): ...

    operations = _posted_descriptor(monkeypatch, reg)["operations"]
    sent = set().union(*(set(op) for op in operations))
    assert sent <= defined, f"not in catalog.proto: {sorted(sent - defined)}"
    # The union must actually exercise the fields, or the subset check above proves nothing.
    assert {"name", "params", "input", "output", "description"} <= sent
