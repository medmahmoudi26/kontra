"""The caller-side SDK: the wire shape it sends, and the identity strings it routes on.

Everything here is pure — no Temporal, no server. That is deliberate: the parts of
`kontra.catalog` that can silently do the wrong thing are the derived strings (a queue or
endpoint nobody serves, which just hangs) and the EntryInput field set (a key the handler never
reads, which runs the batch with the wrong id and looks fine). Both are testable with no
infrastructure, and both are where a drift has no loud failure mode.

The identity strings themselves are pinned by shared/conformance/queues.json, which every language
executes; what stays here is the wire shape, which only this SDK writes.
"""

import asyncio

import pytest

from kontra import catalog

# ---------------------------------------------------------------------------------------------
# Identity. The queue, session-queue and endpoint derivations that used to be pinned here by a
# hand-copied table are pinned by shared/conformance/queues.json now — every language executes it, and
# tests/test_queue_congruence.py is this package's arm. The table that stood here was the third
# copy of one contract, kept correct by somebody remembering to update all three.
#
# What stays is the one identity assertion that is NOT a shared string: an API that was retired.
# ---------------------------------------------------------------------------------------------


def test_there_is_one_deployed_kind():
    """ADR 0023 §9. The Activity kind had its own handle, its own queue suffix and its own host;
    a caller reaching for it now must get an AttributeError here rather than a queue nobody
    polls, which is what a leftover `activities()` shim would have produced."""
    for gone in ("activities", "ActivityHandle", "activities_queue"):
        assert not hasattr(catalog, gone), f"{gone} retired with ADR 0023 §9"


# ---------------------------------------------------------------------------------------------
# The wire shape
# ---------------------------------------------------------------------------------------------


def test_the_service_contract_matches_the_handler_literals():
    """The typed contract and the Go handler agree on both names, and there is no code path
    between them: the Go side generates its service from actor_service.proto, this class is
    hand-written. Two literals, four writers (Go, TS, this, and identity.go's endpoint).
    """
    from kontra.contract import SERVICE_NAME, KontraActorService

    assert SERVICE_NAME == catalog.SERVICE_NAME == "kontra.actor"
    assert KontraActorService.run.name == catalog.RUN_OPERATION == "run"


def test_the_contract_payload_types_stay_dicts_at_runtime():
    """TypedDict, not dataclass, and that is a wire decision rather than a style one: a
    dataclass would serialize `params: null` where interpreter.ts omits the key, and two
    callers must not put different bytes on one contract."""
    from kontra.contract import BareRef, EntryInput

    entry = catalog.entry_input([1], run_id="r", node_id="n")
    assert isinstance(entry, dict)
    assert EntryInput.__annotations__.keys() >= set(entry)
    assert set(BareRef.__annotations__) == {"sha256", "size", "meta"}


def test_the_contract_type_is_exactly_the_proto_field_set():
    """The typed contract is hand-written (TypedDict, for the runtime-dict reason above), so it
    is held to the proto the same way the Go wire struct is by wire_congruence_test.go — and
    EXACTLY, not as a subset, in both directions. A proto field the TypedDict lacks is a field
    no caller can name without a type error; a TypedDict field the proto lacks is a key the
    handler never reads, which runs the batch with a zero-valued id and looks fine.
    """
    from kontra.contract import EntryInput
    from kontra.v1 import entry_pb2

    fields = {f.name for f in entry_pb2.EntryInput.DESCRIPTOR.fields}
    assert set(EntryInput.__annotations__) == fields


def test_entry_input_is_a_subset_of_the_proto_contract():
    """The proto is the type-of-record (ADR 0002); the wire stays JSON. A key we send that the
    contract does not define is a key the handler never reads — and since EntryInput's fields are
    all optional on the wire, the run would proceed with a zero-valued id rather than fail."""
    from kontra.v1 import entry_pb2

    fields = {f.name for f in entry_pb2.EntryInput.DESCRIPTOR.fields}
    sent = set(catalog.entry_input([1], run_id="r", node_id="n", params={"k": 1}))
    assert sent <= fields, f"not in entry.proto: {sorted(sent - fields)}"
    # Named rather than left to the subset check: the v2 fields are the two whose absence is
    # silent. A dropped `method` runs the sole Method (or blames the author for declaring
    # several); a dropped `session_id` unbinds the call from its scope.
    assert {"method", "session_id"} <= sent


def test_the_dispatch_names_the_method_it_means():
    """An Actor exposes as many Methods as it has jobs (ADR 0023 §5) and the caller names the
    one it means. The engine resolves the name per batch, so without a field on the wire the
    callee's registry has nothing to resolve and a multi-Method actor is unaddressable."""
    entry = catalog.entry_input([1], run_id="r", node_id="n", method="crawl")
    assert entry["method"] == "crawl"


def test_the_dispatch_says_which_session_it_belongs_to():
    """A Session is opened by the caller and spans however many Method calls it makes inside it
    (ADR 0023 §4, §6). The id is what binds those calls to the one activated actor — so it
    travels with every dispatch, not only with the one that opened the scope."""
    entry = catalog.entry_input([1], run_id="r", node_id="n", session_id="s-7f3a")
    assert entry["session_id"] == "s-7f3a"


def test_a_dispatch_forwards_the_method_the_caller_named():
    """`entry_input` is only half of it — the caller reaches the wire through `dispatch_ref`, and
    a kwarg it quietly drops is the field's whole failure mode: a multi-Method actor resolves
    nothing and blames its author. Read from the source because `dispatch_ref` needs a workflow
    context to run and this file is deliberately infrastructure-free."""
    import inspect

    fn = catalog.ActorHandle.dispatch_ref
    assert "method" in inspect.signature(fn).parameters
    assert "method=method" in inspect.getsource(fn)


def test_entry_input_always_asks_for_a_ref():
    """return_ref is not a knob. The result comes back as a claim-check ref and is dereferenced
    on demand, which is what keeps a large batch result out of the caller's workflow history."""
    entry = catalog.entry_input([], run_id="r", node_id="n")
    assert entry["return_ref"] is True


def test_entry_input_omits_unset_params():
    """The orchestrator omits unset optionals entirely (interpreter.ts). Sending `null` would
    reach Go as an explicit empty map — same value today, different JSON, and the two callers
    should not put different bytes on one wire."""
    assert "params" not in catalog.entry_input([], run_id="r", node_id="n")
    assert "params" not in catalog.entry_input([], run_id="r", node_id="n", params={})
    assert catalog.entry_input([], run_id="r", node_id="n", params={"a": 1})["params"] == {"a": 1}


def test_entry_input_carries_the_units_inline():
    entry = catalog.entry_input(iter(["a", "b"]), run_id="r", node_id="n")
    assert entry["units"] == ["a", "b"]  # an iterator is consumed, not passed through


def test_entry_input_omits_input_ref_unless_given():
    assert "input_ref" not in catalog.entry_input([], run_id="r", node_id="n")


def test_entry_input_keeps_units_present_and_empty_beside_a_ref():
    """The handler selects the ref path on `len(units) == 0 && in.InputRef != nil`. Dropping
    the key would not select it — it would send an id-less batch of nothing."""
    ref = {"sha256": "abc", "size": 9, "meta": {"kind": "units", "n": "2"}}
    entry = catalog.entry_input([], run_id="r", node_id="n", input_ref=ref)
    assert entry["input_ref"] == ref
    assert entry["units"] == []
    assert "units" in entry


# ---------------------------------------------------------------------------------------------
# Batch — the currency: a Dataset yields Batches, a Method takes one and returns one
# ---------------------------------------------------------------------------------------------


