"""Kontra actor SDK — the author surface.

Authors write plain async functions and decorate them:

    from actorkit import actor

    @actor.load
    async def open_resource(self):
        self.resource = ...

    @actor.method
    async def crawl(self, batch, dataset): ...

    @actor.method
    async def extract(self, batch, dataset): ...

    @actor.close
    async def shutdown(self):
        await self.resource.close()

Decorators only REGISTER; the bodies run later, on the actor host, against
ONE shared `Actor` instance (so `self.browser` opened in load is visible to every
Method). An actor declares as MANY Methods as it likes and the caller names the one
it wants — chaining a transformation no longer costs a second actor and an edge
between them (ADR 0023 §9).

Pure Python (no temporalio import) so it is safe to import anywhere. The host wiring —
a Temporal activity worker — lives in `internals/temporal`, under `runtime/python/`.

THE ARROW, AND THE ONE EXCEPTION IN THIS FILE. `sdk/` may not import `runtime/`: the runtime
imports the author's vocabulary (Unit, Batch, Dataset, the schema derivation), never the other
way, and tests/test_sdk_arrow.py fails the build on a module-scope `internals` import anywhere
under sdk/. `serve()` is the exception, because serve() IS the handoff — the line where an author
stops writing code and gives the process to the engine. It is spelled as a DEFERRED import, inside
the method body (search `from internals.temporal.host import serve` below), so the edge exists only
once control has already been handed over: `import actorkit` still reaches no runtime module, no
Temporal, no Redis and no object store.
"""

from __future__ import annotations

import contextvars
import inspect
import warnings
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Awaitable, Callable, Optional

# An author function: async (self, batch, dataset) for a Method, or async (self) -> Any for
# load/close/healthcheck.
ActorFn = Callable[..., Awaitable[Any]]

# Run-wide params of the CURRENTLY-executing load/step/close body, so the canonical
# `param.get(...)` returns real values in a BODY. A contextvar so concurrent step bodies on
# one Actor don't clobber each other; `None` outside a run (e.g. at import time, when a
# `@actor.method/load` decorator is evaluated).
_run_params: contextvars.ContextVar = contextvars.ContextVar("kontra_run_params", default=None)

# `session_state` was a fourth tier and RETIRED with ADR 0023 §19. Every path that could read it
# runs in the same process on the same instance, where `self.*` already works; the one path that
# loses `self.*` is host death, and that FAILS THE SCOPE (§7), so the reader is gone too.
#
# The rule it leaves behind is one sentence: DURABLE MEANS KEYED, IN-MEMORY MEANS SCOPED.
# `self.*` is the Session's memory, `object_state` is the key's, `global_state` is the name's.
#
# `unit_state` SURVIVES — see the amendment to §19. Its reader is not the isolated Unit the ADR
# reasoned about; it is a Unit that was IN FLIGHT when the activity died and re-runs on the
# handler's retry, against the same batch hash and therefore the same slot.

# Per-unit unit_state IO, bound by the host around each in-flight Unit. A contextvar
# (task-local) so units running concurrently each see their OWN slot; `None` outside a hosted
# run, which makes unit_state.get()/set() safe no-ops in plain unit tests of Method bodies. (The
# internal name keeps "ckpt", matching the frozen `{slot}-ckpt` state key.)
_ckpt_io: contextvars.ContextVar = contextvars.ContextVar("kontra_ckpt_io", default=None)


class _UnitState:
    """Per-unit durable RESUME SCRATCH for a FAT Unit (a crawl frontier, a cursor) — not a result
    store. KEYED: `await self.unit_state.get(key)` is `None` on a fresh Unit and the last
    `set(key, value)` snapshot when the Unit re-runs after a death, so one Unit can carry several
    independent resume slots. The framework DELETES the whole Unit's scratch when the Unit
    commits, so a finished Unit never resumes.

    WHO READS IT, precisely — because ADR 0023 §19 got this wrong once and retired it: a Unit
    that was IN FLIGHT when the activity died, re-running on the handler's retry
    (`MaximumAttempts: 3`) against the same session queue, the same worker and the same batch
    hash — hence the same slot. Committed Units are skipped by the commit map and ISOLATED Units
    are never resumed, so neither of those reads it; the in-flight one does, and for a Unit whose
    body runs for minutes that is the difference between resuming and starting over.

    Contents are opaque JSON, author-owned; `set()` is at-least-once (a death between the work
    and its `set()` replays that slice — snapshots must tolerate replay). Outside the host get()
    -> None and set()/delete() are no-ops."""

    async def get(self, key: str) -> Any:
        io = _ckpt_io.get()
        return await io.get(key) if io is not None else None

    async def set(self, key: str, value: Any) -> None:
        io = _ckpt_io.get()
        if io is not None:
            await io.set(key, value)

    async def delete(self, key: str) -> None:
        io = _ckpt_io.get()
        if io is not None:
            await io.delete(key)


