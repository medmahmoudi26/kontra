"""Worker → orchestrator catalog client (content-pinned identity, ADR 0011).

A worker self-registers its OCI image digest with the orchestrator catalog on startup,
so a designed graph can pin that digest and `POST /api/runs` can stamp/verify it. These
are the only worker→orchestrator calls and they are strictly best-effort: stdlib urllib, no
new dependency, and every failure is swallowed — registration must never block serving.

BOTH HALVES OF A RUN REGISTER HERE. An actor pushes its descriptor from `serve()`; a
WORKFLOW worker pushes one per `@workflow.defn` class from `serve_workflows_async` (see
`publish_workflow_catalog`). It was actors only, so `GET /api/workflows` could say a file
exists and nothing about what it accepts, returns or is for — all three of which the author
had already written down in the run signature and the class docstring.

No-op unless KONTRA_ORCHESTRATOR_URL is set. KONTRA_ACTOR_DIGEST is NOT a second gate, though
this file used to say so: `kontra workers list` and dispatch-by-name both resolve through the
catalog, so refusing to register an actor that could not name its image would make it invisible
rather than unpinned. The digest simply rides the descriptor when the worker knows one.
"""

from __future__ import annotations

import inspect
import json
import os
import typing
import urllib.error
import urllib.request
from pathlib import Path

from internals.manifest import catalog_key