def _ref(n=2, isolated=0, done=True, failures="", machine=""):
    meta = {"kind": "units", "n": str(n), "done": "true" if done else "false"}
    if isolated:
        meta["isolated"] = str(isolated)
        meta["failures"] = failures or "f" * 8
    if machine:
        meta["machine"] = machine
    return {"sha256": "a" * 8, "size": 100, "meta": meta}


def test_a_batch_is_not_iterable():
    """THE guard that keeps this design honest. `entry_input` does `list(units)`, so an
    iterable Batch reaching dispatch would be silently flattened into inline units — a 50 MB
    payload in workflow history, failing as a size problem in production rather than a type
    error here."""
    b = catalog.Batch.from_ref(_ref())
    with pytest.raises(TypeError):
        list(b)
    with pytest.raises(TypeError):
        for _ in b:
            pass


def test_a_batch_answers_count_and_isolation_without_a_fetch():
    """The reason `n` and `isolated` ride on the ref's meta: the `dropped` half of a call (ADR
    0028 §4) must cost nothing on a clean batch, and `len()` must not mean "download it"."""
    clean = catalog.Batch.from_ref(_ref(n=5))
    assert len(clean) == 5 and clean.isolated == 0 and clean.done is True
    assert bool(clean) is True

    dropped = catalog.Batch.from_ref(_ref(n=0, isolated=3, done=False))
    assert len(dropped) == 0 and dropped.isolated == 3 and dropped.done is False
    assert bool(dropped) is True, "everything dropped is not the same as nothing to do"


def test_a_batch_carries_its_producers_identity_so_a_fetch_knows_the_queue():
    b = catalog.Batch.from_ref(_ref(), actor="crawl4ai", version="0.5.0")
    assert (b.actor, b.version) == ("crawl4ai", "0.5.0")


def test_a_batch_tolerates_a_meta_less_ref():
    """A ref minted before the split carries no counts. It must degrade to zero, not explode."""
    b = catalog.Batch.from_ref({"sha256": "x", "size": 1})
    assert len(b) == 0 and b.isolated == 0 and b.done is True


def test_a_batch_names_the_machine_that_ran_the_method_without_a_fetch():
    """The provenance a published row ultimately carries. It rides the ref's meta for the same
    reason `n` and `isolated` do — reading it must not mean dereferencing the Batch."""
    b = catalog.Batch.from_ref(_ref(machine="kf-dns-01"))
    assert b.machine == "kf-dns-01"


def test_an_unnamed_machine_stays_unrecorded_rather_than_borrowing_a_value():
    """Two refs name no Machine and neither may pretend otherwise: a Dataset page (the lake
    produced it, no Method ran) and a ref from an actor host older than the meta contract.
    Empty is how the Batch says "nothing wrote this", and `publish` passes that through so the
    lake stores NULL instead of a plausible-looking default."""
    assert catalog.Batch.from_ref(_ref()).machine == ""
    assert catalog.Batch.from_ref({"sha256": "x", "size": 1}).machine == ""


def test_the_machine_the_handler_stamps_is_the_key_the_batch_reads():
    """A CONTRACT PAIR WITH NO SHARED CODE. The Go handler writes this key into the ref's meta
    and this Python class reads it; the Go actor host and the Python actor host both put it in
    the envelope the handler copies from. Nothing fails loudly on a drift — the Batch would just
    report unrecorded forever, which reads exactly like a fleet that never ran — so the key is
    pinned on every side that names it."""
    from pathlib import Path

    root = Path(__file__).resolve().parent.parent

    def src(*parts: str) -> str:
        return (root.joinpath(*parts)).read_text()

    # The Python actor host puts it in the envelope; the Go actor host puts the same key there
    # through RunBatchResp's json tag.
    assert 'out["machine"]' in src("runtime", "python", "internals", "temporal", "host.py")
    assert '`json:"machine,omitempty"`' in src("runtime", "go", "engine", "engine.go")
    # The handler copies it from the envelope onto the ref's meta, under the SAME key…
    assert 'result["machine"]' in src("runtime", "handler", "workflow.go")
    assert 'ref.Meta["machine"]' in src("runtime", "handler", "workflow.go")
    # …which is the key both callers' Batch reads back.
    assert 'meta.get("machine")' in src("sdk", "python", "kontra", "catalog.py")
    assert 'ref.Meta["machine"]' in src("sdk", "go", "catalog", "workflows.go")


def test_dispatch_routes_a_batch_to_input_ref_instead_of_inlining_it():
    """The seam where one Method's output becomes the next one's input. Read from the source
    because dispatch_ref needs a workflow context and this file is infrastructure-free."""
    import inspect

    src = inspect.getsource(catalog.ActorHandle.dispatch_ref)
    assert "isinstance(units, Batch)" in src
    assert "input_ref=ref_in" in src


# ---------------------------------------------------------------------------------------------
# Datasets — the caller's paging loop
# ---------------------------------------------------------------------------------------------


def _drain(gen):
    """Run an async generator to exhaustion without a workflow, collecting what it yields."""
    import asyncio

    async def go():
        out = []
        async for x in gen:
            out.append(x)
        return out

    return asyncio.run(go())


def test_a_dataset_is_addressed_by_call_or_by_subscript_and_they_are_one_call():
    """`catalog.dataset["test"]` is `catalog.dataset("test")`, spelled as a lookup — the same shape
    `crawler["acme.com"]` already has, because a Dataset name is an address in the same sense.

    ONE IMPLEMENTATION, THE OTHER DELEGATING: `__getitem__` IS the call, so the two spellings
    cannot drift into two behaviours. The call form is not replaced — it is the only one that can
    express a partition scope or an authored tag."""
    called = catalog.dataset("lame")
    subscripted = catalog.dataset["lame"]
    assert type(called) is type(subscripted)
    assert called.__dict__ == subscripted.__dict__

    # The call form keeps every keyword; the subscript names the Dataset and nothing else.
    scoped = catalog.dataset("crawl4ai", version="0.5.0", dt="2026-08-14", tag="nightly")
    assert (scoped.version, scoped.dt) == ("0.5.0", "2026-08-14")

    # `.temp()` still hangs off the same surface (temp-datasets PRD) — the object holds all three.
    assert isinstance(catalog.dataset.temp(), catalog.TempDataset)

    # A partition scope is not a tuple key: the subscript refuses it and names the form that works.
    with pytest.raises(TypeError, match="ONE name"):
        catalog.dataset["crawl4ai", "0.5.0"]
    with pytest.raises(TypeError, match="must be a str"):
        catalog.dataset[7]


def test_paging_refuses_a_size_above_the_measured_hard_cap():
    """The 200/1000 thresholds are measured, not guessed: a cascading unit takes a whole node
    with it and the run still reports `completed`. A page IS a batch, so it inherits them."""
    ds = catalog.dataset("subs")
    with pytest.raises(ValueError) as exc:
        _drain(ds.batches(5000, order_by="host"))
    assert "exceeds the safe maximum" in str(exc.value)

    with pytest.raises(ValueError):
        _drain(ds.batches(0, order_by="host"))


def test_paging_requires_an_order_by_at_the_signature():
    """A materialized dataset stamps no row id, so LIMIT/OFFSET has no defined order and two
    pages may overlap or skip units. Keyword-only and required, so it cannot be forgotten."""
    import inspect

    p = inspect.signature(catalog.DatasetHandle.batches).parameters
    assert p["order_by"].kind is inspect.Parameter.KEYWORD_ONLY
    assert p["order_by"].default is inspect.Parameter.empty


def test_paging_refuses_query_and_where_together():
    with pytest.raises(ValueError) as exc:
        _drain(catalog.dataset("subs").batches(10, order_by="h", query="SELECT 1", where="x"))
    assert "not both" in str(exc.value)