unit_state = _UnitState()


# Cross-session global_state IO, bound by the host per batch (over its own ETag'd Redis keys,
# not the per-actor state hash). A contextvar so it's None outside a hosted run,
# making global_state reads None / writes no-ops in plain unit tests.
_global_io: contextvars.ContextVar = contextvars.ContextVar("kontra_global_io", default=None)

# Key-scoped object_state IO — the same store shape as global_state under a namespace that
# carries the actor's KEY, bound by the host per batch. Same no-op-outside-host rule.
_object_io: contextvars.ContextVar = contextvars.ContextVar("kontra_object_io", default=None)


class _AtomicState:
    """The two CROSS-SESSION durable tiers. They are the same operations over the same store
    shape and differ ONLY in what they are scoped BY:

      • `global_state` — actor-NAME-scoped (ADR 0015 tier 3). Every session of every key
        shares it, and it deliberately survives a version bump.
      • `object_state` — actor-KEY-scoped (ADR 0022 tier 4). Only sessions dispatched under
        the same `idempotency_key` share it; two keys of one actor cannot read each other.
        This is the tier that makes an actor a virtual object: `crawler["acme.com"]` gets
        state that belongs to acme.com and outlives the batch that created it.

    Both offer ATOMIC ops so concurrent sessions never lose updates: `add_to_set` (the dedupe
    flagship — a shared visited/seen set), `incr` (a shared counter), and `compare_and_set`.
    Reach for the atomics, not get/set + your own read-modify-write — plain get/set is
    LAST-WRITE-WINS across sessions and silently drops concurrent updates. Outside the host
    (plain unit tests) reads are None, writes no-op."""

    def __init__(self, io: "contextvars.ContextVar", tier: str) -> None:
        self._io = io
        self._tier = tier

    def __repr__(self) -> str:  # pragma: no cover - debugging affordance
        return f"<kontra {self._tier}>"

    async def get(self, key: str) -> Any:
        io = self._io.get()
        return await io.get(key) if io is not None else None

    async def set(self, key: str, value: Any) -> None:
        io = self._io.get()
        if io is not None:
            await io.set(key, value)

    async def add_to_set(self, key: str, member: Any) -> bool:
        io = self._io.get()
        return await io.add_to_set(key, member) if io is not None else False

    async def incr(self, key: str, by: int = 1) -> int:
        io = self._io.get()
        return await io.incr(key, by) if io is not None else 0

    async def compare_and_set(self, key: str, expected: Any, new: Any) -> bool:
        io = self._io.get()
        return await io.compare_and_set(key, expected, new) if io is not None else False


global_state = _AtomicState(_global_io, "global_state")
object_state = _AtomicState(_object_io, "object_state")


@dataclass(frozen=True)
class ParamRef:
    """A deferred read of a run-wide param, produced by `param.get(key, default)` when it's
    called in a step/load DEFINITION — where the run (and its values) don't exist yet. The
    framework resolves it per run (e.g. a step's `concurrency`). It's plain data, so
    it rides the workflow boundary a `lambda` couldn't."""

    key: str
    default: Any = None


class _Param:
    """The one canonical param accessor. `param.get(key, default)`:
      • inside a step/load BODY (a run is active)       -> the resolved value
      • in a DEFINITION (@actor.method/load, no run yet) -> a deferred {@link ParamRef}
    Same call in both places — no `self.params.get` here / magic-string key there."""

    def get(self, key: str, default: Any = None) -> Any:
        p = _run_params.get()
        return p.get(key, default) if p is not None else ParamRef(key, default)


param = _Param()


