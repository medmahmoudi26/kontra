"""global_state — tier 3 of the three-tier state model (ADR 0015).

No store here, and none by construction: the atomics are written against the `EtagKV` seam, so
the backing store is an implementation detail. The real adapter is `internals.redis_kv`; tests
inject a fake. Swapping the implementation has never required touching a line of this file.

Cross-SESSION (cross-actor-id), actor-NAME-scoped durable state. It CANNOT use the per-actor
state manager (that state is scoped to one actor id and unreadable by another), so it goes
through a store shared across actor ids. Keys are namespaced `kontra-global:{actor}:{key}`
— by actor NAME, not name+version, so a version bump deliberately SHARES the state (the dedupe-SET
flagship would be useless if it reset each version).

The atomic ops (add_to_set / incr / compare_and_set) are the point: naive get/set is
last-write-wins and loses concurrent updates. They are built here as optimistic-CAS retry
loops over a small `EtagKV` seam, so the concurrency logic is unit-tested with a fake KV and
the real wiring stays a thin adapter.
"""

from __future__ import annotations

import json
from typing import Any, Optional, Protocol, Tuple

# Bounded optimistic-retry budget for a contended atomic op before giving up loud (a real
# hot key under this much contention wants a different data structure, not silent staleness).
_CAS_RETRIES = 16


class EtagKV(Protocol):
    """The minimal ETag key/value seam global_state's atomics need. The real impl wraps Redis
    (`internals.redis_kv`); tests inject an in-memory fake."""

    async def get(self, key: str) -> Tuple[Optional[bytes], str]:
        """Return (data, etag). data is None when absent; etag is the read version ("" if none)."""

    async def put(self, key: str, data: bytes) -> None:
        """Unconditional write (last-write-wins) — for set()."""

    async def try_save(self, key: str, data: bytes, etag: str) -> bool:
        """Compare-and-set: write only if the store's current etag still matches `etag`. Return
        True on success, False on an ETag conflict (a concurrent writer won — caller retries)."""


def global_prefix(actor_name: str) -> str:
    """Tier 3 namespace: actor NAME, not name+version, so a version bump deliberately SHARES."""
    return f"kontra-global:{actor_name or 'actor'}:"


def object_prefix(actor_name: str, object_key: str) -> str:
    """Tier 4 namespace (ADR 0022): the actor's KEY — its `idempotency_key`, which is what the
    handler derives the actor id from. The actor NAME stays in the namespace because the key is
    the CALLER's string: two different actors both keyed "acme.com" must not collide. Version is
    out for the same reason as tier 3 — a bump must not orphan the key's accumulated state."""
    return f"kontra-object:{actor_name or 'actor'}:{object_key}:"


class GlobalStore:
    """The author-facing operations for BOTH cross-session tiers over an EtagKV. The tier is
    entirely the namespace: name-scoped `global_state` by default, key-scoped `object_state`
    when the caller passes a `prefix`. The atomics below are identical for both, which is the
    point — a per-key dedupe set and a per-actor dedupe set are the same data structure with
    different blast radius."""

    def __init__(self, kv: EtagKV, actor_name: str = "", *, prefix: str | None = None) -> None:
        self._kv = kv
        self._prefix = prefix if prefix is not None else global_prefix(actor_name)

    def _k(self, key: str) -> str:
        return self._prefix + key

    async def get(self, key: str) -> Any:
        data, _ = await self._kv.get(self._k(key))
        return json.loads(data) if data else None

    async def set(self, key: str, value: Any) -> None:
        """Last-write-wins set. Use the atomic ops below when concurrent writers race a key."""
        await self._kv.put(self._k(key), json.dumps(value).encode())

    async def add_to_set(self, key: str, member: Any) -> bool:
        """Atomically add member to the SET at key (the dedupe flagship). Returns True if it was
        newly added, False if it was already present. Never loses a concurrent add (ETag CAS)."""
        k = self._k(key)
        for _ in range(_CAS_RETRIES):
            data, etag = await self._kv.get(k)
            members = json.loads(data) if data else []
            if member in members:
                return False
            if await self._kv.try_save(k, json.dumps([*members, member]).encode(), etag):
                return True
        raise RuntimeError(f"add_to_set({self._k(key)!r}): ETag contention exhausted")

    async def incr(self, key: str, by: int = 1) -> int:
        """Atomically add `by` to the integer counter at key; returns the new value. Never loses
        a concurrent increment (ETag CAS)."""
        k = self._k(key)
        for _ in range(_CAS_RETRIES):
            data, etag = await self._kv.get(k)
            n = (json.loads(data) if data else 0) + by
            if await self._kv.try_save(k, json.dumps(n).encode(), etag):
                return n
        raise RuntimeError(f"incr({self._k(key)!r}): ETag contention exhausted")

    async def compare_and_set(self, key: str, expected: Any, new: Any) -> bool:
        """Set key to `new` only if its current value equals `expected`. Returns True on success,
        False if the current value differed or a concurrent writer won the ETag race."""
        k = self._k(key)
        data, etag = await self._kv.get(k)
        current = json.loads(data) if data else None
        if current != expected:
            return False
        return await self._kv.try_save(k, json.dumps(new).encode(), etag)