def test_a_dataset_handle_carries_its_partition_scope():
    ds = catalog.dataset("crawl4ai", version="0.5.0", dt="2026-08-14")
    assert (ds.name, ds.version, ds.dt) == ("crawl4ai", "0.5.0", "2026-08-14")


def test_the_pager_targets_its_own_queue():
    """A page read must not queue behind a 40-minute materialization; the constant has to match
    control/orchestrator/src/queues.ts or the activity sits until ScheduleToStart."""
    assert catalog.DATASET_QUEUE == "kontra-datasets"
    assert catalog.PAGE_DATASET_ACTIVITY == "pageDataset"


def test_a_writer_refuses_anything_that_is_not_a_batch():
    """The manifest IS the Batch's ref. Handing it a list would mean the caller had already
    materialized rows, which is the thing the write path exists to avoid."""
    import asyncio

    w = catalog.dataset("crawled").writer()
    with pytest.raises(TypeError):
        asyncio.run(w.publish([{"host": "a"}]))


def test_a_writer_skips_an_empty_batch_without_an_activity():
    import asyncio

    w = catalog.dataset("crawled").writer()
    empty = catalog.Batch.from_ref(_ref(n=0))
    # No workflow context here, so reaching execute_activity would raise; returning 0 proves
    # it short-circuited.
    assert asyncio.run(w.publish(empty)) == 0


def test_the_writer_scope_seals_and_an_explicit_seal_is_idempotent(monkeypatch):
    """ADR 0023 §11, in the ADR's own words. A clean exit SEALS; an exception marks `abandoned`;
    a CRASH reaches neither and leaves the Dataset `open`, which is what lets a reader tell a
    dead producer from an empty one.

    The vocabulary is asserted here rather than read out of the source because it is a four-way
    contract with no shared code — this writer, the Go writer, the orchestrator's activity and
    the web badge — and a drift has no loud failure mode.
    """
    import asyncio

    from temporalio import workflow as temporal_workflow

    closed: list[tuple[str, str]] = []

    async def fake_execute_activity(name, arg, **_kw):
        closed.append((name, arg.get("state")))
        return {}

    monkeypatch.setattr(temporal_workflow, "execute_activity", fake_execute_activity)

    async def clean():
        async with catalog.dataset("crawled").writer():
            pass

    asyncio.run(clean())
    assert closed == [(catalog.CLOSE_DATASET_ACTIVITY, "sealed")]

    # The explicit form, for a producer that finishes writing before its scope ends. Exactly ONE
    # close: a scope that re-sealed on exit would overwrite an abandon a caller had just made.
    closed.clear()

    async def early():
        async with catalog.dataset("crawled").writer() as out:
            await out.seal()

    asyncio.run(early())
    assert closed == [(catalog.CLOSE_DATASET_ACTIVITY, "sealed")]

    closed.clear()

    async def failed():
        with pytest.raises(RuntimeError):
            async with catalog.dataset("crawled").writer():
                raise RuntimeError("the caller caught its own failure")

    asyncio.run(failed())
    assert closed == [(catalog.CLOSE_DATASET_ACTIVITY, "abandoned")]


def _temp_seams(monkeypatch, activities: list):
    """Fake the two workflow seams a temporary Dataset touches at open: `info()` (for the owning
    Run id) and `uuid4()` (for the framework-derived name), plus `execute_activity` (captured into
    `activities`). Returns nothing — the caller reads `activities`."""
    from datetime import datetime, timezone

    from temporalio import workflow as temporal_workflow

    class _Info:
        workflow_id = "NsCheck-42"
        start_time = datetime(2026, 8, 19, tzinfo=timezone.utc)

    class _UUID:
        hex = "a7f31b2c9d5e0000"

    async def fake_execute_activity(name, arg, **_kw):
        activities.append({"activity": name, **arg})
        return {}

    monkeypatch.setattr(temporal_workflow, "info", lambda: _Info())
    monkeypatch.setattr(temporal_workflow, "uuid4", lambda: _UUID())
    monkeypatch.setattr(temporal_workflow, "execute_activity", fake_execute_activity)


def test_a_temp_dataset_is_framework_named_and_owned_by_its_run(monkeypatch):
    """Slice 01's whole shape. Opening a temp mints a `tmp_`-prefixed name FROM the Run (never a
    caller-invented one), records the owning Run's id, and does both through the one temp-specific
    activity — before any Batch lands, so an owner is answerable even for a temp that got nothing."""
    acts: list = []
    _temp_seams(monkeypatch, acts)

    async def go():
        async with catalog.dataset.temp() as tmp:
            # The name is the framework's, derived from the Run, and visibly temporary — and it
            # SAYS which Run, which `tmp_a7f31b2c` did not. The uuid suffix survives because a Run
            # may open several temps and they must not collide.
            assert tmp.name.startswith("tmp_")
            assert tmp.name == "tmp_NsCheck-42_a7f31b2c"
            # Owned by its Run — the workflow id IS the Run (ADR 0023 §12).
            assert tmp.owner == "NsCheck-42"
            # Traceable BY EYE: the owning Run reads out of the storage name itself.
            assert tmp.owner in tmp.name
            return tmp.name

    name = asyncio.run(go())
    # Ownership is recorded at open, ahead of any publish, with exactly the fields the activity reads.
    assert acts[0] == {
        "activity": catalog.OPEN_TEMP_ACTIVITY,
        "dataset": name,
        "owner": "NsCheck-42",
    }


def test_two_temps_of_one_run_share_the_run_and_differ_by_the_suffix(monkeypatch):
    """A Run may open several temps, so the name has to be unique WITHIN a Run as well as between
    Runs. The Run part is what attributes them; the replay-stable uuid suffix is what separates
    them. Losing either one is a real failure: the first orphans rows under a name nobody can
    attribute, the second collides two temps into one table."""
    acts: list = []
    _temp_seams(monkeypatch, acts)

    from temporalio import workflow as temporal_workflow

    minted = iter(["aaaaaaaa0000", "bbbbbbbb1111"])

    class _Next:
        def __init__(self) -> None:
            self.hex = next(minted)

    monkeypatch.setattr(temporal_workflow, "uuid4", lambda: _Next())

    async def go():
        async with catalog.dataset.temp() as one:
            async with catalog.dataset.temp() as two:
                return one.name, two.name

    one, two = asyncio.run(go())
    assert one == "tmp_NsCheck-42_aaaaaaaa"
    assert two == "tmp_NsCheck-42_bbbbbbbb"
    assert one != two


def test_a_temp_name_is_path_safe_whatever_the_run_id_is(monkeypatch):
    """A Run id is `<type>-<unixseconds>` only by DEFAULT — `--id` takes anything Temporal takes,
    and this name becomes a DuckLake table AND an object-store key segment. Anything outside
    `[A-Za-z0-9_.-]` (the orchestrator's own `safeName` rule) is mapped to `_` HERE, so the name a
    caller reads and the name the lake stores are one string rather than two."""
    acts: list = []
    _temp_seams(monkeypatch, acts)

    from temporalio import workflow as temporal_workflow

    class _Info:
        workflow_id = "runs/2026-08-19T14:49:20+00:00 sweep"

    monkeypatch.setattr(temporal_workflow, "info", lambda: _Info())

    async def go():
        async with catalog.dataset.temp() as tmp:
            return tmp.name, tmp.owner

    name, owner = asyncio.run(go())
    assert name == "tmp_runs_2026-08-19T14_49_20_00_00_sweep_a7f31b2c"
    # The OWNER is the id verbatim — the marker addresses the Runs surface and must not be mangled.
    assert owner == "runs/2026-08-19T14:49:20+00:00 sweep"
    # Nothing that would split a path or need quoting in SQL survives into the storage name.
    assert not any(c in name for c in "/\\:+ '\"")