@dataclass
class MethodRegistration:
    """One registered Method. `name` is what a caller dispatches; it defaults to the
    function's own name and only differs when the author overrides it.

    `takes` and `emits` are this Method's OWN signature (ADR 0023 §9): an Actor holds many
    Methods and `fetch` taking a URL to emit a page says nothing about `title` taking a page,
    so one Actor-level pair could only ever describe one of them. Either may be None — a
    Method that declares nothing still registers and stays dispatchable, it just advertises
    no schema."""

    fn: ActorFn
    name: str               # the dispatch name a caller uses
    fn_name: str            # local function name, e.g. "crawl"
    takes: Optional[type] = None   # the Unit type this Method consumes
    emits: Optional[type] = None   # the record type it emits (never a return annotation: §18)

    @property
    def description(self) -> str:
        """This Method's docstring, as a caller should read it.

        THE CATALOG HAD NO WORDS IN IT. A Method advertised a name and two JSON Schemas, so a
        caller reading the Actors page saw `run()` and a field table and had to guess what the
        Method DID — and an operator composing one Actor's Method into another's had nothing to
        compose from. The author had already written the answer; nothing carried it.

        The docstring, not a second `description=` argument. Authors write docstrings, the two
        would drift, and a field that duplicates one already on the page is one that ends up
        stale in exactly the cases where it matters.

        FIRST PARAGRAPH ONLY. A docstring's later paragraphs are for whoever maintains the
        Method; the first is what it is for, which is what a caller needs. `inspect.cleandoc`
        first, because a docstring indented to its `def` carries that indentation into every
        line but the first.
        """
        doc = inspect.cleandoc(self.fn.__doc__ or "")
        return doc.split("\n\n", 1)[0].strip()


class Actor:
    """The shared, mutable namespace the author's `self` refers to. One instance
    per session, created on the dedicated worker; holds non-serializable resources
    (self.browser, ...) across load -> steps -> close.

    A Method returns PLAIN JSON; there is no author-managed artifact API. The batch's
    input and result cross the handler boundary via a claim-check codec when large
    (runtime/handler/internal/codec), so authors never touch the object store directly."""

    def __init__(self) -> None:
        # Run-wide config from the dispatcher (reaches the author as self.params). The
        # host sets the real dict before load runs; the default keeps
        # `self.params.get(...)` safe even when nothing was passed.
        self.params: dict[str, Any] = {}
        # Run lineage stamped by the orchestrator, set by the host before load runs
        # (empty on the back-compat path). Defaulted here so `self.run_id` is always safe.
        self.run_id: str = ""
        self.idempotency_key: str = ""
        # How many times this session's resource has been rebuilt after a SessionLost
        # (0 on the first load). Lets an author react to a rebuild. The Go peer is
        # Session.Rebuilds.
        self.rebuilds: int = 0
        # Per-unit resume scratch (see _UnitState) — the accessor is global, the slot is bound
        # per in-flight Unit by the host. Read by a Unit that re-runs after an activity death.
        self.unit_state = unit_state
        # Cross-session, actor-NAME-scoped durable state with atomic ops (see _AtomicState) —
        # the accessor is global, the store is bound per batch by the host.
        self.global_state = global_state
        # Cross-session, actor-KEY-scoped durable state, same ops (ADR 0022). Empty and
        # per-dispatch unless the CALLER keyed the dispatch (`handle["acme.com"]`), because
        # without an idempotency_key the actor id is a fresh run/node coordinate.
        self.object_state = object_state
        # True when pushed records are persisted at push-time (an object store is configured).
        # A Method that RESUMES by skipping already-pushed work MUST gate that skip on this:
        # in the inline no-S3 mode a record is not durable until its Unit commits, so skipping
        # it on a resume would silently lose it. Set by the host.
        self.emit_durable = False


@dataclass
class SlotDeclaration:
    """One credential this actor asks for, by a name of ITS OWN choosing.

    A slot is NOT a secret name. The author writes `api_key`; the operator binds `api_key` to
    whichever of their secrets it should be — `stripe-prod`, `stripe-test`, a name you will never
    see. That indirection is what makes an actor written by somebody else usable at all: its author
    cannot know your inventory, and an actor that named one of your secrets directly would either
    be wrong everywhere but on the author's machine, or would be asking to be handed a credential
    you did not choose to give it.

    `description` is your sentence about what the credential is FOR, and it is the only thing the
    operator has to go on when they decide which secret to point at it. Write one."""

    name: str
    description: str = ""


