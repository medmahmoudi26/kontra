"""ActorStateKV — the per-actor durable state, tiers 1 and 2 (ADR 0015, ADR 0018).

This is tier 1 and tier 2 of the three-tier model: the per-(step,unit) commit map, the per-unit
resume scratch (`unit_state`), and the cross-session `global_state`. ADR 0018 changed the client
these ride on but **not the tiers**: same keys, same Redis, same 24 h TTL.

Why they stay in Redis rather than riding Temporal heartbeat details, which was the original
plan and is struck in the ADR:

  - `RecordHeartbeat` *panics* on encode failure rather than degrading, and `unit_state` is
    author-controlled opaque JSON — a crawl frontier is exactly the shape that outgrows a
    payload limit and would kill the activity.
  - Heartbeat details are throttled to one per `0.8 × HeartbeatTimeout` window and are not
    flushed on SIGKILL, so a hard kill loses up to a minute of commits.
  - Details survive *attempts*, not *executions*. The Redis map is keyed by actor id with a
    24 h TTL, so it survives a re-dispatch on the same idempotency key and `recover_run`.

And the failure mode is not degradation: a lost commit **duplicates**. Unit blob keys end in
`sha256(data)`, and the downstream node is handed every key it has not seen — so a re-run whose
records are not byte-identical writes a second blob and the child processes both.

## Layout

One Redis HASH per actor id, one field per key:

    kontra-actor:{actor_id}  ->  { "u0": …, "u0-ckpt": …, "s-index": …, "s-name": … }

A hash rather than a key per field because the TTL is what makes this safe to leave behind, and
`EXPIRE` applies to the whole hash — so one `EXPIRE` per write slides the whole actor's state
forward instead of needing a touch per field, so renewal is a single `EXPIRE` rather than a
re-write of every live key.

Values are JSON. `None` is a real stored value (a unit can commit `null`), so absence is
signalled by the field being missing, never by a sentinel.
"""

from __future__ import annotations

import json
import os
from typing import Any, Optional

# 24 h. Long enough to outlive any retry budget (MaxAttempts × StartToClose) and a same-day
# `recover_run`; short enough that abandoned runs do not accumulate in Redis forever.
STATE_TTL_S = 24 * 60 * 60

_PREFIX = "kontra-actor"


def actor_key(actor_id: str) -> str:
    return f"{_PREFIX}:{actor_id}"


class ActorStateKV:
    """Per-actor durable state over plain Redis.

    The client is built lazily from a factory, so constructing one costs nothing — the actor
    host builds it per activity execution and most of those touch state only once work starts.
    """

    def __init__(self, client_factory: Any, actor_id: str, ttl_s: int = STATE_TTL_S) -> None:
        self._make = client_factory
        self._key = actor_key(actor_id)
        self._ttl = ttl_s
        self._c: Any = None

    def _client(self) -> Any:
        if self._c is None:
            self._c = self._make()
        return self._c

    async def get(self, field: str, default: Any = None) -> Any:
        c = self._client()
        raw = await c.hget(self._key, field)
        if raw is None:
            return default
        return json.loads(raw)

    async def set(self, field: str, value: Any) -> None:
        c = self._client()
        # HSET then EXPIRE, not a pipeline: the write must land even if the TTL call fails, and
        # a state entry that outlives its TTL is recoverable while one that never lands is not.
        await c.hset(self._key, field, json.dumps(value))
        await c.expire(self._key, self._ttl)

    async def delete(self, field: str) -> bool:
        c = self._client()
        return bool(await c.hdel(self._key, field))

    async def touch(self) -> None:
        """Slide the whole actor's TTL forward. One call, because the TTL is on the hash.

        One call, because the TTL is on the hash rather than on each field.
        """
        await self._client().expire(self._key, self._ttl)

    async def drop(self) -> None:
        """Remove everything for this actor id. Called when a batch completes: the commit map
        exists to make a RETRY skip finished units, and a finished batch has no retry."""
        await self._client().delete(self._key)


def redis_client_factory() -> Any:
    """The shared Redis the fleet already points at.

    `KONTRA_REDIS_HOST` is `host:port`; the machine install and the worker entrypoint both set
    it to the Controller, which is what makes a whole fleet share one state store instead of
    each Worker being an island.
    """
    hostport = os.environ.get("KONTRA_REDIS_HOST", "localhost:6379")
    host, _, port = hostport.partition(":")

    def _factory() -> Any:
        import redis.asyncio as aioredis

        return aioredis.Redis(host=host or "localhost", port=int(port or 6379), db=0)

    return _factory


def state_kv(actor_id: str, ttl_s: Optional[int] = None) -> ActorStateKV:
    return ActorStateKV(redis_client_factory(), actor_id, ttl_s or STATE_TTL_S)
