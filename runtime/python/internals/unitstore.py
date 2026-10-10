"""Per-unit blob store for the actor host — the actor side of the claim-check data plane.

Each pushed record is written to the object store THE MOMENT it is pushed (`blob_key`, keyed by
content sha), and what a Unit commits holds only small refs `{"$ref": {key, size, sha256}}` — so
neither actor memory nor any state store ever carries batch payloads. A node's whole output is an S3
PREFIX a query engine can scan directly (`read_json('s3://…/units/run={run}/**/*.json')`).

A Unit's COMMIT is an object here too (ADR 0060): one per finished Unit under `commit_prefix`,
holding its refs or its isolation error, written before the heartbeat that reports it. The
heartbeat's checkpoint says WHICH Units finished and names that prefix; this says what they
produced. It used to be a field in the actor's Redis hash, which was one TTL or one eviction away
from a retry re-running finished work or skipping unfinished work with nothing raised. The prefix
is keyed by the INSTANCE (`actor_id`), not the dispatch, so a fresh execution of the same Batch on
the same instance can LIST it and fold back what an earlier execution finished (`list_commits`).

Unconfigured (`KONTRA_S3_ENDPOINT` unset) -> None. S3 IS MANDATORY FOR AN ACTOR THAT COMMITS
(ADR 0060, owner decision A8): `serve()` refuses to start an actor that declares a Method without
it, because without it a finished Unit's output is durable nowhere. `None` survives only as the
engine's in-process seam for tests that predate the in-memory store, where records are collected
inline and no commit object is written.

boto3 is sync — callers wrap writes in `asyncio.to_thread` so a PUT never stalls the
window's event loop.
"""

from __future__ import annotations

import datetime as _dt
import hashlib
import json
import os
import re
from typing import Any, Optional