def test_a_pathological_run_id_cannot_mint_an_unbounded_temp_name(monkeypatch):
    """The Run part is bounded, so no `--id` can produce an object key a store refuses. Uniqueness
    never rode on it — the uuid suffix does — so truncation costs legibility and nothing else."""
    acts: list = []
    _temp_seams(monkeypatch, acts)

    from temporalio import workflow as temporal_workflow

    class _Info:
        workflow_id = "n" * 500

    monkeypatch.setattr(temporal_workflow, "info", lambda: _Info())

    async def go():
        async with catalog.dataset.temp() as tmp:
            return tmp.name

    name = asyncio.run(go())
    assert name == "tmp_" + "n" * 64 + "_a7f31b2c"
    assert len(name) < 128


def test_a_temp_carries_the_durable_open_sealed_abandoned_lifecycle(monkeypatch):
    """The lifecycle is a durable Dataset's, unchanged (ADR 0023 §11): a clean scope exit SEALS and
    an exception on the way out marks `abandoned`. A crash reaches neither and leaves it `open` —
    which is not reachable from a test, and is the point. Inherited straight from DatasetWriter, so
    this pins that the temp did not accidentally override it."""
    acts: list = []
    _temp_seams(monkeypatch, acts)

    async def clean():
        async with catalog.dataset.temp():
            pass

    asyncio.run(clean())
    assert acts[0]["activity"] == catalog.OPEN_TEMP_ACTIVITY
    assert (acts[-1]["activity"], acts[-1]["state"]) == (catalog.CLOSE_DATASET_ACTIVITY, "sealed")

    acts.clear()

    async def failed():
        with pytest.raises(RuntimeError):
            async with catalog.dataset.temp():
                raise RuntimeError("the caller caught its own failure")

    asyncio.run(failed())
    assert (acts[-1]["activity"], acts[-1]["state"]) == (catalog.CLOSE_DATASET_ACTIVITY, "abandoned")


def test_a_method_call_cannot_tell_a_temp_from_a_durable_destination(monkeypatch):
    """ADR 0028's central invariant, from the caller's side. A temp IS a DatasetWriter, so the same
    `_publish_to` a durable destination takes accepts it and publishes into it byte-identically —
    the actor's `dataset` parameter never learns which kind it was, because only the caller ever
    holds the name."""
    acts: list = []
    _temp_seams(monkeypatch, acts)
    batch = catalog.Batch.from_ref(_ref(n=2, machine="kf-01"), actor="nscheck", version="0.1.0")

    async def go():
        async with catalog.dataset.temp() as tmp:
            assert isinstance(tmp, catalog.DatasetWriter)
            await catalog._publish_to(tmp, batch)
            return tmp.name

    name = asyncio.run(go())
    published = [a for a in acts if a["activity"] == catalog.PUBLISH_BATCH_ACTIVITY]
    assert len(published) == 1
    # The publish names the temp — caller-side — and forwards the Batch's own provenance, exactly
    # as a durable publish does.
    assert published[0]["dataset"] == name
    assert published[0]["machine"] == "kf-01"


def test_insert_from_promotes_and_never_stamps_the_promoter(monkeypatch):
    """Slice 02's whole shape from the caller's side. `insert_from` promotes rows out of a temp
    into a durable Dataset through the `promoteDataset` activity — carrying `target`, `source` and
    the SELECT — and, load-bearing, sends NO runId/machine. Promotion carries the PRODUCING Run's
    provenance; a runId here would be this workflow's, which is exactly the 1,246-rows-`w` bug."""
    acts: list = []
    _temp_seams(monkeypatch, acts)

    async def go():
        async with catalog.dataset.temp() as tmp:
            return await catalog.dataset("lame").insert_from(tmp, where="NOT ok"), tmp.name

    rows, name = asyncio.run(go())
    assert rows == 0  # the fake activity returns {}, so no rows — the wire shape is the assertion
    promote = next(a for a in acts if a["activity"] == catalog.PROMOTE_DATASET_ACTIVITY)
    assert promote["target"] == "lame"
    assert promote["source"] == name
    assert promote["sql"] == f'SELECT * FROM "{name}" WHERE NOT ok'
    # The promoter's identity must NOT ride along — provenance is the source row's.
    assert "runId" not in promote and "machine" not in promote


def test_insert_from_full_query_references_the_source_by_its_framework_name(monkeypatch):
    """`query=` is full SQL over the source, and because a temp is framework-named the caller
    reaches its name through the object once it is open — the same bare-name rule the pager's
    `--query` follows. `where` is the shorthand, and passing both raises."""
    acts: list = []
    _temp_seams(monkeypatch, acts)

    async def go():
        async with catalog.dataset.temp() as tmp:
            sql = f'SELECT domain, node, version, run_id FROM "{tmp.name}" WHERE verdict = \'lame\''
            await catalog.dataset("lame").insert_from(tmp, query=sql)
            return tmp.name

    name = asyncio.run(go())
    promote = next(a for a in acts if a["activity"] == catalog.PROMOTE_DATASET_ACTIVITY)
    assert promote["sql"] == (
        f'SELECT domain, node, version, run_id FROM "{name}" WHERE verdict = \'lame\''
    )


def test_insert_from_refuses_query_and_where_together(monkeypatch):
    acts: list = []
    _temp_seams(monkeypatch, acts)

    async def go():
        async with catalog.dataset.temp() as tmp:
            await catalog.dataset("lame").insert_from(tmp, where="NOT ok", query="SELECT 1")

    with pytest.raises(ValueError) as exc:
        asyncio.run(go())
    assert "not both" in str(exc.value)


def test_insert_from_refuses_a_non_dataset_source():
    """The near-miss is a Batch (a chain), which has no name. Type-checked so it fails at the call
    site rather than building SQL against an empty table name."""
    batch = catalog.Batch.from_ref(_ref(n=2))
    with pytest.raises(TypeError):
        asyncio.run(catalog.dataset("lame").insert_from(batch, where="x"))


def test_insert_from_refuses_an_unopened_temp():
    """A temp mints its name at `async with`; promoting one that was never opened is a caller
    ordering bug, and it says so rather than promoting from `""`."""
    from datetime import timedelta

    unopened = catalog.TempDataset(timeout=timedelta(minutes=1))
    with pytest.raises(ValueError) as exc:
        asyncio.run(catalog.dataset("lame").insert_from(unopened, where="x"))
    assert "unopened" in str(exc.value)


def _publishes(monkeypatch):
    """Run `publish` outside a cluster and return what it put on the wire.

    Both seams it touches are faked: `execute_activity` (captured) and `info()` (a workflow id
    and a start time, which is all publish reads)."""
    import asyncio
    from datetime import datetime, timezone

    from temporalio import workflow as temporal_workflow

    sent: list[dict] = []

    async def fake_execute_activity(name, arg, **_kw):
        sent.append({"activity": name, **arg})
        return {"rows": arg.get("n", 1)}

    class _Info:
        workflow_id = "NsCheck-1"
        start_time = datetime(2026, 8, 15, tzinfo=timezone.utc)

    monkeypatch.setattr(temporal_workflow, "execute_activity", fake_execute_activity)
    monkeypatch.setattr(temporal_workflow, "info", lambda: _Info())

    def run(writer, batch):
        asyncio.run(writer.publish(batch))
        return sent[-1]

    return run


