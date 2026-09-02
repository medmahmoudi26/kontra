"""Kontra SDK — the `actorkit` package, and the whole of what an author writes against.

IT LIVES IN `sdk/python`, AND IT IMPORTS NOTHING OF `runtime/python` (ADR 0035 §2). The engine,
the codec, the state tiers and the Temporal hosts are the `internals` package one seam over; the
arrow runs runtime -> sdk and never back, and `tests/test_sdk_arrow.py` fails the build on a
module-scope crossing. The one exception is `serve()`, the entry-point handoff, spelled as a
deferred import inside the function body — so `import actorkit` costs no runtime module, no
temporalio, no Redis and no object store (157 sys.modules, against 448 once a verb is named).

The IMPORT NAME did not move with the directory: an actor written against `import actorkit` is
untouched by the split. What did move is Unit/Batch/Dataset and the type derivation, out of the
runtime and into the author surface where their names already were — `actorkit.batch` and
`actorkit.schema`, not `internals.batch` and `internals.schema`.

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

    from actorkit import ask, speak

    await speak(f"batch {i} of {n}")                  # tell the operator where you are
    answer = await ask("Approve these 12 hosts?", takes=Approval, context={"n": 12})

THEY ARE A PAIR, AND THE DIFFERENCE IS THE WHOLE REASON THERE ARE TWO. `speak` costs history and
RETURNS IMMEDIATELY. `ask` costs history AND STOPS THE RUN until a human moves it. Confusing them
turns a progress line into a stalled run, so they are exposed side by side here rather than
buried in two modules an author would meet separately. `speak`'s shape is A SENTENCE PER PHASE,
never one per Unit — see {@link actorkit.narrate.speak} for the budget that enforces it.

Neither may carry a credential. Both reach history, where the codec is a claim-check and not
encryption (ADR 0007): under its threshold a value rides inline, in the clear, readable by anyone
who can read the run. Each has a guard against the ordinary mistake and neither is a boundary.

BOTH ARE RESOLVED LAZILY, by the module `__getattr__` at the bottom of this file, and that is not
an optimisation — it is what keeps the paragraph below true. `narrate` and `hitl` import temporalio
at module scope (they must, to subclass `ApplicationError` at class-definition time), so importing
either here would put a Temporal dependency behind `import actorkit`. The verbs cost nothing until
an author names one, and naming one is something only a workflow does.

── THE MODULES BEHIND THEM, AND WHAT ELSE IS NOT IMPORTED HERE ─────────────────────────────────

`hitl` is the module `ask` lives in — it parks a workflow on a question a human has to answer, and
carries `AskExpired`, `pending()` and the memo constants beside it. It is not imported here for the
reason above — the same exemption `contract` takes. Reach it directly, from inside your workflow:

    from actorkit import hitl
    answer = await hitl.ask("Approve these 12 hosts?", takes=Approval, context={"n": 12})

A SECOND surface is deliberately not imported here, for a different reason than `hitl`'s:
`secrets`, which fetches this actor's OWN credential at load, authenticated as itself. It is
stdlib-only and would import cleanly, but it does network I/O and belongs to the ACTOR's process,
not to a workflow's — and `actorkit/__init__` is imported inside the Temporal workflow sandbox.
Reach it from your actor, the same way:

    from actorkit import secrets

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

And a THIRD, on `hitl`'s grounds rather than `secrets`': `narrate`, the module `speak` lives in,
which writes one sentence of the author's own prose into the run's transcript at the point in the
run where it was written — derived turns make an untooled workflow readable; narration makes a
tooled one explain itself. It too imports temporalio at module scope, to subclass
`ApplicationError`, and it too is reached from inside your workflow:

    from actorkit import narrate
    await narrate.say(f"{live} of {len(apexes)} apexes resolve; crawling those")

`narrate.say` is `speak` under its older name — one function, either spelling, and every existing
caller of `say` is untouched.
"""

from actorkit import catalog, fleet
from actorkit.actor import (
    Actor, ActorRegistry, ParamRef, MethodRegistration, Slot, SlotDeclaration, actor,
    unit_state, param, global_state, object_state,
)
from actorkit.retry import NonRetryableError, SessionLost
from actorkit.version import CONTRACT_VERSION

#: The two verbs a workflow says out loud. RESOLVED ON FIRST USE by `__getattr__` below, never at
#: import: the modules they live in import temporalio at module scope, and `import actorkit` is
#: required to stay free of a Temporal dependency (see the header).
_VERBS = ("speak", "ask")


def __getattr__(name: str) -> object:
    """`from actorkit import ask, speak`, without importing temporalio to find out.

    PEP 562, and the only mechanism that gives BOTH halves of what this package needs: the pair is
    reachable at the top level where an author looks for it, and `import actorkit` still costs no
    Temporal import — which is what the workflow sandbox re-imports per instance, and what every
    non-workflow caller of this package (the CLI, a loader, a test) is entitled to.

    It is a re-export and never a wrapper: `actorkit.ask is hitl.ask`, so there is one function,
    one docstring and one implementation behind either spelling.

    PLAIN `import` STATEMENTS, NOT `importlib`. This runs INSIDE the Temporal workflow sandbox,
    where an import statement goes through the sandbox's own import machinery — which passes
    `actorkit` through by configuration (`internals/temporal/wfhost.py`). `importlib.import_module`
    reaches around that machinery entirely, which happens to give the same answer for a passthrough
    module and would quietly stop doing so the day one of these was not passed through.
    """
    if name == "speak":
        from actorkit.narrate import speak

        return speak
    if name == "ask":
        from actorkit.hitl import ask

        return ask
    raise AttributeError(f"module 'actorkit' has no attribute {name!r}")


def __dir__() -> list[str]:
    """`dir(actorkit)` names the verbs too — a lazy attribute is invisible to it otherwise, and a
    surface an author cannot discover from the REPL is a surface they will not find."""
    return sorted([*globals(), *_VERBS])


__all__ = [
    "actor",
    # THE PAIR, at the top level and side by side, which is what makes it obvious there are two of
    # them: `speak` reports and returns, `ask` stops the run until a human moves it.
    "speak",
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
    "NonRetryableError",
    "SessionLost",
    "CONTRACT_VERSION",
]
