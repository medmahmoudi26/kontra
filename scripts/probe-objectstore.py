#!/usr/bin/env python3
"""Can the process running me write to kontra's object store? Run it INSIDE an actor.

    docker exec <actor-container> python3 /probe-objectstore.py

Reads the same `KONTRA_S3_*` variables the Go handler's `ConfigFromEnv` reads, with the same
defaults, and attempts the one operation that matters: a small PUT into the configured bucket.

WHY A SCRIPT AND NOT A `run:` BLOCK. The first version of this was inline Python inside a YAML
block scalar and it broke the parse — which is the lesser reason. The real one is already written
down beside `scripts/assert-install-is-self-contained.py`: a multi-line checker embedded in a
workflow is something nothing can run locally and nothing can test.

WHY FROM INSIDE THE ACTOR. `the local docker install` stops at `kontra.store_blob`, dispatched and
never returning — no error, two minutes, then worker shutdown. Everything above it is ruled out by
evidence: seaweed answers and lists the `kontra` bucket, first boot logged creating it, the actor
reached `temporal:7233` and POSTed to the catalog with a 200. What has never been checked is the
endpoint the ACTOR holds. `docker-compose.yml` sets `http://seaweed:8333`; `kontra deploy`'s banner
advertises `http://orchestrator-api:8333` for a manual `docker run`; and a working actor on a
developer machine carries `http://seaweed:8333`. Only the actor can answer which it got.

IT NEVER PRINTS A KEY, and it does not need to: the endpoint and the bucket are not secrets, and the
failure mode being hunted is reachability. Timeouts are short and retries are off deliberately — a
hang has to report AS a hang, in seconds, rather than as the job's wall clock.
"""

from __future__ import annotations

import os
import sys

CONNECT_TIMEOUT = 10
READ_TIMEOUT = 10
KEY = "ci-probe"


def main() -> int:
    endpoint = os.environ.get("KONTRA_S3_ENDPOINT")
    bucket = os.environ.get("KONTRA_S3_BUCKET") or "kontra"
    print(f"KONTRA_S3_ENDPOINT = {endpoint or '(unset)'}")
    print(f"KONTRA_S3_BUCKET   = {bucket}")
    if not endpoint:
        # The handler treats an unset endpoint as "run the codec in passthrough", so this is a
        # legitimate configuration and NOT a failure — but it would also mean store_blob never
        # touches S3, which would make this whole line of enquiry the wrong one.
        print("no endpoint configured — the codec would run in passthrough, not store_blob")
        return 0

    try:
        import boto3
        from botocore.config import Config
    except ImportError as e:  # pragma: no cover - depends on the image
        print(f"boto3 is not in this image: {e}")
        return 0

    client = boto3.client(
        "s3",
        endpoint_url=endpoint,
        aws_access_key_id=os.environ.get("KONTRA_S3_ACCESS_KEY") or "kontra",
        aws_secret_access_key=os.environ.get("KONTRA_S3_SECRET_KEY") or "kontra",
        region_name=os.environ.get("KONTRA_S3_REGION") or "us-east-1",
        config=Config(
            connect_timeout=CONNECT_TIMEOUT,
            read_timeout=READ_TIMEOUT,
            retries={"max_attempts": 1},
            s3={"addressing_style": "path"},  # SeaweedFS, as the Go side sets UsePathStyle
        ),
    )

    for label, call in (
        ("list_buckets", lambda: [b["Name"] for b in client.list_buckets().get("Buckets", [])]),
        ("put_object", lambda: client.put_object(Bucket=bucket, Key=KEY, Body=b"x") and "ok"),
    ):
        try:
            print(f"{label}: {call()}")
        except Exception as e:  # noqa: BLE001 - the type is the finding
            print(f"{label} FAILED: {type(e).__name__}: {str(e)[:400]}")
            # EXIT 0 ON A FAILED CALL. This is a probe inside a diagnostic block that has already
            # decided the job fails; a non-zero here would only mask the caller's own exit code.
            return 0
    print("the actor CAN write to the object store — store_blob is not blocked on reachability")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