class Slot:
    """A declared slot, and the handle you resolve it through.

        API_KEY = actor.slot("api_key", "the vendor key this actor calls with")

        @actor.load
        async def load(self):
            self.client = Vendor(await API_KEY.get(run=self.run_id))

    DECLARING IS THE POINT, and it happens at import — before a single Unit runs. The worker
    registers its slots as it registers itself, so an operator can see what this code will ask for
    on the Actors page BEFORE serving it, and a run that needs an unbound slot is refused at the
    start rather than inside `@actor.load`. Failing at load is failing after the Machines have been
    provisioned, the image pulled and the Session opened; on a fleet, that is a refusal that has
    already cost money.

    AND ASKING FOR A SLOT YOU DID NOT DECLARE IS REFUSED, not quietly satisfied. `Slot` is the only
    way to reach one, so an undeclared ask is a line you would have to write on purpose — but the
    orchestrator refuses it independently (403), because the declaration is only worth something if
    it is the whole list.

    `run=` is the RUN this resolution is for, and it is what makes the operator's audit trail
    answer "which actor read my key, and when, and for what". `self.run_id` is on your actor
    instance; pass it. Omitting it costs nothing but leaves that column empty."""

    def __init__(self, name: str, description: str = "", registry: "Optional[ActorRegistry]" = None) -> None:
        self.name = name
        self.description = description
        # THE REGISTRY, NOT A COPY OF ITS VERSION. `actor.slot(...)` runs at import, and the actor's
        # version is read out of actor.json later, in `serve()` — a Slot that snapshotted the
        # version at declaration time would resolve against the empty string forever.
        self._registry = registry

    @property
    def version(self) -> str:
        return getattr(self._registry, "version", "") or ""

    async def get(self, *, run: str = "", timeout: float = 5.0) -> str:
        """This slot's value, from whatever the operator bound to it. Raises `SecretUnavailable`
        (a `NonRetryableError`) when it cannot have it — see `actorkit.secrets`."""
        from actorkit import secrets

        return await secrets.slot(self.name, version=self.version, run=run, timeout=timeout)

    def get_sync(self, *, run: str = "", timeout: float = 5.0) -> str:
        """The blocking form, for an actor whose load is not async. Same rules, same failures."""
        from actorkit import secrets

        return secrets.slot_sync(self.name, version=self.version, run=run, timeout=timeout)

    def __repr__(self) -> str:  # pragma: no cover - debugging aid
        return f"Slot({self.name!r})"


