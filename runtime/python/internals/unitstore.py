"""Per-unit blob store for the actor host — the actor side of the claim-check data plane.

Each unit's output is written to the object store THE MOMENT it completes (deterministic
key `units/{run_id}/{node_id}/u{i}.json`), and the durable commit holds only a small ref
`{"$ref": {key, size, sha256}}` — so neither actor memory nor the Redis state store ever
carries batch payloads. Deterministic keys (not content-addressed) on purpose: a re-run
overwrites idempotently, and a node's whole output is an S3 PREFIX a query engine can
scan directly (`read_json('s3://…/units/{run}/{node}/*.json')`) without the manifest.

Unconfigured (`KONTRA_S3_ENDPOINT` unset) -> None: the host commits payloads inline,
exactly the no-S3 dev/test behavior. Configured -> `ensure()` fails fast at serve() (S3
sits in the per-unit hot path; better to die at boot than on the first unit).

boto3 is sync — callers wrap writes in `asyncio.to_thread` so a PUT never stalls the
window's event loop.
"""

from __future__ import annotations

import datetime as _dt
import hashlib
import json
import os
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
        # separate blast radius (backend/src/data/parquet.ts builds `s3://<bucket>/` + this key)
        # and it belongs with prefix rows in conformance/blobkey.json, which has none either.
        key = self.prefix + blob_key(run_date, _actor_name(), run_id, node_id, i, sha)
        self._s3.put_object(Bucket=self.bucket, Key=key, Body=data,
                            ContentType="application/json")
        return {"$ref": {"key": key, "size": len(data), "sha256": sha}}



    def get_subunit(self, key: str) -> Any:
        """Read back ONE record written by put_subunit, unwrapping the 1-element array it
        wraps records in (see put_subunit).

        This is the INGEST half of the blob plane, and it exists so a Method can be handed the
        output of another Method without the payload passing through the caller. The caller has
        no S3 credentials by design (`actorkit.catalog` promises exactly that), but the actor
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
    conformance/blobkey.json, asserted by each SDK's own suite.
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