def test_publish_forwards_the_batchs_machine_and_version_not_its_own_defaults(monkeypatch):
    """The whole of this slice's write half. `publish` used to send `self.version or "0"` and no
    Machine at all, so a four-Machine run landed 1,246 rows with one distinct node and one
    distinct version between them. Both now come off the Batch, which is the only thing in this
    workflow that knows who produced the rows."""
    publish = _publishes(monkeypatch)
    batch = catalog.Batch.from_ref(
        _ref(n=3, machine="kf-dns-01"), actor="nscheck", version="0.1.0")

    sent = publish(catalog.dataset("lame").writer(), batch)
    assert sent["machine"] == "kf-dns-01"
    assert sent["version"] == "0.1.0", "the Actor's real version, not the placeholder '0'"


def test_publish_forwards_an_unrecorded_machine_as_unrecorded(monkeypatch):
    """A Batch paged straight out of a Dataset was produced by the lake, not by a Machine. It
    must arrive at the activity as EMPTY — the activity is what turns that into SQL NULL — and
    never as a stand-in that a reader would take for a real Machine."""
    publish = _publishes(monkeypatch)
    sent = publish(catalog.dataset("lame").writer(), catalog.Batch.from_ref(_ref(n=1)))
    assert sent["machine"] == ""
    assert sent["version"] == ""


def test_an_explicitly_versioned_writer_still_wins(monkeypatch):
    """`dataset(name, version=…)` is a caller asking for that partition on purpose, which is a
    different thing from the empty default the Batch now fills. Deleting the default must not
    delete the choice."""
    publish = _publishes(monkeypatch)
    batch = catalog.Batch.from_ref(_ref(n=1, machine="kf-dns-01"), version="0.1.0")
    sent = publish(catalog.dataset("lame", version="pinned").writer(), batch)
    assert sent["version"] == "pinned"
    assert sent["machine"] == "kf-dns-01", "the Machine is measured, never a caller's label"


def test_repaging_a_batch_keeps_the_machine_that_produced_its_rows(monkeypatch):
    """`split` refs are minted by the orchestrator and name no Machine, but the ROWS inside them
    are still the ones one Machine emitted. Losing it here would silence provenance for exactly
    the 1→N Methods that make re-paging necessary — which is what `nscheck` does between
    `delegation` and `ask`."""
    from temporalio import workflow as temporal_workflow

    async def fake_execute_activity(name, arg, **_kw):
        return {"refs": [_ref(n=1), _ref(n=1)]}

    monkeypatch.setattr(temporal_workflow, "execute_activity", fake_execute_activity)
    big = catalog.Batch.from_ref(
        _ref(n=2, machine="kf-dns-01"), actor="nscheck", version="0.1.0")
    chunks = _drain(big.batches(1))
    assert [c.machine for c in chunks] == ["kf-dns-01", "kf-dns-01"]
    assert [c.actor for c in chunks] == ["nscheck", "nscheck"]


def test_the_write_side_targets_the_same_queue_as_the_read_side():
    assert catalog.PUBLISH_BATCH_ACTIVITY == "publishBatch"
    assert catalog.CLOSE_DATASET_ACTIVITY == "closeDataset"
    assert catalog.DATASET_STATE_ACTIVITY == "datasetState"
    assert catalog.SPLIT_BATCH_ACTIVITY == "splitBatch"
    assert catalog.TAG_DATASET_ACTIVITY == "tagDataset"


# ---------------------------------------------------------------------------------------------
# In-workflow tag at publish time (ADR 0029 §4, issue 03): the RECORD is written first, then
# MIRRORED to KontraTag. The record is the authority; the mirror is a projection over the live
# window and its failure must not lose the record.
# ---------------------------------------------------------------------------------------------


def _tags(monkeypatch, *, upsert_raises: bool = False):
    """Run a tagged publish outside a cluster and return (events, sent) where `events` is the
    ORDERED log of everything the write touched — each `("activity", name, arg)` and each
    `("upsert", attrs)` — so a test can assert the record was written BEFORE the search attribute.

    Faked seams: `execute_activity` (captured, returns a plausible result per activity), `info` (a
    workflow id and start time — all a tag reads), and `upsert_search_attributes` (captured, or made
    to raise when `upsert_raises`, standing in for a mirror that could not land)."""
    import asyncio
    import logging
    from datetime import datetime, timezone

    from temporalio import workflow as temporal_workflow

    events: list = []

    async def fake_execute_activity(name, arg, **_kw):
        events.append(("activity", name, arg))
        if name == catalog.TAG_DATASET_ACTIVITY:
            return {"tags": [arg["tag"]]}
        return {"rows": 1}

    def fake_upsert(attrs):
        events.append(("upsert", attrs))
        if upsert_raises:
            raise RuntimeError("visibility store unreachable")

    class _Info:
        workflow_id = "NsCheck-42"
        start_time = datetime(2026, 8, 19, tzinfo=timezone.utc)

    class _UUID:
        hex = "a7f31b2c9d5e0000"

    monkeypatch.setattr(temporal_workflow, "execute_activity", fake_execute_activity)
    monkeypatch.setattr(temporal_workflow, "info", lambda: _Info())
    monkeypatch.setattr(temporal_workflow, "uuid4", lambda: _UUID())
    monkeypatch.setattr(temporal_workflow, "upsert_search_attributes", fake_upsert)
    # The best-effort mirror logs its swallow via `workflow.logger`, which needs the workflow event
    # loop this pure test does not run in — the same reason `info`/`execute_activity` are faked.
    monkeypatch.setattr(temporal_workflow, "logger", logging.getLogger("kontra-test"))
    return events, asyncio


def _ref_batch(n=3, machine="kf-01"):
    return catalog.Batch.from_ref(_ref(n=n, machine=machine), actor="nscheck", version="0.1.0")


def test_a_tagged_publish_writes_the_record_before_it_mirrors_to_kontratag(monkeypatch):
    """AC 1 and 2. `dataset(name, tag=…)` is authored policy: the first publish writes the durable
    Dataset record (keyed by this Run), THEN mirrors the tag to KontraTag. The order is the whole
    issue — reversed, a mirror that failed would leave the record, which the sweeper reads, saying
    untagged. So the record activity must appear before the upsert in the ordered log."""
    events, aio = _tags(monkeypatch)

    aio.run(catalog.dataset("lame", tag="prod-sweep").writer().publish(_ref_batch()))

    kinds = [e[0] for e in events]
    tag_at = kinds.index("activity")  # the tagDataset activity is the FIRST thing a tagged publish does
    assert events[tag_at] == (
        "activity",
        catalog.TAG_DATASET_ACTIVITY,
        {"runId": "NsCheck-42", "tag": "prod-sweep"},
    ), "the record is written keyed by the Run, with the author's tag"
    # The mirror is AFTER the record — record first, Temporal second (ADR 0029 §4).
    upsert_at = kinds.index("upsert")
    assert tag_at < upsert_at, "the record must be written before the KontraTag mirror"
    assert events[upsert_at] == ("upsert", {catalog.KONTRA_TAG_ATTRIBUTE: ["prod-sweep"]})


def test_the_record_is_written_even_when_the_kontratag_mirror_fails(monkeypatch):
    """AC 3. The mirror is best-effort — a projection over the live window (ADR 0029 §4). When the
    visibility store is unreachable the publish must STILL succeed with the record written, because
    the record is the authority and the sweeper reads it, not KontraTag."""
    events, aio = _tags(monkeypatch, upsert_raises=True)

    # No raise: the failing mirror is swallowed, and the tagged publish returns its rows.
    added = aio.run(catalog.dataset("lame", tag="prod-sweep").writer().publish(_ref_batch()))
    assert added == 1

    # The record write happened (before the doomed mirror), so the tag is not lost.
    tag_calls = [e for e in events if e[0] == "activity" and e[1] == catalog.TAG_DATASET_ACTIVITY]
    assert tag_calls == [("activity", catalog.TAG_DATASET_ACTIVITY, {"runId": "NsCheck-42", "tag": "prod-sweep"})]
    # The mirror was attempted and raised — captured, then swallowed.
    assert ("upsert", {catalog.KONTRA_TAG_ATTRIBUTE: ["prod-sweep"]}) in events


