"""Kontra SDK — the `kontra` package, and the whole of what an author writes against.

IT LIVES IN `sdk/python`, AND IT IMPORTS NOTHING OF `runtime/python` (ADR 0035 §2). The engine,
the codec, the state tiers and the Temporal hosts are the `internals` package one seam over; the
arrow runs runtime -> sdk and never back, and `tests/test_sdk_arrow.py` fails the build on a
module-scope crossing. The one exception is `serve()`, the entry-point handoff, spelled as a
deferred import inside the function body — so `import kontra` costs no runtime module, no
temporalio, no Redis and no object store (157 sys.modules, against 448 once a verb is named).

The IMPORT NAME did not move with the directory: an actor written against `import kontra` is
untouched by the split. What did move is Unit/Batch/Dataset and the type derivation, out of the
runtime and into the author surface where their names already were — `kontra.batch` and
`kontra.schema`, not `internals.batch` and `internals.schema`.

Two surfaces, one package:
  • `actor`   — the one deployed kind: an Actor with one or more Methods (ADR 0023 §9)
  • `catalog` — the CALLER's side: orchestrate deployed Actors from your own workflow

…and one step before the caller's side: `fleet`, which provisions the Machines a run executes
on, scoped to the workflow that runs it. It is separate from `catalog` on purpose — placement is
declarative state, dispatch is a loop you write, and fusing them is the boundary this system
keeps open.

There is no second kind. An **Activity** — a function with no loaded resource — is an Actor
with one Method and no `@actor.load`/`@actor.close`, which costs nothing it did not cost as a
kind of its own and spares the catalog, the CLI, the queue derivations and both SDKs a fork.

── THE TWO THINGS A WORKFLOW SAYS OUT LOUD ────────────────────────────────────────────────────

    from kontra import ask, note

    note(f"batch {i} of {n}")                        # tell the operator where you are
    answer = await ask("Approve these 12 hosts?", takes=Approval, context={"n": 12})

THEY ARE NOT A PAIR, AND THE DIFFERENCE IS THE WHOLE REASON THERE ARE TWO. `note` is a LOG LINE —
it costs no history, is not capped, and reaches the log store where it can be queried across runs.
`ask` costs history AND STOPS THE RUN until a human moves it. Confusing them turns a progress line
into a stalled run, so they are exposed side by side here rather than buried in two modules an
author would meet separately.

AND THERE IS A THIRD, WHICH IS THE ONE TO GET RIGHT: `partial` says the RESULT IS NOT WHAT A READER
WOULD ASSUME — an abandoned axis, a limit reached, a phase skipped. It emits at WARNING with a
structured `incomplete` field, and the console filters on that field INDEPENDENTLY of the level, so
raising the floor cannot hide it. The question at each call site is: would a reader be wrong about
the result if they missed this line? (ADR 0050 §2 — this replaced `speak`, which paid ~5 history
events and ~1s a sentence, capped at 200 a Run, for no property a log line lacks.)

None may carry a credential. `ask` reaches history, where the codec is a claim-check and not
encryption (ADR 0007): under its threshold a value rides inline, in the clear, readable by anyone
who can read the run. `note` and `partial` reach the log store, which is no more private. Each has a
guard against the ordinary mistake and none is a boundary.

BOTH ARE RESOLVED LAZILY, by the module `__getattr__` at the bottom of this file, and that is not
an optimisation — it is what keeps the paragraph below true. `say` and `hitl` import temporalio
at module scope (they must, to subclass `ApplicationError` at class-definition time), so importing
either here would put a Temporal dependency behind `import kontra`. The verbs cost nothing until
an author names one, and naming one is something only a workflow does.

── THE MODULES BEHIND THEM, AND WHAT ELSE IS NOT IMPORTED HERE ─────────────────────────────────

`hitl` is the module `ask` lives in — it parks a workflow on a question a human has to answer, and
carries `AskExpired`, `pending()` and the memo constants beside it. It is not imported here for the
reason above — the same exemption `contract` takes. Reach it directly, from inside your workflow:

    from kontra import hitl
    answer = await hitl.ask("Approve these 12 hosts?", takes=Approval, context={"n": 12})

A SECOND surface is deliberately not imported here, for a different reason than `hitl`'s:
`secrets`, which fetches this actor's OWN credential at load, authenticated as itself. It is
stdlib-only and would import cleanly, but it does network I/O and belongs to the ACTOR's process,
not to a workflow's — and `kontra/__init__` is imported inside the Temporal workflow sandbox.
Reach it from your actor, the same way:

    from kontra import secrets

    @actor.load
    async def load(self):
        self.client = Shodan(await secrets.get("shodan-key"))

`Slot` IS imported here, and that is not a contradiction with the paragraph above:
`actor.slot("api_key")` DECLARES a credential and does no I/O. Declaring is the half that has to
happen at import, so the operator can see what an actor will ask for BEFORE it runs; the handle
imports `secrets` lazily, inside `.get()`, so the network half stays out of the sandbox.

    API_KEY = actor.slot("api_key", "the vendor key this actor calls with")

    @actor.load
    async def load(self):
        self.client = Vendor(await API_KEY.get(run=self.run_id))

And a THIRD, on `hitl`'s grounds rather than `secrets`': `say`, the module `note` and `partial`
live in. Derived turns make an untooled workflow readable; these make a tooled one explain itself —
in the log store rather than in history, so the explanation can be searched across runs instead of
being read one execution at a time. It imports temporalio at module scope to reach
`workflow.logger`, which is replay-aware, and it is reached from inside your workflow:

    from kontra import say
    say.note(f"{live} of {len(apexes)} apexes resolve; crawling those")

`narrate.say` and `speak` were removed by ADR 0050 §2. A caller of either becomes `note`, or
`partial` where the sentence is a claim about the RESULT rather than about progress.
"""