class UnitStore:
    def __init__(self, endpoint: str, bucket: str, prefix: str,
                 access: str, secret: str, region: str) -> None:
        import boto3  # actor-image / [seaweed] extra dep, not a base SDK requirement

        self._s3 = boto3.client(
            "s3", endpoint_url=endpoint, region_name=region,
            aws_access_key_id=access, aws_secret_access_key=secret,
        )
        self.bucket = bucket
        self.prefix = prefix

    def ensure(self) -> None:
        """Fail fast: the bucket must exist (create it on first boot, like the Go side)."""
        try:
            self._s3.head_bucket(Bucket=self.bucket)
        except Exception:
            self._s3.create_bucket(Bucket=self.bucket)

    def put_subunit(self, run_id: str, node_id: str, i: int, record: Any, run_date: str = "") -> dict:
        """Write ONE pushed record (`await dataset.push(x)`) to its own blob under the
        partitioned key (see blob_key), wrapped as a 1-element JSON
        array so the streaming cursor's array-flattening consumer (orchestrator mapAndRoute)
        reads flat and sub-unit blobs uniformly. The sha256 of the CANONICAL record is the
        sub-unit id, so re-pushing the same record on a resume is an idempotent overwrite —
        records must be content-deterministic (no timestamps), the author-visible identity rule
        of ADR 0015. Returns the ref entry (its own sha256 == the id, the integrity anchor).

        The one write path since ADR 0028: a Method pushes records to its Dataset, so there is
        no whole-unit blob to write and nothing that only appears once a unit finishes."""
        data = json.dumps([record], sort_keys=True).encode()
        sha = hashlib.sha256(data).hexdigest()
        # Concatenated, deliberately, and NOT the CAS rule: `casstore.object_key` joins the
        # prefix as a path segment because a claim-check key is DERIVED from a digest
        # independently on both sides. A sub-unit key is CARRIED — it rides in the `$ref` below
        # and the reader (get_subunit, the orchestrator's resolveBatch) uses it verbatim — so the
        # writer's spelling round-trips whatever it is. Changing it is a separate decision with a
        # separate blast radius (control/orchestrator/src/data/parquet.ts builds `s3://<bucket>/` + this key)
        # and it belongs with prefix rows in shared/conformance/blobkey.json, which has none either.
        key = self.prefix + blob_key(run_date, _actor_name(), run_id, node_id, i, sha)
        self._s3.put_object(Bucket=self.bucket, Key=key, Body=data,
                            ContentType="application/json")
        return {"$ref": {"key": key, "size": len(data), "sha256": sha}}



    def get_subunit(self, key: str) -> Any:
        """Read back ONE record written by put_subunit, unwrapping the 1-element array it
        wraps records in (see put_subunit).

        This is the INGEST half of the blob plane, and it exists so a Method can be handed the
        output of another Method without the payload passing through the caller. The caller has
        no S3 credentials by design (`kontra.catalog` promises exactly that), but the actor
        does — it wrote these blobs — so resolution is symmetric with emission and costs the
        caller nothing.

        `key` is the FULL key as it appears in a `{"$ref": {...}}` entry, i.e. already
        prefixed; put_subunit returns it that way. Not content-addressed reading: these live in
        the hive `units/run=…` layout, which the CAS-only `kontra.fetch_blob` cannot reach.
        """
        body = self._s3.get_object(Bucket=self.bucket, Key=key)["Body"].read()
        rec = json.loads(body)
        # put_subunit writes `[record]`; tolerate a bare record so a hand-written or
        # externally-produced blob is not a hard failure.
        if isinstance(rec, list):
            if len(rec) != 1:
                raise ValueError(
                    f"sub-unit blob {key} holds {len(rec)} records; expected exactly 1")
            return rec[0]
        return rec

    # ---- per-Unit commit objects (ADR 0060) -----------------------------------------------------

    def commit_prefix(self, run_id: str, actor_id: str, batch_id: str) -> str:
        """This batch's commit prefix as this store spells it — the value a checkpoint carries as
        `manifest_ref`. Concatenated with the store prefix exactly as `put_subunit` does, for the
        same reason: the prefix is CARRIED (in the heartbeat) and read back verbatim."""
        return self.prefix + commit_prefix(_actor_name(), run_id, actor_id, batch_id)

    def put_commit(self, key: str, body: dict) -> None:
        """Write one Unit's commit object. SYNCHRONOUS ON PURPOSE: the beat that reports the Unit
        finished is sent only after this returns, so a checkpoint can never name a Unit whose
        outcome is not already in the store."""
        data = json.dumps(body, sort_keys=True, ensure_ascii=False).encode()
        self._s3.put_object(Bucket=self.bucket, Key=key, Body=data,
                            ContentType="application/json")

    def get_commit(self, key: str) -> Any:
        """Read one commit object back, or `None` when there is no object at `key`.

        ABSENCE IS RETURNED, NOT RAISED, so the caller decides what it means — and the engine
        decides it is loud (`CommitLost`): a Unit the checkpoint calls finished with nothing here
        is the store having lost something, never a Unit to quietly re-run. Every OTHER failure
        (a refused credential, a network error) raises as itself, because those are retryable and
        absence is not.
        """
        try:
            body = self._s3.get_object(Bucket=self.bucket, Key=key)["Body"].read()
        except Exception as e:  # noqa: BLE001 - narrowed to "not found" just below
            code = str(((getattr(e, "response", None) or {}).get("Error") or {}).get("Code", ""))
            if code in ("NoSuchKey", "404", "NotFound"):
                return None
            raise
        return json.loads(body)

    def list_commits(self, prefix: str) -> list[str]:
        """Every key under one batch's commit prefix, as the store returns them — what a FRESH
        execution of the batch resumes from (`commit_units` says which of them are Units).

        One LIST per first attempt, and it is usually empty: a batch nobody ran before has nothing
        here. Paginated, because a LIST page stops at 1,000 keys and a batch can hold more Units
        than that. A failure raises as itself — retryable, like every read that is not absence.
        """
        keys: list[str] = []
        pages = self._s3.get_paginator("list_objects_v2").paginate(Bucket=self.bucket, Prefix=prefix)
        for page in pages:
            keys.extend(o["Key"] for o in page.get("Contents", ()) or ())
        return keys


#: The only commit-object version this reader understands (shared/conformance/commit.json).
COMMIT_VERSION = 1


class CommitInvalid(ValueError):
    """A commit object that may not be folded into a resumed batch — see `decode_commit`."""