def test_an_untagged_publish_touches_neither_the_record_nor_the_search_attribute(monkeypatch):
    """Only DEVIATION is stored (ADR 0029 §1): a publish with no tag must not manufacture a record
    row or a KontraTag value. The publish still lands its rows."""
    events, aio = _tags(monkeypatch)

    aio.run(catalog.dataset("lame").writer().publish(_ref_batch()))

    assert not any(e[0] == "activity" and e[1] == catalog.TAG_DATASET_ACTIVITY for e in events)
    assert not any(e[0] == "upsert" for e in events)
    # The publish itself ran.
    assert any(e[0] == "activity" and e[1] == catalog.PUBLISH_BATCH_ACTIVITY for e in events)


def test_the_tag_is_applied_once_across_a_streaming_publish(monkeypatch):
    """A destination named once and published into per chunk (the ADR 0028 §2 auto-publish loop)
    pays for the record write and the mirror ONCE — the `_tagged` guard, so a long streaming run is
    not N identical record writes and N upserts."""
    events, aio = _tags(monkeypatch)

    async def go():
        out = catalog.dataset("lame", tag="prod-sweep").writer()
        await out.publish(_ref_batch())
        await out.publish(_ref_batch())
        await out.publish(_ref_batch())

    aio.run(go())

    tag_calls = [e for e in events if e[0] == "activity" and e[1] == catalog.TAG_DATASET_ACTIVITY]
    upserts = [e for e in events if e[0] == "upsert"]
    assert len(tag_calls) == 1, "the record is written once, not per chunk"
    assert len(upserts) == 1, "the mirror is issued once, not per chunk"
    publishes = [e for e in events if e[0] == "activity" and e[1] == catalog.PUBLISH_BATCH_ACTIVITY]
    assert len(publishes) == 3, "every chunk still publishes its rows"


def test_a_bare_handle_destination_tags_through_the_method_call_auto_publish(monkeypatch):
    """The tag rides the destination, so `_publish_to` (what a Method call reaches when the caller
    names an output) tags the Run without a caller-written publish line."""
    events, aio = _tags(monkeypatch)

    aio.run(catalog._publish_to(catalog.dataset("lame", tag="prod-sweep"), _ref_batch()))

    assert any(
        e == ("activity", catalog.TAG_DATASET_ACTIVITY, {"runId": "NsCheck-42", "tag": "prod-sweep"})
        for e in events
    )


def test_a_temp_dataset_is_untagged_by_construction(monkeypatch):
    """A temporary Dataset is untagged by construction (ADR 0029 renumber note): it has an explicit
    owner and deletion verb, so it never carries authored keep-policy. Publishing into a temp must
    write no tag record."""
    events, aio = _tags(monkeypatch)

    async def go():
        # A temp minted directly (its __aenter__ records ownership via the openTemp activity, faked
        # here); publishing into it must not tag.
        tmp = catalog.TempDataset(timeout=__import__("datetime").timedelta(minutes=1))
        async with tmp:
            await tmp.publish(_ref_batch())

    aio.run(go())
    assert not any(e[0] == "activity" and e[1] == catalog.TAG_DATASET_ACTIVITY for e in events)


def test_a_batch_repages_itself_the_way_a_dataset_does():
    """The answer to "how big is what the last Method emitted?" — a caller sizes its input
    pages, but a 1:N Method's fan-out is the author's business."""
    import inspect

    assert inspect.isasyncgenfunction(catalog.Batch.batches)
    src = inspect.getsource(catalog.Batch.batches)
    assert catalog.SPLIT_BATCH_ACTIVITY in src or "SPLIT_BATCH_ACTIVITY" in src


def test_session_call_returns_a_batch_not_a_materialized_result():
    """A scoped call resolves to a ref (`dispatch_ref` + `Batch.from_ref`), never a fetch — so
    one Method's output feeds the next without a row entering the workflow. The Batch is produced
    in `_dispatch_batch`, which `Session.call` and the `MethodCall` dual object reach alike."""
    import inspect

    src = inspect.getsource(catalog.Session._dispatch_batch)
    assert "dispatch_ref" in src, "a scoped call must not go through a blob fetch"
    assert "Batch.from_ref" in src
    # The tuple, not a raise: `call` destructures into (results, dropped) (ADR 0028 §4).
    assert "Dropped(batch)" in inspect.getsource(catalog.Session.call)


# ---------------------------------------------------------------------------------------------
# The callable handle: a Method call returns (results, dropped) (ADR 0028 §4)
# ---------------------------------------------------------------------------------------------


def _fake_dispatch_ref(monkeypatch, ref, captured=None):
    """Stand in for the one seam a Method call reaches — the Nexus dispatch — so a `MethodCall`
    can run with no cluster. Captures the kwargs (which is how the routing is checked) and hands
    back a chosen ref."""
    async def dispatch_ref(self, units, **kw):
        if captured is not None:
            captured.append({"units": units, **kw})
        return dict(ref)

    monkeypatch.setattr(catalog.ActorHandle, "dispatch_ref", dispatch_ref)


def _await(method_call):
    """Await a `MethodCall` to completion outside a cluster. A Method call now returns the object
    that is both awaitable and async-iterable (ADR 0023 §8), and `asyncio.run` rejects a bare
    awaitable, so it is driven through a coroutine — `await ns.method(...)` is the real spelling
    this stands in for."""
    async def go():
        return await method_call

    return asyncio.run(go())


def test_the_handle_is_callable_and_a_method_call_returns_results_and_dropped(monkeypatch):
    """The hole this slice fills: `catalog.actor(n, v).method(batch)` used to raise AttributeError
    because ActorHandle had no __getattr__, forcing callers to hand-assemble the result. Now it is
    callable off a bare handle — no scope — and returns `(results, dropped)`."""
    captured = []
    _fake_dispatch_ref(monkeypatch, _ref(n=4, isolated=1, failures="f" * 8), captured)

    handle = catalog.actor("nscheck", "0.1.0")
    results, dropped = _await(handle.delegation([{"domain": "a"}]))

    assert isinstance(results, catalog.Batch) and len(results) == 4
    assert isinstance(dropped, catalog.Dropped) and len(dropped) == 1
    # The name rode the wire as the Method, and no scope means the shared queue every worker of
    # this version polls — not a per-session queue that only exists inside `async with`.
    assert captured[0]["method"] == "delegation"
    assert captured[0].get("session_id", "") == ""
    # `results` remembers who produced it, so it can chain and its rows dereference on the right
    # queue — the identity the old hand-assembled `Batch.from_ref(..., actor=, version=)` re-passed.
    assert (results.actor, results.version) == ("nscheck", "0.1.0")


def test_the_same_spelling_inside_a_scope_pins_to_the_session(monkeypatch):
    """The other half of the acceptance: `browser.crawl(batch)` on an open Session carries the
    session id, so it lands on that Session's own queue and reaches the one pinned process
    (ADR 0023 §6) — the same call, the same tuple, a different route."""
    captured = []
    _fake_dispatch_ref(monkeypatch, _ref(n=2), captured)

    scope = catalog.actor("crawler", "0.1.0").session()
    scope._id = "sid123"                                  # stand in for an opened scope
    results, dropped = _await(scope.crawl([1, 2]))

    assert len(results) == 2 and len(dropped) == 0
    assert captured[0]["method"] == "crawl"
    assert captured[0]["session_id"] == "sid123"