from kontra import catalog, fleet
from kontra.actor import (
    Actor, ActorRegistry, ParamRef, MethodRegistration, Slot, SlotDeclaration, actor,
    unit_state, param, global_state, object_state,
)
from kontra.retry import NonRetryableError, SessionLost
from kontra.version import CONTRACT_VERSION

#: The verbs a workflow says out loud. RESOLVED ON FIRST USE by `__getattr__` below, never at
#: import: the modules they live in import temporalio at module scope, and `import kontra` is
#: required to stay free of a Temporal dependency (see the header).
#:
#: `speak` WAS HERE AND ITS REMOVAL WAS LEFT HALF DONE — ADR 0050 §2 deleted the function and
#: `__getattr__` stopped resolving it, but this tuple and `__all__` went on advertising it. So
#: `dir(kontra)` listed a name that raised, and `from kontra import *` failed outright. A surface
#: that names something it will not provide is worse than one that never mentioned it.
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    # THE LAZY `__getattr__` BELOW DEFEATS TYPE CHECKING, and for `progress` that is the whole
    # point of the function. MEASURED with mypy: imported from `kontra.say` a misspelled field is
    #
    #     error: Unexpected keyword argument "fond" for "progress"; did you mean "found"?
    #
    # and imported from `kontra` it is `error: "object" not callable` — the signature is gone, so
    # `progress(fond=7)` type-checks clean and the pane silently renders nothing. Re-exported here
    # under TYPE_CHECKING so a checker sees the real signatures while the runtime keeps the lazy
    # import that stops an author who never reports from paying for `temporalio`.
    from kontra.say import Progress as Progress
    from kontra.say import KontraFlow as KontraFlow
    from kontra.say import note as note
    from kontra.say import partial as partial
    from kontra.say import progress as progress
    from kontra.actor import stream as stream

_VERBS = ("note", "partial", "progress", "ask")

#: The two INPUT TYPES an author declares on a Method — `takes=File`. Lazy for the same reason the
#: verbs are, and a different dependency: `blobs` imports pydantic at module scope, which
#: `schema.py` already keeps off the workflow sandbox path deliberately. An author who never takes a
#: file never pays for it.
_INPUT_TYPES = ("File", "Folder")