class ActorRegistry:
    """Singleton collecting one author module's load / methods / close. `run()` (the serve
    path) reads this to build the actor host from the registered lifecycle."""

    def __init__(self) -> None:
        self.actor_name: str = "actor"
        self.actor_dir: Optional[Path] = None
        # THE RUNTIME'S SLOT, LEFT UNTYPED ON PURPOSE. `internals.manifest.load_manifest`
        # fills it with an ActorManifest; naming that class here — even under
        # `if TYPE_CHECKING:` — would be an sdk -> runtime edge in the source, which is the
        # one direction this package does not go (tests/test_sdk_arrow.py fails on it, and a
        # type-only import is exactly the kind that grows a real one later).
        self.manifest: Optional[Any] = None
        self.load_fn: Optional[ActorFn] = None
        self.close_fn: Optional[ActorFn] = None
        # Optional liveness probe: async (self) -> progress. When declared, the host calls it
        # after ANY Method failure and on the beat, and ENDS the Session if it raises/returns
        # False (ADR 0023 §20) — so resource death is DETECTED BY US, not by the author
        # hand-raising SessionLost. The author only declares how to probe THEIR resource; we own
        # the detect-and-end. Undeclared -> a plain failure isolates the unit; SessionLost stays
        # available as the imperative fast path.
        self.healthcheck_fn: Optional[ActorFn] = None
        # Every credential slot this actor declares, by name, in declaration order. Registered
        # with the orchestrator as the worker registers itself, so what this code will ASK FOR is
        # visible before it runs and a new slot in a new version reads as a diff.
        self.slots: "dict[str, SlotDeclaration]" = {}
        # Every declared Method, by dispatch name, in declaration order (ADR 0023 §9).
        # An actor with one Method is the old shape and needs no name on the wire; the
        # caller names the Method only when there is a choice to make.
        self.methods: "dict[str, MethodRegistration]" = {}
        # Actor version from actor.json. Part of the actor's identity / the
        # `{name}-{version}` queue the handler + orchestrator route to. Empty ->
        # the back-compat `{actor}-shared`.
        self.version: str = ""
        # Typed I/O declared on the @actor.defn class. The JSON Schemas the catalog stores
        # and the UI ports read are DERIVED from these (actorkit.schema), so there is no
        # hand-written ActorInput.json. None -> undeclared (DIY actor).
        self.input_type: Optional[type] = None
        self.output_type: Optional[type] = None
        self.params_type: Optional[type] = None
        # The runtime `self` class: the author's class mixed with the framework Actor
        # base (so methods get a typed self.* plus self.params/run_id/object_state). None
        # for the bare function surface -> the runtime instantiates Actor directly.
        self.actor_class: Optional[type] = None

    # @actor.defn  OR  @actor.defn(name="...")  — the class surface.
    def defn(
        self,
        cls: Optional[type] = None,
        *,
        name: Optional[str] = None,
    ) -> Any:
        """Declare a class-based actor: a run-wide `params` type (a dataclass or pydantic
        model) + the lifecycle as `@actor.load/method/close` methods. Schemas are DERIVED from
        the types — no ActorInput.json. In your methods `self` is an instance of YOUR class
        (typed) with framework state mixed in
        (`self.params`/`run_id`/`idempotency_key`/`object_state`).

            @actor.defn
            class Crawler:
                params = CrawlParams          # run-wide config, the one Actor-level type

                @actor.load
                async def setup(self): ...
                @actor.method(takes=CrawlSeed, emits=PageOut)
                async def crawl(self, batch, dataset): ...

        I/O TYPES BELONG TO THE METHOD (ADR 0023 §9), because each Method has its own
        signature and one Actor-level pair could only ever describe one of them. Class-level
        `input`/`output` attributes are still READ here, and nothing consumes them any more:
        they fed a build-time compat push to a schema registry that was never running, deleted
        by ADR 0027. They do not describe what any Method takes or emits, and the catalog does
        not read them.
        """

        def register(c: type) -> type:
            # The @actor.load/method/close functions already registered into this registry as
            # the class body executed; defn captures the declared types + builds the
            # runtime self class. `params` as a class attr (the type) is shadowed at
            # runtime by the instance's `self.params` dict the load activity sets — read
            # the class attr off the class, the values off the instance.
            self.input_type = getattr(c, "input", None)
            self.output_type = getattr(c, "output", None)
            self.params_type = getattr(c, "params", None)
            if name is not None:
                self.actor_name = name
            # ponytail: still fills the module singleton (one actor per file). Per-class
            # collection (multiple actors per file) is a later internal swap — the author
            # surface here is already the final one.
            self.actor_class = type(c.__name__, (c, Actor), {})
            return c

        return register(cls) if cls is not None else register

    # @actor.load  (bare) — the once-per-session resource open.
    def load(self, fn: Optional[ActorFn] = None) -> Any:
        def register(f: ActorFn) -> ActorFn:
            self.load_fn = f
            return f

        return register(fn) if fn is not None else register

    # @actor.close  (bare)
    def close(self, fn: ActorFn) -> ActorFn:
        self.close_fn = fn
        return fn

    # @actor.healthcheck  (bare) — periodic progress + resource-liveness probe.
    def healthcheck(self, fn: ActorFn) -> ActorFn:
        """Declare a probe `async (self) -> progress`. The runtime runs it PERIODICALLY (every
        few seconds, on the session heartbeat) AND after any Method failure. It does two things:

          • REPORT PROGRESS — whatever you return is surfaced on the Temporal heartbeat and
            logged, so a long-running session is observable (pages crawled, bytes fetched, …).
          • END THE SESSION — if the shared resource is gone, RAISE. The Session is over: the
            caller's scope raises and it reopens if it wants to, resuming from the cursor it
            holds, so at most one Batch is lost (ADR 0023 §20). It is NOT a reload — rebuilding
            in place would reset `self.*` under a running author, and a Session's promise has no
            third case: your state survives, or you get an exception. (Returning `False` also
            means dead.)

            @actor.healthcheck
            async def pulse(self):
                if not self.browser.is_connected():
                    raise RuntimeError("browser gone")      # -> this Session ends
                return {"pages": self.pages_crawled}         # -> progress

        A HEALTH CALL IS PRE-FLIGHT, NOT MID-SWEEP. A caller detecting a dependency outage does
        it by calling a health Method BEFORE dispatching the sweep — that is what several
        Methods are for (§14), and it is why there is no framework failure budget. Once a Batch
        is running, nothing counts failures on your behalf and decides the dependency is down.
        """
        self.healthcheck_fn = fn
        return fn

    # @actor.method  OR  @actor.method(name=...)
    def method(
        self,
        fn: Optional[ActorFn] = None,
        *,
        name: Optional[str] = None,
        takes: Optional[type] = None,
        emits: Optional[type] = None,
    ) -> Any:
        """Declare a dispatchable Method. It receives the whole Batch, the output Dataset, and
        YOU write the loop (ADR 0028 §2):

            @actor.method(takes=Target, emits=Page)
            async def crawl(self, batch, dataset):
                async for unit in batch:
                    await dataset.push(await fetch(unit.value))

        `await dataset.push(x)` appends one record to the output — it names no Unit (put the
        provenance you care about INSIDE the record) and returns nothing (a write failure
        surfaces at the next checkpoint or at Method exit, the Kafka-producer model). The
        framework commits a Unit as your loop moves past it — so a death mid-Batch resumes at
        the first Unit you had not finished. Bodies must be IDEMPOTENT: a Unit re-runs if
        death strikes between the work and its commit.

        Because push names no Unit, every granularity is the same call with no flag: one record
        per Unit, N per Unit, one per three Units, one for the whole Batch, or none at all
        (ADR 0028 §1). Run Units concurrently by taking them yourself (`batch.units`) and push
        from the spawned tasks — a parameter can be called from anywhere, which a `yield` could
        not. A push made with NO current Unit — before the loop, after it drains, or from such a
        spawned task — needs an explicit `key=`: `await dataset.push(summary, key="batch-summary")`.
        The framework reconciles it across an isolation re-invoke by that key, first-write-wins,
        never by its position; an unkeyed out-of-loop push raises (ADR 0028 §consequence 6).

        Declare as many Methods as the actor has jobs; the caller names the one it wants, and
        an actor with a single Method needs no name at the call site. Two Methods of one actor
        share the loaded resource and `self.*`, which is the point: `extract` sees the
        browser `crawl` opened.

        A METHOD IS ENTERED SEVERAL TIMES PER BATCH — once per isolated unit failure
        (ADR 0023 §13). Locals reset between entries; `self.*` does not. Anything you
        accumulate across units belongs on `self`, not in a local.

        `takes=` and `emits=` declare THIS Method's signature, and the catalog registers one
        operation per Method from them (ADR 0023 §9): `fetch` takes a URL and emits a page
        while `title` takes a page, so there is no one Actor-level pair that could describe
        both. `emits` is explicit rather than read off a return annotation because a Method
        returns nothing — it pushes to its Dataset (ADR 0028 §3). Both are optional: declare
        neither and the Method
        still registers and dispatches, it just advertises no schema. The Go peer is
        `a.Method("fetch", fetch, kontra.Takes(Target{}), kontra.Emits(Page{}))`.

        `name=` overrides the dispatch name when the function's own name is not the one
        callers should type."""

        def register(f: ActorFn) -> ActorFn:
            if inspect.isasyncgenfunction(f):
                # Caught at import rather than at dispatch: a generator Method would be built
                # and never iterated, which is a Batch that silently emits nothing.
                raise TypeError(
                    f"@actor.method {f.__name__} is a generator; a Method pushes with "
                    "`await dataset.push(x)` and returns nothing (ADR 0028 §3)"
                )
            dispatch_name = name or f.__name__
            clash = self.methods.get(dispatch_name)
            if clash is not None:
                raise TypeError(
                    f"@actor.method name {dispatch_name!r} declared twice "
                    f"({clash.fn_name}, {f.__name__}); give one of them name=\"...\""
                )
            self.methods[dispatch_name] = MethodRegistration(
                fn=f, name=dispatch_name, fn_name=f.__name__, takes=takes, emits=emits)
            return f

        return register(fn) if fn is not None else register

    # actor.slot("api_key", "…") — declare a credential this actor asks for.
    def slot(self, name: str, description: str = "") -> Slot:
        """Declare a credential slot and get the handle you read it through — see `Slot`.

            API_KEY = actor.slot("api_key", "the vendor key this actor calls with")

        Call it at module level, beside your Methods. It registers a NAME and a sentence and
        nothing else: the value never crosses Temporal, is never a `@param`, never rides in a
        **Batch** and is never a Method argument, because all three are workflow arguments and the
        payload codec is a claim-check rather than encryption (ADR 0007) — a hundred-byte credential
        rides inline in workflow history in the clear for the namespace's whole retention.

        Declaring the same slot twice is the same slot; declaring it twice with two different
        sentences RAISES, because the two lines disagree about what an operator is being asked to
        grant and silently keeping one of them makes the page lie about the other."""
        if not isinstance(name, str) or not name:
            raise TypeError("actor.slot needs a name — the credential's slot, e.g. \"api_key\"")
        said = (description or "").strip()
        existing = self.slots.get(name)
        if existing is not None:
            if said and existing.description and said != existing.description:
                raise TypeError(
                    f"actor.slot({name!r}) is declared twice with different descriptions "
                    f"({existing.description!r}, {said!r}); one of them is what the operator reads"
                )
            if said and not existing.description:
                existing.description = said
            return Slot(name, existing.description, self)
        self.slots[name] = SlotDeclaration(name=name, description=said)
        return Slot(name, said, self)

    def resolve_method(self, name: str = "") -> "Optional[MethodRegistration]":
        """The Method a dispatch means. Named -> that one; unnamed -> the only one.

        Unnamed against several Methods raises rather than picking declaration order: a
        caller who forgot the name would otherwise silently get whichever the author
        happened to write first."""
        if name:
            m = self.methods.get(name)
            if m is None:
                known = ", ".join(self.methods) or "none declared"
                raise TypeError(f"no @actor.method named {name!r} (have: {known})")
            return m
        if not self.methods:
            return None
        if len(self.methods) > 1:
            raise TypeError(
                f"{self.actor_name} declares {len(self.methods)} methods "
                f"({', '.join(self.methods)}); the dispatch must name one"
            )
        return next(iter(self.methods.values()))

    def serve(self) -> None:
        """Serve this actor as a Temporal activity worker. THE way to start an actor — put

            if __name__ == "__main__":
                actor.serve()

        at the bottom of your actor.py and launch it:

            kontra serve --actor <dir>          # or just: python3 actor.py

        The process becomes a Temporal activity worker polling `{name}-{version}-sessions`
        (ADR 0018) — it needs no sidecar and serves no HTTP. A Python actor is run by Python;
        there is no CLI to start one. Dispatch and the workflow half are the Go handler's job
        (/handler), so the runtime wiring is imported lazily (here, not at module import).

        SERVE, not run. This call does not execute your actor's code — it boots a worker and
        blocks forever waiting to be given a Batch. `run` named the wrong direction, and the
        cost of that was concrete: it reads like the verb for "make this actor do the work",
        which is what a CALLER wants, and a caller who reaches for it inside `@workflow.defn`
        gets a blocking `asyncio.run` in a workflow sandbox. The caller's verb is a Method call,
        `catalog.actor(...).method(units)`. Prefect draws the same line with `flow.serve()`,
        and `serve()` is what this method has always called internally."""
        import json
        import sys
        from pathlib import Path

        from internals.temporal.host import serve

        # Resolve (name, version) from the actor.json beside this actor.py: they derive the
        # TASK QUEUE this worker polls, and the handler derives the same string from its
        # workflow. A mismatch is an actor that registers, polls nothing and looks idle. Read
        # directly — no manifest/jsonschema (build-time validation, not a runtime dependency).
        actor_dir = self.actor_dir or Path(sys.argv[0]).resolve().parent
        self.actor_dir = actor_dir
        aj = actor_dir / "actor.json"
        if aj.exists():
            m = json.loads(aj.read_text())
            self.actor_name = m.get("name") or self.actor_name
            self.version = m.get("version", self.version)
        elif self.actor_name == "actor":
            self.actor_name = actor_dir.name
        # No app_port: the actor is a Temporal activity worker now, not an HTTP app a sidecar
        # calls back into. Its address is a task queue, derived from the identity just read.
        serve(self)

    def run(self) -> None:
        """Deprecated alias for `actor.serve()`; kept one release (same policy as
        `checkpoint` → `unit_state`, ADR 0015). Every existing actor.py keeps working."""
        warnings.warn(
            "actor.run() is renamed to actor.serve() — it starts a worker, it does not run "
            "your code. To make a deployed actor do a batch, that is a Method call, "
            "actorkit.catalog.actor(name, version).method(units) from a workflow.",
            DeprecationWarning, stacklevel=2,
        )
        self.serve()


# The singleton authors import.
actor = ActorRegistry()