def test_dropped_everything_is_distinguishable_from_found_nothing(monkeypatch):
    """THE failure this return shape exists to prevent (ADR 0028 §4). On the survivors alone a run
    that dropped every Unit and one that legitimately found nothing are identical — both hand back
    an empty `results`. `dropped` is where they diverge, and it costs no fetch to ask."""
    def call_with(ref):
        _fake_dispatch_ref(monkeypatch, ref)
        return _await(catalog.actor("a", "1").m([1]))

    r_none, d_none = call_with(_ref(n=0, isolated=0))
    r_all, d_all = call_with(_ref(n=0, isolated=3, failures="f" * 8))

    assert len(r_none) == 0 and len(r_all) == 0           # indistinguishable on results
    assert bool(d_none) is False and len(d_none) == 0     # found nothing
    assert bool(d_all) is True and len(d_all) == 3        # dropped everything


def test_dropped_counts_without_a_fetch_and_materializes_the_units_with_one(monkeypatch):
    """`bool(dropped)` and `len(dropped)` ride the ref's meta, like `len(results)`; only
    `dropped.rows()` pays a fetch, and it exists to hand the lost Units back for a retry."""
    from temporalio import workflow as temporal_workflow

    fetched = []

    async def fake_execute_activity(name, arg, **_kw):
        fetched.append((name, arg))
        return [{"host": "x"}, {"host": "y"}]

    monkeypatch.setattr(temporal_workflow, "execute_activity", fake_execute_activity)

    batch = catalog.Batch.from_ref(
        _ref(n=1, isolated=2, failures="f" * 8), actor="probe", version="0.1.0")
    dropped = catalog.Dropped(batch)

    assert len(dropped) == 2 and bool(dropped) is True and not fetched, "count cost a fetch"
    assert asyncio.run(dropped.rows()) == [{"host": "x"}, {"host": "y"}]
    assert fetched and fetched[0][0] == catalog.FETCH_BLOB_ACTIVITY


def test_an_unknown_method_rides_to_a_polled_queue_and_is_rejected_there(monkeypatch):
    """The handle turns any attribute into a Method call, so a typo becomes a dispatch — but it
    lands on the shared queue workers actually poll, carrying `method=<name>`, and the actor's
    registry is what refuses it. It fails at the await, never by sitting on a queue nobody serves."""
    captured = []
    _fake_dispatch_ref(monkeypatch, _ref(n=0), captured)

    _await(catalog.actor("nscheck", "0.1.0").not_a_real_method([1]))
    assert captured[0]["method"] == "not_a_real_method"
    assert captured[0].get("session_id", "") == ""

    # The callee half, where the rejection actually happens — an unknown name raises rather than
    # resolving to whichever Method was declared first.
    from kontra import ActorRegistry

    with pytest.raises(TypeError):
        ActorRegistry().resolve_method("not_a_real_method")


# ---------------------------------------------------------------------------------------------
# The caller names a Method's output Dataset (ADR 0028 §2) — the second positional argument
# ---------------------------------------------------------------------------------------------


def _dispatch_by_len(monkeypatch, machine=""):
    """Fake the Nexus dispatch so a call's results Batch reports as many units as it was handed —
    which is what lets the iterating form's chunk totals be asserted against the awaiting form's
    whole."""
    async def dispatch_ref(self, units, **kw):
        n = units.n if isinstance(units, catalog.Batch) else len(list(units))
        return _ref(n=n, machine=machine)

    monkeypatch.setattr(catalog.ActorHandle, "dispatch_ref", dispatch_ref)


def _capture_publishes(monkeypatch, rows=1):
    """Capture what the publish activity is handed. `info()` is faked because publish reads a
    workflow id and start time off it, and the activity returns a fixed row count so a writer's
    running total is assertable."""
    from datetime import datetime, timezone

    from temporalio import workflow as temporal_workflow

    published: list[dict] = []

    async def fake_execute_activity(name, arg, **_kw):
        published.append({"activity": name, **arg})
        return {"rows": rows}

    class _Info:
        workflow_id = "NsCheck-1"
        start_time = datetime(2026, 8, 15, tzinfo=timezone.utc)

    monkeypatch.setattr(temporal_workflow, "execute_activity", fake_execute_activity)
    monkeypatch.setattr(temporal_workflow, "info", lambda: _Info())
    return published


def test_a_method_call_publishes_into_a_named_writer(monkeypatch):
    """The whole of slice 07: the caller hands the output Dataset as the SECOND positional
    argument — mirroring the Method's own `(self, batch, dataset)` — and the call publishes the
    results into it, so `await out.publish(...)` leaves the caller's loop (ADR 0028 §2)."""
    _dispatch_by_len(monkeypatch, machine="kf-dns-01")
    published = _capture_publishes(monkeypatch, rows=2)

    ns = catalog.actor("nscheck", "0.1.0")
    out = catalog.dataset("lame").writer()
    results, dropped = _await(ns.ask([{"host": "a"}, {"host": "b"}], out))

    assert len(results) == 2
    assert [p["dataset"] for p in published] == ["lame"]
    # The Batch's own Machine is forwarded — a multi-Machine run names every Machine, because
    # each dispatched Batch carries its own producer (the bug where 1,246 rows all said 'w').
    assert published[0]["machine"] == "kf-dns-01"
    assert out.rows == 2


def test_a_method_call_accepts_a_bare_dataset_handle_needing_no_scope(monkeypatch):
    """Either an open writer OR a bare handle, so a single call does not need an `async with`."""
    _dispatch_by_len(monkeypatch)
    published = _capture_publishes(monkeypatch)

    ns = catalog.actor("nscheck", "0.1.0")
    _await(ns.ask([1, 2, 3], catalog.dataset("lame")))
    assert [p["dataset"] for p in published] == ["lame"]


def test_omitting_the_destination_publishes_nothing_and_costs_nothing_extra(monkeypatch):
    """Leaving the argument off is the intermediate-fan-out case: an unnamed, chainable Batch and
    NO publish activity at all — the one-word choice at the call site that keeps it cheap."""
    _dispatch_by_len(monkeypatch)
    published = _capture_publishes(monkeypatch)

    results, dropped = _await(catalog.actor("a", "1").m([1, 2]))
    assert isinstance(results, catalog.Batch) and len(results) == 2
    assert published == []


def test_a_batch_handed_as_the_destination_is_refused(monkeypatch):
    """A Batch is chained by passing it as the FIRST argument; as the second it is a mistake, and
    silently treating it as "no destination" would drop rows a caller asked to name."""
    _dispatch_by_len(monkeypatch)
    _capture_publishes(monkeypatch)

    ns = catalog.actor("a", "1")
    with pytest.raises(TypeError):
        _await(ns.m([1], catalog.Batch.from_ref(_ref())))


def test_a_multi_machine_run_publishes_rows_naming_every_machine(monkeypatch):
    """Provenance is structurally correct: each dispatched Batch forwards ITS OWN producing
    Machine, so a run whose pages landed on different Machines publishes rows naming each — the
    fix for a four-Machine `nscheck` that once wrote 1,246 rows all reading `node='w'`."""
    seen = iter(["kf-dns-01", "kf-dns-02", "kf-dns-03"])

    async def dispatch_ref(self, units, **kw):
        return _ref(n=len(list(units)), machine=next(seen))

    monkeypatch.setattr(catalog.ActorHandle, "dispatch_ref", dispatch_ref)
    published = _capture_publishes(monkeypatch)

    ns = catalog.actor("nscheck", "0.1.0")
    out = catalog.dataset("lame").writer()
    for page in ([{"d": 1}], [{"d": 2}], [{"d": 3}]):
        _await(ns.ask(page, out))

    assert [p["machine"] for p in published] == ["kf-dns-01", "kf-dns-02", "kf-dns-03"]