def __getattr__(name: str) -> object:
    """`from kontra import ask, note`, without importing temporalio to find out.

    PEP 562, and the only mechanism that gives BOTH halves of what this package needs: the pair is
    reachable at the top level where an author looks for it, and `import kontra` still costs no
    Temporal import — which is what the workflow sandbox re-imports per instance, and what every
    non-workflow caller of this package (the CLI, a loader, a test) is entitled to.

    It is a re-export and never a wrapper: `kontra.ask is hitl.ask`, so there is one function,
    one docstring and one implementation behind either spelling.

    PLAIN `import` STATEMENTS, NOT `importlib`. This runs INSIDE the Temporal workflow sandbox,
    where an import statement goes through the sandbox's own import machinery — which passes
    `kontra` through by configuration (`internals/temporal/wfhost.py`). `importlib.import_module`
    reaches around that machinery entirely, which happens to give the same answer for a passthrough
    module and would quietly stop doing so the day one of these was not passed through.
    """
    if name == "stream":
        # `from kontra import actor` yields the REGISTRY INSTANCE, not the module — `actor` is an
        # object an author decorates with. The module has to be fetched by name; `engine.py` does
        # the same dance for `_run_params` and for the same reason.
        import importlib

        return importlib.import_module("kontra.actor").stream
    if name in ("note", "partial", "progress", "KontraFlow"):
        # ADR 0050 §2 — `speak` is gone. `note` is progress; `partial` says the RESULT is not what a
        # reader would assume and carries `incomplete=true`. Lazy for the same reason `ask` is: the
        # module imports `temporalio`, and an author who never logs should not pay for it.
        from kontra import say

        return getattr(say, name)
    if name == "ask":
        from kontra.hitl import ask

        return ask
    if name in _INPUT_TYPES:
        from kontra import blobs

        return getattr(blobs, name)
    raise AttributeError(f"module 'kontra' has no attribute {name!r}")


def __dir__() -> list[str]:
    """`dir(kontra)` names the verbs too — a lazy attribute is invisible to it otherwise, and a
    surface an author cannot discover from the REPL is a surface they will not find.

    DERIVED FROM `__all__`, not from the tuples beside it. `stream` and `KontraFlow` are lazy and
    public but belong to neither `_VERBS` (the workflow's verbs) nor `_INPUT_TYPES`, so a listing
    built only from those two silently omitted both — and an author hunting for the actor-side
    progress verb saw `progress` and not `stream`, which is the workflow's half and degrades to a
    log line inside an actor process. `__all__` is the one list that already has to name every
    public attribute, which is what stops the two drifting again.
    """
    return sorted({*globals(), *__all__, *_VERBS, *_INPUT_TYPES})


__all__ = [
    "actor",
    # THE VERBS, at the top level and side by side, which is what makes it obvious how they differ:
    # `note` reports progress and returns, `partial` reports that the RESULT is short and returns,
    # `ask` stops the run until a human moves it.
    "note",
    "partial",
    # `progress` is the fourth verb and the only one a MACHINE reads: typed state onto the run's
    # workflow stream, for a pane drawing now, where `note` is a sentence for a human reading after.
    "progress",
    # `stream` is the ACTOR's half: one typed record per Method, on that Method's own topic.
    # `progress` is the WORKFLOW's. They are different publishers with different vocabularies.
    "stream",
    # The base class `progress` needs: the stream must exist at construction (Temporal fixes the
    # handler set before the first activation), so one word on the class line is the minimum.
    #
    # NAMED `KontraFlow` AND NOT `Workflow` because `temporalio.workflow` is already imported into
    # every one of these files as `workflow`, and a `Workflow` beside it reads as Temporal's own.
    # A base class whose name makes an author guess which library owns it has already cost more
    # than the character count it saved.
    "KontraFlow",
    "ask",
    "catalog",
    "fleet",
    "param",
    "unit_state",
    "global_state",
    "object_state",
    "ParamRef",
    "Actor",
    "ActorRegistry",
    "MethodRegistration",
    "Slot",
    "SlotDeclaration",
    # THE TWO INPUT TYPES A FORM CAN COLLECT BY DRAGGING. Top level beside `Slot`, because they are
    # things an author DECLARES on a Method, not a module you call into.
    "File",
    "Folder",
    "NonRetryableError",
    "SessionLost",
    "CONTRACT_VERSION",
]
