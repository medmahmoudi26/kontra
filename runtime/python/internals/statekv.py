"""ActorStateKV — the per-actor durable state, tier 2 (ADR 0015, ADR 0018).

What lives here now is the per-unit resume scratch (`unit_state`, `{batch}-u{i}-ckpt`) and the
poison counter beside it (`{batch}-u{i}-kills`). Both describe a Unit that is still IN FLIGHT.

TIER 1 — THE COMMIT MAP — USED TO LIVE HERE TOO, AND MOVED (ADR 0060). Which Units finished rides
the activity heartbeat as a checkpoint, and what each one produced is a commit object in the unit
store. A cache with a TTL was the wrong owner for the one record a retry acts on: losing it either
re-ran finished work or skipped unfinished work, and nothing raised either way (ADR 0059). With it
went the `batch-owner` field and the batch-boundary TTL renewal (`touch`), which existed only to
keep the commit map coherent and alive; and `drop`, whose only caller was that owner guard.

Why `unit_state` stays here rather than riding the heartbeat with the checkpoint:

  - `RecordHeartbeat` *panics* on encode failure rather than degrading, and `unit_state` is
    author-controlled opaque JSON — a crawl frontier is exactly the shape that outgrows a
    payload limit and would kill the activity. The checkpoint is a range set precisely so that
    it never grows that way.
  - Its move off Redis (with `global_state`/`object_state`, which live in another store) is the
    hardening Phase 9 decision, not this one.

## Layout

One Redis HASH per actor id, one field per key:

    kontra-actor:{actor_id}  ->  { "<batch>-u0-ckpt": …, "<batch>-u3-kills": … }

A hash rather than a key per field because the TTL is what makes this safe to leave behind, and
`EXPIRE` applies to the whole hash — so one `EXPIRE` per write slides the whole actor's state
forward instead of needing a touch per field.

Values are JSON. `None` is a real stored value (an author can store `null` in `unit_state`), so
absence is signalled by the field being missing, never by a sentinel.
"""

from __future__ import annotations

import json
import os
from typing import Any, Optional

# 24 h. Long enough to outlive any retry budget (MaxAttempts × StartToClose), so an in-flight
# Unit's scratch is still there for the attempt that re-runs it; short enough that abandoned runs
# do not accumulate in Redis forever.
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

        # Optional; set with `requirepass` by the VPC overlay. Unset means no auth, which is
        # the loopback and install case. See redis_kv.py for why this exists.
        return aioredis.Redis(host=host or "localhost", port=int(port or 6379), db=0,
                              password=os.environ.get("KONTRA_REDIS_PASSWORD") or None)

    return _factory


def state_kv(actor_id: str, ttl_s: Optional[int] = None) -> ActorStateKV:
    return ActorStateKV(redis_client_factory(), actor_id, ttl_s or STATE_TTL_S)