def commit_prefix(actor: str, run_id: str, actor_id: str, batch_id: str) -> str:
    """Where every commit object of one batch lives — byte-identical to Go's unitstore.CommitPrefix.

        commits/run={run}/actor={actor}/actor_id={actor_id}/batch={batch_id}/

    ITS OWN TOP-LEVEL PREFIX, NOT A CORNER OF `units/`. Everything under `units/run=<id>/` that
    ends in `.json` is a ROW to the live row tail (`control/orchestrator/src/rowTail.ts` counts
    them) and to a DuckDB glob over the run, so a commit object there would inflate every run's
    row count by its Unit count. `run=` still leads, for the measured reason `blob_key` gives:
    an object store prunes a LIST only by literal prefix, and whatever sweeps this tree asks by run.

    KEYED BY THE INSTANCE, NOT THE DISPATCH. `actor_id` is the id the host keys the live instance by
    — the idempotency key, else the Session, else run and node joined — and it replaced the node's
    `shard=` here. A keyed re-dispatch carries a fresh node id, so a node-keyed prefix could never be
    found by the execution that re-runs the same Batch on the same key; this one is, which is what
    makes cross-execution resume a LIST (owner decision A7). `run=` still scopes it: a Unit's `out`
    is refs into this run's `units/` prefix, and another run's would not be this run's rows.

    The batch id is the batch's CONTENT HASH, so every attempt and every execution of one Batch on
    one instance lands on one prefix, and two batches never share one.
    shared/conformance/commit.json pins it.
    """
    return (
        f"commits/run={_part_safe(run_id or 'run')}"
        f"/actor={_part_safe(actor or 'unknown')}"
        f"/actor_id={_part_safe(actor_id or 'unknown')}"
        f"/batch={_part_safe(batch_id or 'batch')}/"
    )


def commit_key(prefix: str, unit: int) -> str:
    """Unit `unit`'s commit object under a batch's prefix. Five digits is a floor, not a width:
    an index past 99999 widens rather than truncating into a neighbour's key."""
    return f"{prefix}unit={unit:05d}.json"


#: The one name `commit_key` writes under a prefix: `unit=` + at least five digits + `.json`.
_COMMIT_NAME = re.compile(r"unit=([0-9]{5,})\.json")


def commit_units(prefix: str, keys, n: int) -> list[int]:
    """Which Units a LISTING of one batch's commit prefix says finished — sorted, each once.

    Only a name the writer can produce counts: `unit=` + at least five digits + `.json`, directly
    under `prefix`. Anything else (an operator's marker, a temp file, a nested path, a sibling batch
    whose id merely starts the same) is not a Unit, and is skipped rather than guessed at — the
    decode of each object is the integrity check that follows.

    AN INDEX OUTSIDE THE BATCH RAISES `CommitInvalid`. The batch id hashes the Units, so a Unit
    past the batch's length under its prefix is not this batch's: folding it is impossible, and
    skipping it would hide whatever put it there. shared/conformance/commit.json §list pins this.
    """
    found = set()
    for key in keys:
        if not key.startswith(prefix):
            continue
        m = _COMMIT_NAME.fullmatch(key[len(prefix):])
        if m is None:
            continue
        i = int(m.group(1))
        if not 0 <= i < n:
            raise CommitInvalid(f"{key} names unit {i}, and this batch has {n} unit(s)")
        found.add(i)
    return sorted(found)


def encode_commit(batch_id: str, unit: int, out=None, error=None, category=None) -> dict:
    """One Unit's outcome as its commit object holds it.

    The body NAMES its batch and its Unit even though the key already does. That redundancy is the
    integrity check: a body at the wrong key — a copy, a skewed prefix, a hand edit — is refused
    on read instead of folded into a batch it does not describe.

    `out` is always a list: an isolated Unit has `[]`, and a committed Unit that pushed nothing has
    `[]` too, which a reader must tell apart from a missing `out` (refused, never "empty").
    """
    body = {"v": COMMIT_VERSION, "batch_id": batch_id, "unit": int(unit), "out": list(out or [])}
    if error is not None:
        body["error"] = error
        body["category"] = category or "exhausted"
    return body


def decode_commit(body: Any, batch_id: str, unit: int) -> dict:
    """Validate one commit object read back for (`batch_id`, `unit`), or raise `CommitInvalid`.

    Returns `{"out": [...]}` for a committed Unit, `{"out": [], "error": {...}, "category": ...}`
    for an isolated one.

    REFUSED RATHER THAN PARTLY READ, the checkpoint's rule one level down: a version this code does
    not know, a body naming another batch or another Unit, or a committed body with no `out` list.
    The safe-looking alternatives are both wrong — treating it as "not finished" re-runs silently,
    and folding what it says puts another Unit's output in this one.
    """
    if not isinstance(body, dict):
        raise CommitInvalid(f"commit object is a {type(body).__name__}, not an object")
    if body.get("v") != COMMIT_VERSION:
        raise CommitInvalid(f"commit object version {body.get('v')!r}; this reader knows "
                            f"{COMMIT_VERSION}")
    if body.get("batch_id") != batch_id:
        raise CommitInvalid(f"commit object names batch {body.get('batch_id')!r}, expected "
                            f"{batch_id!r}")
    got_unit = body.get("unit")
    if not isinstance(got_unit, int) or isinstance(got_unit, bool) or got_unit != unit:
        raise CommitInvalid(f"commit object names unit {got_unit!r}, expected {unit}")
    out = body.get("out")
    if not isinstance(out, list):
        raise CommitInvalid(f"commit object has no `out` list (got {out!r})")
    err = body.get("error")
    if err is None:
        return {"out": out}
    if not isinstance(err, dict):
        raise CommitInvalid(f"commit object's error is a {type(err).__name__}, not an object")
    return {"out": [], "error": err, "category": body.get("category") or "exhausted"}


