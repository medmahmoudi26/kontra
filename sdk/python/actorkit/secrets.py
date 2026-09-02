"""Fetch this actor's OWN secret, at load, authenticated as itself.

    from actorkit import secrets

    @actor.load
    async def load(self):
        self.client = Shodan(await secrets.get("shodan-key"))

── WHY AN ACTOR ASKS INSTEAD OF BEING TOLD ────────────────────────────────────────────────────

A credential is never handed to an actor. Not as a `@param`, not in a **Batch**, not as a Method
argument — because all three are workflow arguments, and the payload codec is a CLAIM-CHECK, NOT
ENCRYPTION (ADR 0007): anything under 128 KiB rides inline in workflow history in the clear for the
namespace's whole retention, and a credential is a hundred bytes. There is no size at which a
secret is safely an argument, because the mechanism that would protect a large one is offload, not
encryption. The same reasoning `hitl.redact` applies to an ask applies here to everything the
caller could hand over.

So the value never crosses Temporal at all. The actor asks the orchestrator directly, over HTTP,
and gets back one secret: its own. The `owner` recorded when the operator created it is what makes
"its own" mean something (`backend/src/secrets/store.ts`) — an actor cannot fetch an operator
secret, and cannot fetch another actor's.

── WHO THIS WORKER IS ─────────────────────────────────────────────────────────────────────────

`KONTRA_ACTOR_TOKEN`, set in the worker's environment by whoever served it. The console mints one
when it serves an actor (`backend/src/actorControl.ts`); a worker started by hand gets one
from `POST /api/secrets/identity`. It is signed by the orchestrator and names ONE actor, so it
cannot be edited into somebody else's identity — and it is not itself a credential: it resolves
what this actor owns and nothing more.

── TWO THINGS THIS DELIBERATELY DOES NOT DO ───────────────────────────────────────────────────

**It does not cache.** A rotation has to take effect, and the store's whole recovery story from a
leak is "rotate, then revoke". A process-lifetime cache would keep a revoked value alive in memory
on every worker that had ever loaded it — which is precisely the state revocation exists to end.
`@actor.load` runs once per **Session**, so the cost is one call per session, not per unit.

**It does not retry.** A failure here is `SecretUnavailable`, which is a `NonRetryableError`: the
unit isolates immediately, with a sentence. MEASURED, and it is why: one bad credential in a loop
that retried produced 202 authentication POSTs and a green empty run — the load kept failing, the
host kept reopening, and nothing anywhere said "the credential is wrong". A missing identity, a
revoked secret and a name that does not exist are all permanent until a human acts, so they fail
once, loudly.

NOTHING HERE LOGS THE VALUE, and every message this module raises names the SECRET, the reason and
what to do — never what came back.
"""

from __future__ import annotations

import asyncio
import json
import os
import urllib.error
import urllib.request

from actorkit.retry import NonRetryableError

__all__ = ["get", "get_sync", "slot", "slot_sync", "SecretUnavailable"]

#: The worker's identity, set by whoever served this actor. Named once, here and in
#: `backend/src/secrets/identity.ts:ACTOR_TOKEN_VAR`.
ACTOR_TOKEN_VAR = "KONTRA_ACTOR_TOKEN"


class SecretUnavailable(NonRetryableError):
    """This actor cannot have that secret, and no amount of retrying will change it.

    A `NonRetryableError` on purpose — see the module docstring. The message says which secret and
    why; it never carries a value, and there is no `value` attribute to print by accident.
    """


async def get(name: str, *, version: int | None = None, timeout: float = 5.0) -> str:
    """This actor's own secret, by name. Raises `SecretUnavailable` if it cannot have it.

    `version` pins a version; the default is whatever is current, so a rotation reaches the next
    Session without touching the actor's code.

    Runs the blocking call in a thread: `@actor.load` is async and holds the event loop that the
    host's heartbeat runs on, so a slow orchestrator must not stall it.
    """
    return await asyncio.to_thread(get_sync, name, version=version, timeout=timeout)