# ---------------------------------------------------------------------------------------------
# A Method call can be iterated as its results land (ADR 0023 §8) — awaitable AND async-iterable
# ---------------------------------------------------------------------------------------------


async def _drain_call(mc):
    """Consume a MethodCall's iterating form, returning (total results, chunk count)."""
    total = chunks = 0
    async for results, dropped in mc:
        total += len(results)
        chunks += 1
    return total, chunks


def test_a_method_call_is_both_awaitable_and_iterable_with_identical_totals(monkeypatch):
    """One object, two ways to take the answer (ADR 0023 §8). The Actor is byte-identical between
    them — the caller writes `await` or `async for`, and the chunks partition the same input, so
    the totals reconcile."""
    _dispatch_by_len(monkeypatch)
    ns = catalog.actor("a", "1")
    units = list(range(450))                      # > two chunks at the bound of 200

    whole, _ = _await(ns.m(list(units)))
    total, chunks = asyncio.run(_drain_call(ns.m(list(units))))

    assert len(whole) == 450
    assert total == 450, "the iterating totals must reconcile with the awaiting whole"
    assert chunks == 3, "ceil(450 / 200) — one (results, dropped) per bounded chunk"


def test_iteration_chunk_granularity_is_bounded(monkeypatch):
    """The bound is the point (ADR 0023 §8): each chunk is a workflow-visible event, so a
    40,000-unit Batch iterated at per-record granularity would rebuild the history blow-up that
    capped a run near 5,100 units. At the fixed width of `SAFE_PAGE_MAX` a 1,000-unit Batch is
    five chunks, and no chunk exceeds the bound."""
    sizes: list[int] = []

    async def dispatch_ref(self, units, **kw):
        seq = list(units)
        sizes.append(len(seq))
        return _ref(n=len(seq))

    monkeypatch.setattr(catalog.ActorHandle, "dispatch_ref", dispatch_ref)

    asyncio.run(_drain_call(catalog.actor("a", "1").m(list(range(1000)))))
    assert len(sizes) == 5 == -(-1000 // catalog.SAFE_PAGE_MAX)
    assert max(sizes) <= catalog.SAFE_PAGE_MAX


def test_breaking_out_of_the_loop_stops_dispatching_the_rest(monkeypatch):
    """Break is defined, not discovered (ADR 0023 §8): each chunk is a COMPLETE Method call that
    finished before it was yielded, so stopping early simply never dispatches the remaining
    chunks — nothing is cancelled and no Actor is left mid-Batch."""
    dispatched: list[int] = []

    async def dispatch_ref(self, units, **kw):
        seq = list(units)
        dispatched.append(len(seq))
        return _ref(n=len(seq))

    monkeypatch.setattr(catalog.ActorHandle, "dispatch_ref", dispatch_ref)

    async def take_one():
        async for _results, _dropped in catalog.actor("a", "1").m(list(range(450))):
            break

    asyncio.run(take_one())
    assert dispatched == [catalog.SAFE_PAGE_MAX], "only the first chunk was dispatched"


def test_a_method_call_is_single_use(monkeypatch):
    """A MethodCall runs work, so consuming it twice would silently re-dispatch the whole Batch.
    It refuses instead — the defined answer to "a caller that iterates and then also awaits"."""
    _dispatch_by_len(monkeypatch)
    ns = catalog.actor("a", "1")

    awaited = ns.m([1, 2])
    _await(awaited)
    with pytest.raises(RuntimeError):
        _await(awaited)                                       # awaited twice

    iterated = ns.m([1, 2])
    asyncio.run(_drain_call(iterated))
    with pytest.raises(RuntimeError):
        asyncio.run(_drain_call(iterated))                   # iterated twice

    mixed = ns.m([1, 2])
    asyncio.run(_drain_call(mixed))
    with pytest.raises(RuntimeError):
        _await(mixed)                                         # awaited after iterating


def test_iterating_publishes_each_chunk_into_the_named_dataset(monkeypatch):
    """Slice 07 and 08 compose: iterating with a named destination streams each chunk to the
    Dataset as it lands AND hands the caller that chunk to act on — one publish per chunk."""
    _dispatch_by_len(monkeypatch, machine="kf-dns-01")
    published = _capture_publishes(monkeypatch, rows=200)

    ns = catalog.actor("nscheck", "0.1.0")
    out = catalog.dataset("lame").writer()

    async def drain():
        async for _results, _dropped in ns.ask(list(range(450)), out):
            pass

    asyncio.run(drain())
    assert len(published) == 3, "one publish per bounded chunk"
    assert {p["dataset"] for p in published} == {"lame"}
    assert out.rows == 600


# ---------------------------------------------------------------------------------------------
# Handles
# ---------------------------------------------------------------------------------------------


def test_actor_handle_derives_its_endpoint():
    h = catalog.actor("subfinder", "0.1.0")
    assert h.endpoint == "kontra-subfinder-0-1-0"


def test_actor_handle_endpoint_is_overridable():
    """A cross-namespace endpoint may be registered under another name; the derivation is the
    default, not a law."""
    h = catalog.actor("subfinder", "0.1.0", endpoint="partner-subfinder")
    assert h.endpoint == "partner-subfinder"


# --- virtual-object keying (ADR 0022) -------------------------------------------------------
#
# `handle[key]` is the whole caller-side of the pattern: it fills idempotency_key, which is what
# handler/nexus.go:backingWorkflowID keys the backing workflow on, which is what makes the batch
# exclusive per key and object_state addressable. Nothing else on the wire changes.


def test_getitem_binds_the_key_without_touching_the_address():
    a = catalog.actor("crawler", "0.1.0")
    keyed = a["acme.com"]
    assert keyed.key == "acme.com"
    assert (keyed.name, keyed.version, keyed.endpoint) == (a.name, a.version, a.endpoint)


def test_getitem_does_not_mutate_the_shared_handle():
    """Handles live at module scope next to the workflow class. If `a["x"]` keyed the handle in
    place, a second workflow sharing that module would silently dispatch to x too."""
    a = catalog.actor("crawler", "0.1.0")
    a["acme.com"]
    assert a.key == ""
    assert a["one.com"].key == "one.com" and a["two.com"].key == "two.com"


def test_a_bound_key_becomes_the_idempotency_key():
    """The handler derives the actor id from idempotency_key FIRST (runtime/handler/workflow.go:33), so
    this field — not node_id — is what pins the dispatch to one virtual object."""
    entry = catalog.entry_input(
        [1, 2], run_id="r", node_id="n", idempotency_key=catalog.actor("c", "1")["k"].key
    )
    assert entry["idempotency_key"] == "k"


def test_an_explicit_idempotency_key_outranks_the_binding():
    """Both spellings exist; the argument is the more specific one and must win, or a caller
    could not override a bound key for one dispatch."""
    import inspect

    src = inspect.getsource(catalog.ActorHandle.dispatch_ref)
    assert "idempotency_key or self.key" in src


def test_an_unusable_key_is_refused_at_the_call_site():
    """Each of these reaches Temporal as part of a workflow id and fails deep inside a Nexus
    start, where the error names neither the key nor the actor."""
    a = catalog.actor("crawler", "0.1.0")
    for bad in ("", "   ", "a\nb", "a\x00b", "x" * 401):
        with pytest.raises((ValueError, TypeError)):
            a[bad]
    with pytest.raises(TypeError):
        a[42]


def test_the_key_rides_the_nexus_summary():
    """`kontra runs` and the Temporal UI show the summary; a keyed dispatch that renders
    identically to an un-keyed one hides the one thing worth seeing."""
    import inspect

    src = inspect.getsource(catalog.ActorHandle.dispatch_ref)
    assert "self.key" in src.split("summary=")[1]