def _part_safe(v: str) -> str:
    """Strip characters that would forge a partition segment or break a glob."""
    for ch in "/= *?":
        v = v.replace(ch, "_")
    return v


def _shard_of(node_id: str) -> str:
    """Render a node id as a sortable, glob-safe partition value: `n7` -> `0007`.

    Default FIRST, then derive from the defaulted value — deriving from the raw argument makes
    an empty node produce `shard=` with no value, a malformed path the reader can never match.

    A streaming chunk-run appends `.k` (`n1.2`); the suffix is split off before padding rather
    than defeating it, so `n1.2` -> `0001.2`. Padding only the un-chunked form would leave
    `n1.2` and `n10.2` sharing a prefix — the very collision the padding exists to prevent.
    """
    name = node_id or "node"
    base, _, chunk = name.partition(".")
    try:
        padded = f"{int(base[1:] if base.startswith('n') else base):04d}"
    except ValueError:
        return _part_safe(name)
    return f"{padded}.{_part_safe(chunk)}" if chunk else padded


def blob_key(run_date: str, actor: str, run_id: str, node_id: str, unit: int, sha: str) -> str:
    """The HIVE-PARTITIONED blob key — byte-identical to Go's unitstore.BlobKey.

        units/run={run}/dt=2026-08-02/actor=cachebuster/shard=0007/unit=00011/{sha}.json

    Every segment is a `key=value` partition, so DuckDB's `hive_partitioning=true` exposes
    run/dt/actor/shard/unit as typed COLUMNS and a `WHERE dt = ...` prunes at the file level.

    run= leads for a measured reason, not a stylistic one: every interactive query filters by
    run, and an object store prunes a LIST only by literal prefix. With dt= first, locating one
    run means listing the whole bucket — 8.4s against 0.17s on a 188k-object bucket, a gap that
    widens with every object ever written.

    run_date is the RUN's date, passed down from the handler rather than read from this worker's
    clock, so a run crossing midnight stays in one dt partition instead of splitting and
    disagreeing between hosts. Empty falls back to today: degraded, never a lost blob.

    This is a CROSS-SDK contract (ADR 0015): the Go and Python hosts must produce the same key
    for the same inputs, or the reader sees two different layouts. They have already drifted
    once (isolation counters shipped Go-only), so both are pinned to one golden fixture —
    shared/conformance/blobkey.json, asserted by each SDK's own suite.
    """
    return (
        f"units/run={_part_safe(run_id or 'run')}"
        f"/dt={_part_safe(run_date or _dt.datetime.now(_dt.timezone.utc).strftime('%Y-%m-%d'))}"
        f"/actor={_part_safe(actor or 'unknown')}"
        f"/shard={_shard_of(node_id)}"
        f"/unit={unit:05d}/{sha}.json"
    )


def blob_key_at(dt, actor: str, run_id: str, node_id: str, unit: int, sha: str) -> str:
    """The same key from a datetime, for callers holding a clock rather than a string."""
    return blob_key(f"{dt:%Y-%m-%d}", actor, run_id, node_id, unit, sha)


def _actor_name() -> str:
    return os.environ.get("KONTRA_ACTOR_NAME", "")


def from_env() -> Optional[UnitStore]:
    """The same KONTRA_S3_* contract as the Go handler and TS orchestrator."""
    endpoint = os.environ.get("KONTRA_S3_ENDPOINT")
    if not endpoint:
        return None
    return UnitStore(
        endpoint=endpoint,
        bucket=os.environ.get("KONTRA_S3_BUCKET", "kontra"),
        prefix=os.environ.get("KONTRA_S3_PREFIX", ""),
        access=os.environ.get("KONTRA_S3_ACCESS_KEY", "kontra"),
        secret=os.environ.get("KONTRA_S3_SECRET_KEY", "kontra"),
        region=os.environ.get("KONTRA_S3_REGION", "us-east-1"),
    )


def unit_ref(entry: Any) -> Optional[dict]:
    """The `{"$ref": {key,size,sha256}}` discriminator (shared with the TS resolver)."""
    if isinstance(entry, dict) and isinstance(entry.get("$ref"), dict):
        return entry["$ref"]
    return None