def get_sync(name: str, *, version: int | None = None, timeout: float = 5.0) -> str:
    """The blocking form, for an actor whose load is not async. Same rules, same failures."""
    url = (os.environ.get("KONTRA_ORCHESTRATOR_URL") or "").rstrip("/")
    if not url:
        raise SecretUnavailable(
            f"cannot resolve secret {name!r}: this worker has no KONTRA_ORCHESTRATOR_URL, so there "
            "is no secret store to ask. Serve it from the console, or set the variable."
        )
    token = os.environ.get(ACTOR_TOKEN_VAR)
    if not token:
        raise SecretUnavailable(
            f"cannot resolve secret {name!r}: this worker has no identity ({ACTOR_TOKEN_VAR} is "
            "unset), so it cannot prove which actor it is. Serve it from the console, which mints "
            "one, or mint one with POST /api/secrets/identity."
        )

    body: dict[str, object] = {"name": name}
    if version is not None:
        body["version"] = version
    req = urllib.request.Request(
        f"{url}/api/secrets/resolve",
        data=json.dumps(body).encode("utf-8"),
        headers={"Content-Type": "application/json", "Authorization": f"Bearer {token}"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            payload = json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as e:
        raise SecretUnavailable(_why(name, e.code)) from None
    except (urllib.error.URLError, OSError) as e:
        # `from None` on every raise in this function: an exception chain prints the request, and
        # a urllib request object's repr carries its headers — which is where the identity token is.
        raise SecretUnavailable(
            f"cannot resolve secret {name!r}: the orchestrator at {url} is unreachable ({e.__class__.__name__})"
        ) from None

    value = payload.get("value")
    if not isinstance(value, str):
        raise SecretUnavailable(f"the secret store answered without a value for {name!r}")
    return value


def _why(name: str, code: int) -> str:
    """The four refusals, each with the act that fixes it. A status code alone sends an author to
    read the orchestrator's source; this sends them to the one thing that changes the outcome."""
    if code == 401:
        return (
            f"cannot resolve secret {name!r}: this worker's identity was rejected or has expired — "
            "serve the actor again to mint a fresh one"
        )
    if code == 403:
        return (
            f"cannot resolve secret {name!r}: it does not belong to this actor. An actor may only "
            "fetch a secret created with it as the owner; an operator secret is resolved by a "
            "worker at the last hop and is never fetched over HTTP."
        )
    if code == 404:
        return f"cannot resolve secret {name!r}: there is no secret by that name — create it in Settings"
    if code == 410:
        return (
            f"cannot resolve secret {name!r}: the version it resolves to has been REVOKED. Write a "
            "new value in Settings; nothing this worker does will bring the old one back."
        )
    return f"cannot resolve secret {name!r}: the secret store answered {code}"


# ─────────────────────────────────────────────────────────────────────────────────────────────
# SLOTS — the half that works when the actor's author does not know your secret names
# ─────────────────────────────────────────────────────────────────────────────────────────────
#
# `get(name)` above resolves a secret this actor OWNS, by the operator's own name for it. That is
# the right shape for an actor you wrote, deployed on your own appliance, against secrets you
# created. It is the wrong shape for an actor written by somebody else, and the reason is not
# security theatre: a third-party author cannot know your inventory, so a hard-coded name is either
# wrong on every installation but theirs, or is a request to be handed a credential you can name
# and did not choose to give them.
#
# So an actor DECLARES a slot — `api_key` — and the operator BINDS it: `api_key -> stripe-prod`.
# The functions below are what an author reaches through `actor.slot(...)`; they are exported for
# the same reason `get` is, and almost nobody should call them directly.
#
# THREE THINGS THIS DOES THAT `get` DOES NOT:
#   • it names the actor VERSION, because a declaration is per version — a slot added in 0.3.0 is a
#     visible diff against 0.2.0 rather than a runtime surprise, and that only holds if a
#     resolution is checked against the version doing the asking.
#   • it names the RUN, so the operator's audit trail answers "which actor read my key, and when,
#     and for what". It is the only field here whose absence costs nothing but an empty column.
#   • it never learns the secret's NAME. The answer is the slot and the value; which of the
#     operator's credentials was behind it stays the operator's business, which is the whole
#     property the indirection exists to keep.


async def slot(name: str, *, version: str, run: str = "", timeout: float = 5.0) -> str:
    """Resolve a declared slot. Prefer `actor.slot("api_key").get(run=self.run_id)`.

    Runs the blocking call in a thread, for `get`'s reason: `@actor.load` is async and holds the
    event loop the host's heartbeat runs on."""
    return await asyncio.to_thread(slot_sync, name, version=version, run=run, timeout=timeout)


def slot_sync(name: str, *, version: str, run: str = "", timeout: float = 5.0) -> str:
    """The blocking form. Same rules, same failures, same silence about the value."""
    url = (os.environ.get("KONTRA_ORCHESTRATOR_URL") or "").rstrip("/")
    if not url:
        raise SecretUnavailable(
            f"cannot resolve slot {name!r}: this worker has no KONTRA_ORCHESTRATOR_URL, so there "
            "is no secret store to ask. Serve it from the console, or set the variable."
        )
    token = os.environ.get(ACTOR_TOKEN_VAR)
    if not token:
        raise SecretUnavailable(
            f"cannot resolve slot {name!r}: this worker has no identity ({ACTOR_TOKEN_VAR} is "
            "unset), so it cannot prove which actor it is. Serve it from the console, which mints "
            "one, or mint one with POST /api/secrets/identity."
        )
    if not version:
        # REFUSED HERE rather than sent as "" and refused there, because the fix is local and
        # specific: the version comes from actor.json, and an actor serving without one is a
        # deployment nobody can pin, not a credential problem.
        raise SecretUnavailable(
            f"cannot resolve slot {name!r}: this actor has no version. A slot is declared per "
            "version, so actor.json must name one."
        )

    body = {"slot": name, "version": version}
    if run:
        body["run"] = run
    req = urllib.request.Request(
        f"{url}/api/slots/resolve",
        data=json.dumps(body).encode("utf-8"),
        headers={"Content-Type": "application/json", "Authorization": f"Bearer {token}"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            payload = json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as e:
        raise SecretUnavailable(_why_slot(name, version, e.code)) from None
    except (urllib.error.URLError, OSError) as e:
        # `from None` for `get_sync`'s reason: an exception chain prints the request, and a urllib
        # request object's repr carries its headers — which is where the identity token is.
        raise SecretUnavailable(
            f"cannot resolve slot {name!r}: the orchestrator at {url} is unreachable "
            f"({e.__class__.__name__})"
        ) from None

    value = payload.get("value")
    if not isinstance(value, str):
        raise SecretUnavailable(f"the secret store answered without a value for slot {name!r}")
    return value


def declare(url: str, actor_name: str, version: str, slots: list, *, timeout: float = 5.0) -> int:
    """POST what this `(actor, version)` asks for to `{url}/api/slots/declare`.

    Called by the worker as it registers itself (`internals/catalog.py`). It carries NAMES and
    SENTENCES — there is nothing here a value could ride in — and what it buys is the property the
    whole slice exists for: the credentials an actor will ask for are visible before it runs.

    Returns the HTTP status. Raises whatever urllib raises; the caller is best-effort and prints."""
    body = json.dumps({"actor": actor_name, "version": version, "slots": slots}).encode("utf-8")
    req = urllib.request.Request(
        f"{url.rstrip('/')}/api/slots/declare",
        data=body,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return int(resp.status)


def _why_slot(name: str, version: str, code: int) -> str:
    """The refusals a slot has that a named secret does not, each with the act that fixes it.

    THE 403 AND THE 409 ARE THE TWO THAT MATTER and they are easy to collapse into one "no". They
    are opposite problems: 403 is the ACTOR asking for something it never declared — a code
    problem, fixed by declaring it — and 409 is the OPERATOR not having granted it — a console
    problem, fixed by binding it. One message over both sends half of everybody to the wrong file.
    """
    if code == 401:
        return (
            f"cannot resolve slot {name!r}: this worker's identity was rejected or has expired — "
            "serve the actor again to mint a fresh one"
        )
    if code == 403:
        return (
            f"cannot resolve slot {name!r}: this actor@{version} does not DECLARE it. Declare it "
            f"with actor.slot({name!r}) and re-serve — an actor may only ask for credentials it "
            "declared, so that what it will ask for is visible before it runs."
        )
    if code == 409:
        return (
            f"cannot resolve slot {name!r}: it is declared and the operator has BOUND NOTHING to "
            "it. Bind it to one of your secrets in Settings; nothing this worker does will fill it."
        )
    if code == 404:
        return (
            f"cannot resolve slot {name!r}: it is bound to a secret that no longer exists — "
            "re-create that secret, or bind the slot to another one."
        )
    if code == 410:
        return (
            f"cannot resolve slot {name!r}: the secret it is bound to has had every version "
            "REVOKED. Write a new value in Settings; nothing this worker does will bring it back."
        )
    return f"cannot resolve slot {name!r}: the secret store answered {code}"
