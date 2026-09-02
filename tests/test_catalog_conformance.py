"""The PYTHON arm of the cross-SDK catalog contract (shared/conformance/catalog.json).

Three hand-written emitters produce the descriptor `POST /api/actors` accepts — this SDK's
`internals/catalog.py`, the Go SDK's `internal/registrar`, and whatever the design tool uploads —
and they share no code (ADR 0002: the wire is JSON, the proto is only the shared type definition).
Nothing but a fixture makes them agree, and they had already stopped agreeing in both directions:
`source` never reached the Go emitter, and `digest` never reached this one, so a Python actor
registered an unpinned descriptor through every path there is while the field, the column and the
endpoint all existed.

The peers assert the SAME file: runtime/go/registrar/conformance_test.go and
backend/src/catalog.conformance.test.ts.

It lives in `tests/` rather than beside the emitter because `testpaths = ["tests"]` — a
conformance test outside that tree is collected by nobody, which is the state
runtime/python/internals/test_blobkey_conformance.py is in today.
"""

from __future__ import annotations

import json
import pathlib
from dataclasses import dataclass
from pathlib import Path

import internals.catalog as C
from actorkit import ActorRegistry

# parents[1] is the repo root: tests/ -> <root>.
FIXTURE = pathlib.Path(__file__).resolve().parents[1] / "shared" / "conformance" / "catalog.json"
FX = json.loads(FIXTURE.read_text())
EXPECT = FX["expect"]


# The fixture's Actor, in Python types. The field names and JSON types come FROM the fixture — its
# `expect.operations[].input.properties` is what these must reflect to — so renaming `host` here,
# or typing it `int`, fails this test rather than drifting.
@dataclass
class Target:
    host: str


@dataclass
class Page:
    url: str
    status: int


@dataclass
class Title:
    text: str


@dataclass
class Params:
    depth: int


def fixture_registry() -> ActorRegistry:
    """The fixture's Actor: three Methods in declaration order, two with DIFFERENT signatures
    (title takes what fetch emits — the §9 property a single Actor-level pair could not express),
    one described, one declaring nothing at all, and Actor-level params."""
    reg = ActorRegistry()
    reg.actor_name, reg.version = EXPECT["name"], EXPECT["version"]
    reg.actor_dir = Path(EXPECT["source"])
    reg.params_type = Params

    @reg.method(takes=Target, emits=Page)
    async def fetch(self, batch):
        """Fetch each host once."""

    @reg.method(takes=Page, emits=Title)
    async def title(self, batch): ...

    @reg.method
    async def probe(self, batch): ...

    return reg


class _Accepted:
    """The 200 the orchestrator would have returned, with urlopen's context-manager shape."""

    status = 200

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False


def posted(monkeypatch, reg, *, digest: str) -> dict:
    """Run the REAL emitter with the socket removed and hand back the JSON it would have sent.

    Not `operations_of` and not a hand-built body: the descriptor is assembled inside
    `register_actor_catalog`, which is where a key can go missing, so the check has to start at
    the POST or it checks a shape nothing sends.
    """
    sent = {}

    def fake_urlopen(req, timeout=None):
        sent["body"] = json.loads(req.data)
        return _Accepted()

    monkeypatch.setattr(C.urllib.request, "urlopen", fake_urlopen)
    monkeypatch.setenv("KONTRA_ORCHESTRATOR_URL", "http://orchestrator-api:8088")
    monkeypatch.setenv("KONTRA_ACTOR_DIGEST", digest)
    C.publish_catalog(reg)
    assert "body" in sent, "the emitter never POSTed — publish_catalog swallows and prints"
    return sent["body"]


def project_schema(value):
    """Strip the branding the schema library adds and keep the part the catalog reads.

    Pydantic stamps `title` on the document and on every property; invopop's Go reflector stamps
    `$schema`, `$id` and `additionalProperties`. Neither is a contract — nobody reads them and
    they can never agree — so the fixture holds the dialect-neutral core (type / properties /
    required) and each SDK projects its emission down to it. `required` is sorted because JSON
    Schema's `required` is a SET: its order is whichever order that language's author declared the
    fields in, and pinning it would fail on a reorder that changed nothing.
    """
    if not isinstance(value, dict):
        return value
    out = {}
    if "type" in value:
        out["type"] = value["type"]
    if "properties" in value:
        out["properties"] = {k: project_schema(v) for k, v in value["properties"].items()}
    if "required" in value:
        out["required"] = sorted(value["required"])
    return out


def project_descriptor(body: dict) -> dict:
    """Project the three schema-carrying keys of every operation. Everything else — the identity
    keys, the operation names and order, the descriptions, and WHICH keys are present at all — is
    compared verbatim."""
    out = dict(body)
    out["operations"] = [
        {k: (project_schema(v) if k in ("params", "input", "output") else v) for k, v in op.items()}
        for op in body["operations"]
    ]
    return out


def test_the_posted_descriptor_is_the_golden_one(monkeypatch):
    body = posted(monkeypatch, fixture_registry(), digest=EXPECT["digest"])
    assert project_descriptor(body) == EXPECT


def test_an_unknown_digest_is_absent_from_the_descriptor(monkeypatch):
    """The two spellings are not equivalent to the reader.

    The catalog keeps a previously registered digest only when the key is ABSENT
    (`body.digest ?? prev.digest`), so a worker that posted `"digest": ""` would not be saying
    "I don't know", it would be unpinning the image the design tool had pinned — on every restart.
    """
    absent = FX["unsetDigest"]["absent"]
    assert absent, "fixture declares no keys to be absent — nothing is being checked"
    body = posted(monkeypatch, fixture_registry(), digest="")
    for key in absent:
        assert key not in body, f'{key} is on the wire with no value: {FX["unsetDigest"]["why"]}'


# --- the fixture against the contract it claims to be an instance of ------------------------


def test_the_fixture_covers_the_descriptor_field_for_field():
    """EQUALITY, not a subset, in both directions.

    A missing key means the golden descriptor does not exercise a field the contract defines —
    which is exactly how `source` sat in the proto, the table and one SDK while the other never
    emitted it. An extra key means the fixture pins something no descriptor can carry, and the
    three emitters would be held to a shape the store has nowhere to put.

    Compared by `json_name`: this wire spells the fourth field `schemaVersion`, which is what
    proto3's JSON mapping makes of `schema_version`.
    """
    from kontra.v1 import catalog_pb2

    defined = {f.json_name for f in catalog_pb2.ActorDescriptor.DESCRIPTOR.fields}
    assert set(EXPECT) == defined


def test_the_fixtures_operations_cover_the_operation_field_for_field():
    """The union across Methods, not one Method's keys — no single operation carries them all.

    `probe` declares nothing and so omits `input`/`output`; `title` is undescribed and so omits
    `description`. Checking the first operation alone would pass while the fixture never
    exercised the unset case it exists to pin.
    """
    from kontra.v1 import catalog_pb2

    defined = {f.json_name for f in catalog_pb2.ActorOperation.DESCRIPTOR.fields}
    used = set().union(*(set(op) for op in EXPECT["operations"]))
    assert used == defined
