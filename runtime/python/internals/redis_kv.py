"""RedisEtagKV — the real implementation of global_state's `EtagKV` seam (ADR 0015, ADR 0018).

The tier, the key scheme and the semantics are as ADR 0015 defined them: the same keys, the same
Redis, the same first-write-wins-on-create rule. `GlobalStore` knows none of it — its CAS retry
loops are tested against a fake.

Why Lua rather than WATCH/MULTI: the compare-and-set must be atomic *on the server*. A
read-then-write from the client is exactly the lost-update race the atomics exist to prevent,
and WATCH/MULTI costs an extra round trip per attempt while still needing retry handling for
the aborted-transaction case. A single `EVALSHA` is one hop and cannot interleave.

Redis has no native ETag, so a version counter supplies one. Each key is a hash:

    key -> { data: <bytes>, ver: <monotonic int as string> }

`ver` is the etag. It is bumped by every write, including the unconditional `put`, so a CAS
holder that missed a last-write-wins `set` correctly loses its race instead of silently
clobbering it.
"""

from __future__ import annotations

import os
from typing import Any, Optional, Tuple

# Compare-and-set. Returns 1 on success, 0 on conflict.
#
#   KEYS[1] = key
#   ARGV[1] = data to write
#   ARGV[2] = expected etag; "" means "this key must not exist yet"
#
# The empty-etag branch is first-write-wins on create: a concurrent creator is a CONFLICT, not
# a silent overwrite. Dropping that would let two actors both believe they created a dedupe set.
_CAS_LUA = """
local cur = redis.call('HGET', KEYS[1], 'ver')
if ARGV[2] == '' then
  if cur then return 0 end
  redis.call('HSET', KEYS[1], 'data', ARGV[1], 'ver', '1')
  return 1
end
if not cur or cur ~= ARGV[2] then return 0 end
redis.call('HSET', KEYS[1], 'data', ARGV[1], 'ver', tostring(tonumber(cur) + 1))
return 1
"""

# Unconditional write. Bumps ver so any in-flight CAS against the old version loses.
_PUT_LUA = """
redis.call('HSET', KEYS[1], 'data', ARGV[1])
redis.call('HINCRBY', KEYS[1], 'ver', 1)
return 1
"""


class RedisEtagKV:
    """The real EtagKV over Redis. `GlobalStore` cannot tell it from the in-memory fake.

    The client is built from a factory on first use, so an actor that never touches
    `global_state` opens no connection.
    """

    def __init__(self, client_factory: Any) -> None:
        self._make = client_factory
        self._c: Any = None
        self._cas: Any = None
        self._put: Any = None

    def _client(self) -> Any:
        if self._c is None:
            self._c = self._make()
            # register_script defers loading until first call, so this stays construction-cheap
            # and survives a Redis restart (redis-py falls back to EVAL on NOSCRIPT).
            self._cas = self._c.register_script(_CAS_LUA)
            self._put = self._c.register_script(_PUT_LUA)
        return self._c

    async def get(self, key: str) -> Tuple[Optional[bytes], str]:
        self._client()
        data, ver = await self._c.hmget(key, ["data", "ver"])
        if data is not None and not isinstance(data, bytes):
            data = data.encode()
        if isinstance(ver, bytes):
            ver = ver.decode()
        return (data or None), (ver or "")

    async def put(self, key: str, data: bytes) -> None:
        self._client()
        await self._put(keys=[key], args=[data])

    async def try_save(self, key: str, data: bytes, etag: str) -> bool:
        self._client()
        return bool(await self._cas(keys=[key], args=[data, etag or ""]))


def redis_kv_from_env() -> RedisEtagKV:
    """Wire `global_state` to the shared Redis the fleet already points at.

    `KONTRA_REDIS_HOST` is `host:port` (the worker entrypoint sets it to the controller, and a
    non-localhost value is what makes a worker skip its bundled Redis). Defaults match the
    self-contained worker.
    """
    hostport = os.environ.get("KONTRA_REDIS_HOST", "localhost:6379")
    host, _, port = hostport.partition(":")
    # ── THE PASSWORD IS OPTIONAL, AND WHERE IT MATTERS IS THE VPC ─────────────────────────────
    #
    # On the default stack Redis publishes on loopback and loopback IS the control (ADR 0056). The
    # VPC overlay publishes it to the fleet network so a Machine can reach the state store — and at
    # that point "anything on this VPC" can read every actor's state and every global_state entry,
    # with no credential at all. `docker-compose.vpc.yml` therefore sets `requirepass` and this
    # variable together, both `${VAR:?}`.
    #
    # UNSET MEANS NO AUTH, which is today's behaviour unchanged — so a loopback install and the
    # embedded appliance store keep working without one.
    password = os.environ.get("KONTRA_REDIS_PASSWORD") or None

    def _factory() -> Any:
        import redis.asyncio as aioredis

        return aioredis.Redis(host=host or "localhost", port=int(port or 6379), db=0,
                              password=password)

    return RedisEtagKV(_factory)