def register_actor_catalog(url, m, operations: list, *, timeout: float = 5.0) -> int:
    """POST the full actor descriptor to `{url}/api/actors` — the catalog the CLI resolves a
    dispatch through and `kontra workers list` joins against. Returns the HTTP status;
    transport errors propagate (the build counts them, unlike the best-effort digest call
    below). `m` is the ActorManifest.

    `operations` is ONE PER METHOD (ADR 0023 §9), each carrying its own schemas. It used to be
    a single operation named `run` built from one Actor-level input/output pair, which stopped
    being expressible the moment an Actor could hold `fetch` and `title` with different
    signatures. An empty list is valid and means identity only: a load-only actor, or one whose
    Methods declare no types, is registered and dispatchable rather than invisible."""
    descriptor = {
        "key": catalog_key(m.name, m.version),
        "name": m.name,
        "version": m.version,
        "schemaVersion": m.schema_version,
        "operations": operations,
    }
    # THE IMAGE THIS WORKER IS RUNNING (ADR 0011), from the same env var the Go SDK reads.
    #
    # This body never carried a digest, and the only other path that could — register_actor_digest
    # below — has no caller anywhere in the repo. So a Python actor registered a descriptor with
    # no digest through every path there is, and content pinning was silently off for the entire
    # Python fleet while the field, the column and the endpoint all existed.
    #
    # OMITTED WHEN UNSET, never "": the orchestrator keeps a previously registered digest only
    # when the key is ABSENT (`body.digest ?? prev.digest`), so an empty string is not "I don't
    # know", it is "unpin it".
    digest = os.environ.get("KONTRA_ACTOR_DIGEST", "")
    if digest:
        descriptor["digest"] = digest
    # WHERE THE CODE IS, from the worker that is running it.
    #
    # Nothing linked a registered actor back to its source. `.kontra/actors/` is where an
    # operator's own actors go and it is usually empty — every actor in this catalog was
    # run from `examples/` or a checkout somewhere — so the Actors page could list
    # twenty-three actors and offer no way to reach any of their code.
    #
    # IT IS THE DIRECTORY THIS WORKER LOADED FROM, which is not always a path on the
    # reader's machine: on a fleet Machine it is `/opt/kontra/actor/<name>`. That is still
    # the honest answer to "where did this actor come from", and the surfaces say which
    # worker said it rather than implying it is local.
    source = str(m.actor_dir) if m.actor_dir else ""
    if source:
        descriptor["source"] = source
    body = json.dumps(descriptor).encode()
    req = urllib.request.Request(
        f"{url.rstrip('/')}/api/actors",
        data=body,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return resp.status


def register_actor_digest(
    url: str, key: str, name: str, version: str, digest: str, *, timeout: float = 3.0
) -> bool:
    """POST `{name, version, digest}` to `{url}/api/actors/{key}/digest`. Returns True on
    a 2xx, False on an empty url/digest or any transport error (best-effort). The digest
    endpoint updates ONLY the digest, preserving any operations/schemas already in the
    catalog, so a worker and the design-tool upload don't clobber each other."""
    if not url or not digest:
        return False
    payload = json.dumps({"name": name, "version": version, "digest": digest}).encode()
    req = urllib.request.Request(
        f"{url.rstrip('/')}/api/actors/{key}/digest",
        data=payload,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return 200 <= resp.status < 300
    except (urllib.error.URLError, OSError):
        return False


def operations_of(registry) -> list[dict]:
    """One catalog operation per Method, each with the schemas that Method declares.

    ADR 0023 §9: an Actor holds many Methods and each has its own signature, so the catalog
    describes Methods, not the Actor. A Method that declares neither `takes` nor `emits`
    contributes an operation with no schemas — it is dispatchable and simply advertises
    nothing, which is a far better answer than the actor vanishing from the catalog.

    `params` is the one thing that stays ACTOR-level (run-wide config, not a signature), so it
    rides on every operation rather than being declared per Method.
    """
    from kontra.schema import schema_of

    params = schema_of(getattr(registry, "params_type", None))
    ops = []
    for method in getattr(registry, "methods", {}).values():
        op = {"name": method.name}
        # WHAT THE METHOD IS FOR, from the author's own docstring. The catalog carried a name and
        # two schemas, so every surface that listed a Method could say what it TAKES and never what
        # it DOES — and an operator composing one Actor's Method into another's had nothing to
        # compose from. Omitted when the author wrote nothing, never sent as an empty string: a
        # blank description and an absent one render differently and mean different things.
        description = getattr(method, "description", "")
        if description:
            op["description"] = description
        for field, tp in (("input", method.takes), ("output", method.emits)):
            schema = schema_of(tp)
            if schema is not None:
                op[field] = schema          # omitted entirely when undeclared, never null
        if params:
            op["params"] = params
        ops.append(op)
    return ops


def publish_catalog(registry) -> None:
    """Publish this actor's full catalog record — identity plus one operation per Method.

    A worker is the only thing that can do this honestly: the schemas are DERIVED from the
    dataclasses the running code actually declares, so what the catalog advertises and what the
    actor will accept cannot drift. The alternative — a build-time push, or a hand-written
    `register_actor` call — is a promise about code that may not be the code that boots.

    This was the missing wire. `internals/catalog.py` was written, documented as "the build-time
    catalog write", and tested, but nothing in the running system ever called it. The visible
    symptom was small and the diagnosis was not: `kontra deploy` succeeded, the image was pushed,
    the worker polled its queue — and `scale_actor` still said "not registered", because the
    catalog had never heard of it.

    EVERY actor registers, typed or not. Refusing to register an actor that declared no types
    was the same bug wearing the other hat: `kontra workers list` joins the catalog against live
    pollers and a caller's `catalog.actor(name)` resolves through it, so an untyped actor served
    happily while being invisible and uncallable. Schemas are a description of an actor, not
    a licence to exist.

    Best-effort, and deliberately so: registration is a convenience for the design surface, and a
    worker that can serve traffic must serve it whether or not the orchestrator is reachable.
    Every failure prints and is swallowed. Silence is not one of the options — a swallowed
    exception with no line is how this became invisible in the first place.
    """
    url = os.environ.get("KONTRA_ORCHESTRATOR_URL")
    if not url:
        return
    try:
        from internals.manifest import ActorManifest

        m = ActorManifest(schema_version="kontra.actor.v1", name=registry.actor_name,
                          version=getattr(registry, "version", "") or "",
                          actor_dir=getattr(registry, "actor_dir", None) or Path("."))
        operations = operations_of(registry)
        status = register_actor_catalog(url, m, operations)
        print(f"[catalog] {m.name}@{m.version} -> POST {url}/api/actors {status} "
              f"({len(operations)} method(s))", flush=True)
        publish_slots(url, registry, m.name, m.version)
    except Exception as e:  # noqa: BLE001 — never block serving, but never go quiet either
        print(f"[catalog] registration failed ({type(e).__name__}: {e}) — "
              f"the actor still serves; wire it by hand if the graph UI shows no ports", flush=True)


def slots_of(registry) -> list[dict]:
    """`[{"name": "api_key", "description": "…"}]` — what this actor asks for, by ITS OWN names.

    A slot is not a secret name (`lib/actor.py:Slot`). What crosses this wire is a name the author
    chose and the sentence they wrote about it; the operator binds each one to a secret of theirs,
    and the actor never learns which. There is no field here a value could ride in."""
    out = []
    for decl in getattr(registry, "slots", {}).values():
        entry = {"name": decl.name}
        if decl.description:
            entry["description"] = decl.description
        out.append(entry)
    return out


def publish_slots(url: str, registry, name: str, version: str) -> None:
    """Register what this `(actor, version)` asks for, and SAY WHAT IS STILL UNBOUND.

    THE PRINT IS THE POINT, not the POST. The orchestrator refuses a run whose actor has an unbound
    slot, which is the gate that matters — but the operator serving an actor for the first time is
    watching THIS pane, and telling them here, by name, which credentials they now have to bind is
    the difference between binding them now and discovering them from a refused run twenty minutes
    later.

    IT DOES NOT REFUSE TO SERVE. An unbound slot at serve time is the ordinary first state of every
    actor that needs a credential — the operator cannot bind what has not been declared yet, so a
    serve that failed on it would be a deadlock with a good excuse. The refusal belongs at the run.

    Best-effort and never silent, exactly like the registration above it: an actor that can serve
    traffic serves it whether or not the orchestrator is reachable.
    """
    slots = slots_of(registry)
    if not slots:
        return
    try:
        from kontra import secrets

        status = secrets.declare(url, name, version, slots)
        print(f"[catalog] {name}@{version} -> POST {url}/api/slots/declare {status} "
              f"({len(slots)} slot(s): {', '.join(s['name'] for s in slots)})", flush=True)
    except Exception as e:  # noqa: BLE001 — never block serving, but never go quiet either
        print(f"[catalog] slot declaration failed ({type(e).__name__}: {e}) — this actor's "
              f"credentials will not be bindable until it registers them", flush=True)


# ---------------------------------------------------------------------------------------------
# The CALLER's side: a workflow registers what it takes, returns and is for
# ---------------------------------------------------------------------------------------------


def first_paragraph(doc: str | None) -> str:
    """The first paragraph of a docstring, dedented — how a Method's description is derived.

    `inspect.cleandoc` first, because every line but the first of a docstring carries the
    indentation of the `class`/`def` it sits under, and sending that verbatim puts leading
    spaces into a surface that renders it as one line.

    The peer is `MethodRegistration.description` in `sdk/python/kontra/actor.py`; one rule for
    what an author's words mean, so a Method and a Workflow are described the same way.
    """
    return inspect.cleandoc(doc or "").split("\n\n", 1)[0].strip()


def run_types(run_fn) -> tuple[type | None, type | None]:
    """The types the `@workflow.run` method declares: (what it takes, what it returns).

    `typing.get_type_hints` rather than `__annotations__`, because a module with `from
    __future__ import annotations` (or any string annotation) holds the TEXT of a type and
    `TypeAdapter("dict")` describes a string.

    `None` means UNANNOTATED, and it is not the same as `-> None`: an unannotated slot has no
    schema at all while `-> None` resolves to `NoneType` and describes a workflow that returns
    null. Collapsing the two would advertise "returns nothing" about a workflow that simply
    never said.

    THE FIRST ARGUMENT ONLY. Temporal's Python SDK accepts a multi-argument run method, and
    `WorkflowDescriptor` has one `input` — the one argument every surface here starts a workflow
    with (`kontra workflow start --input`, the Run button's one JSON box). A second parameter is
    left undescribed rather than described as the first.
    """
    hints = typing.get_type_hints(run_fn)
    # The receiver is dropped by POSITION, not by the name `self`: it is a plain function taken
    # off the class, and an author may spell its first parameter anything at all.
    params = list(inspect.signature(run_fn).parameters)[1:]
    takes = hints.get(params[0]) if params else None
    return takes, hints.get("return")


def workflow_descriptor(cls, *, queue: str = "") -> dict | None:
    """The `WorkflowDescriptor` for one `@workflow.defn` class, or None if it is not one.

    `name` is the TYPE A CALLER STARTS, which is `@workflow.defn(name=…)` when the author
    overrode it and the class name otherwise — not the file, and not the class in that case.
    Temporal accepts a start for any type name and only a worker that registered THAT name picks
    the task up, so a descriptor keyed off the class would name something that hangs.

    `queue` IS THE OTHER HALF OF "HOW DO I START THIS", and it was missing. Temporal accepts a
    start on ANY queue name and routes it there; only a worker polling that exact queue takes the
    task. So a caller with the right TYPE and the wrong QUEUE gets a run that starts, sits, and —
    if some other worker happens to poll that queue — fails its workflow task with `Workflow class
    Canary is not registered on this worker`, over and over, while every surface reports it
    `running`. MEASURED: the Workflows page defaulted its queue field to a previous session's
    value and started `Canary` on `recon`.

    THE WORKER IS THE ONLY HONEST SOURCE for it. It is about to poll that queue; nothing else
    knows, and a value written in a manifest is a promise about a process that may never have
    started. Omitted rather than sent empty when a caller does not supply one — see the docstring
    rule above: undeclared and declared-as-nothing must not render the same.

    The schemas come from `schema_of`, the same derivation a Method's do, so a workflow and a
    Method describe their types the same way or not at all.
    """
    from temporalio import workflow

    from kontra.schema import schema_of

    # None, not a raise, for an ordinary class: `serve()` is handed a list an author wrote, and
    # a helper class in it should be reported and skipped rather than take the whole worker's
    # registration with it.
    defn = workflow._Definition.from_class(cls)
    if defn is None:
        return None

    descriptor: dict = {"name": defn.name}
    if queue:
        descriptor["queue"] = queue
    # WHAT IT IS FOR, in the author's own words. Omitted when the class carries no docstring,
    # never sent as "": undescribed and described-with-nothing render differently, and only one
    # of them is the author's silence.
    description = first_paragraph(cls.__doc__)
    if description:
        descriptor["description"] = description
    takes, returns = run_types(defn.run_fn)
    for field, tp in (("input", takes), ("output", returns)):
        schema = schema_of(tp)
        if schema is not None:
            descriptor[field] = schema      # omitted entirely when undeclared, never null
    return descriptor


def register_workflow_catalog(url: str, descriptor: dict, *, timeout: float = 5.0) -> int:
    """POST one workflow descriptor to `{url}/api/workflows/catalog`. Returns the HTTP status;
    transport errors propagate to the best-effort loop below.

    NOT `/api/workflows`, which is the FILE surface — `.kontra/workflows/` listed by name, byte
    count and mtime. A registration is about a workflow TYPE and arrives from a worker that may
    be serving code from anywhere; posting it to the route that lists an operator's directory
    would conflate a file on this host with a type on the wire.
    """
    body = json.dumps(descriptor).encode()
    req = urllib.request.Request(
        f"{url.rstrip('/')}/api/workflows/catalog",
        data=body,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return resp.status


def publish_workflow_catalog(workflows, *, queue: str = "") -> None:
    """Register every workflow this worker is about to serve — the caller-side peer of
    `publish_catalog`.

    THE WORKER IS THE ONLY HONEST REGISTRAR, for the same reason it is on the actor side: the
    name is the one Temporal routes on, the schemas are derived from the annotations the running
    code declares, and the description is the docstring in the file that just imported. A
    build-time push or a hand-written registration is a promise about code that may not be the
    code that boots.

    ONE POST PER CLASS, EACH IN ITS OWN try. A file may declare several `@workflow.defn` classes
    and `serve([A, B])` is an ordinary spelling — so a class with an annotation pydantic cannot
    describe, or one POST that fails, must not take the others' registration with it.

    Best-effort, and deliberately so: a worker that can serve traffic must serve it whether or
    not the orchestrator is reachable, so every failure prints and is swallowed. Silence is not
    one of the options — a swallowed exception with no line is how the actor side became
    invisible in the first place.
    """
    url = os.environ.get("KONTRA_ORCHESTRATOR_URL")
    if not url:
        return
    for cls in workflows:
        named = getattr(cls, "__name__", str(cls))
        try:
            descriptor = workflow_descriptor(cls, queue=queue)
            if descriptor is None:
                print(f"[catalog] {named} is not a @workflow.defn — nothing to register",
                      flush=True)
                continue
            status = register_workflow_catalog(url, descriptor)
            print(f"[catalog] workflow {descriptor['name']} -> "
                  f"POST {url}/api/workflows/catalog {status}", flush=True)
        except Exception as e:  # noqa: BLE001 — never block serving, but never go quiet either
            print(f"[catalog] workflow {named} registration failed "
                  f"({type(e).__name__}: {e}) — it still serves; the Workflows page will show "
                  f"the file with no contract", flush=True)


def defn_classes(module) -> list[type]:
    """Every `@workflow.defn` class DEFINED IN this module, in definition order.

    The SAME predicate the catalog test's `defns_in` uses and the SAME one `serve()` is handed a
    list against — a class Temporal will register, told apart from a helper class in the file by
    asking Temporal, not by a name convention. Watch mode (`internals/wfwatch.py`) re-imports the
    file on save and needs to find the workflows in the fresh module the same way boot did; keeping
    that here is what stops a second, drifting notion of "which classes are workflows".

    DEFINED IN, NOT PRESENT IN. `vars(module)` is the module's whole namespace, so a served file
    that says `from shared import Base` holds `Base` there exactly as it holds the classes it
    declares. Scraping the namespace alone re-registered every workflow the file merely IMPORTS
    under THIS file's queue — a queue whose worker does not serve it, which is the "starts, routes
    nowhere, reports running forever" failure `queue` exists to prevent. `__module__` is what the
    class body recorded when it executed, so it names the file that DECLARED the class no matter
    which namespaces it was later imported into.

    The boot path never had this: `serve()` publishes the explicit list its author handed over, so
    an imported helper only ever reached the catalog through the namespace scrape here.
    """
    from temporalio import workflow

    here = getattr(module, "__name__", None)
    return [
        v
        for v in vars(module).values()
        if isinstance(v, type)
        and getattr(v, "__module__", None) == here
        and workflow._Definition.from_class(v) is not None
    ]


def publish_workflow_broken(names, error: str, *, queue: str = "") -> None:
    """Register that a workflow's file NO LONGER IMPORTS — a broken file is a state, not a silence.

    Watch mode re-imports the served file on every save. When that import raises there are no fresh
    classes to describe, so nothing can go through `publish_workflow_catalog` — and leaving the last
    good descriptor in place would paint "your contract is fine" over a file that will not load,
    which is the opposite of what the loop is for. This posts the honest state instead: for each type
    the manifest declares (`names`), a descriptor carrying the import error verbatim and the queue the
    worker is STILL polling. The store overwrites the schema slots to null, so the page drops the
    stale form and shows the error; a later clean re-derivation clears `error` and restores it.

    Keyed by the type the way `publish_workflow_catalog` is, because the import failed and there are
    no classes to read a name off — the caller passes the type name(s) it already knew from the last
    good serve (or the manifest). Best-effort and it never raises, exactly like the register path it
    stands in for: a catalog that cannot be reached must not turn a save into a crash.
    """
    url = os.environ.get("KONTRA_ORCHESTRATOR_URL")
    if not url:
        return
    for name in names:
        descriptor: dict = {"name": name, "error": error}
        if queue:
            descriptor["queue"] = queue
        try:
            status = register_workflow_catalog(url, descriptor)
            print(f"[catalog] workflow {name} -> BROKEN -> "
                  f"POST {url}/api/workflows/catalog {status}", flush=True)
        except Exception as e:  # noqa: BLE001 — a save must not become a crash
            print(f"[catalog] workflow {name} broken-state post failed "
                  f"({type(e).__name__}: {e}) — the last contract stands until the catalog is "
                  f"reachable", flush=True)
