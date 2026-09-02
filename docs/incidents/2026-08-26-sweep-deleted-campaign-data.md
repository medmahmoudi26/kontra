# 2026-08-26 — the dataset sweep deleted 223,378 rows of campaign data

**Outcome: not recovered, by operator decision.** The collected Datasets were untagged and past
their TTL, which is what retention exists to do. Both defects that made it possible are fixed.

## What happened

An agent building the appliance's orchestrator bundle had, as an acceptance criterion, "running the
orchestrator from an unpacked bundle serves the API and completes a run". To satisfy it, it ran a
workflow against the live local stack — with isolated ports and three isolated queue names. It chose
`sweepDatasetsWorkflow`. The run completed:

```
purgedRuns: 41   purgedRows: 223378   dryRun: false
```

Writes spanned 2026-08-03 to 2026-08-19, across `wf-docker-registry-monitor-*`, `wf-layer-scan-*`,
`wf-webcrawl-*`, `wf-desync-*` and nscheck runs.

The agent reported it immediately, attempted no recovery, and preserved the full list of collected
Datasets. That is why both causes below are known precisely rather than guessed at.

## Why the isolation did not hold

**1. The sweep could not be routed.** `workflows/retention.ts` pinned the `DATASET_QUEUE` CONSTANT
rather than resolving a queue, so `KONTRA_DATASET_QUEUE` was ignored and the live materializer took
the work. Fixed: the queue is resolved by the schedule creator — ordinary Node code that may read the
environment — and travels as workflow INPUT. `datasetQueue()` would NOT have worked either: workflow
code runs in a `vm` context where `process.env` is not merely a determinism hazard but is not
defined, so that call throws at module scope. What pins the fix is that `KONTRA_DATASET_QUEUE` now
appears nowhere in the workflow bundle, and a sweep with no queue is refused by name rather than
defaulting back to the shared one.

**2. The deployed image predated the dry-run guard.** The running containers carried
`dryRun: input.dryRun ?? false`; the source had already been corrected to
`input.dryRun ?? !retentionCollects()`. Anyone starting that workflow with no input against that
stack deleted data. Fixed by redeploying, and verified by reading the DEPLOYED BYTES rather than the
source.

## Containment

The smoke test's boot created the `kontra-dataset-retention` Schedule — hourly, unpaused, and it had
already fired. It was **paused**. `createRetentionSchedule` swallows ALREADY_EXISTS and leaves an
existing Schedule alone, so the subsequent redeploy did not silently re-arm it.

## What generalises

**A brief that forbids restarting containers does not forbid running workflows against them**, and
an acceptance criterion asking for "a completed run" invites exactly that. Destructive workflows must
be named as forbidden, with a safe alternative given — start your own server on a free port.

**Check the deployed bytes, not the source**, before asserting a safety property holds in a running
system. The guard existed in git and not in the image, and every statement made about it being safe
was true of the wrong artifact.

**A negative test needs a control.** The sweep's own dry-run posture is now proven by a test that
also shows the sweep collecting when told to — a preview that finds nothing proves nothing unless
the same path can be shown to delete.

---

## Postscript: the fleet, verified live 2026-08-27

The credential change this incident forced (a named secret resolved at the last hop, replacing
`DIGITALOCEAN_TOKEN` in the environment) was verified against real infrastructure:

```
kontra fleet up   --count 1 --fleet v2parity-0826 --tag v2parity-0826
  → {"create":2}   kf-v2parity-0826-01  10.124.0.3  24.199.122.198
kontra fleet status → COMPLETED, machine inventoried
kontra fleet down   → {"delete":2}
```

Before provisioning, the three pre-existing droplets were written down and a teardown rule fixed —
destroy only names beginning `kf-v2parity-`. After teardown: exactly those three remained, and no
`kf-v2parity-*`. The naming was the whole point: after an agent deleted 223,378 rows because
isolation rested on names nothing enforced, "which machine is mine" had to be answerable from the
name alone rather than from memory of what was there before.
