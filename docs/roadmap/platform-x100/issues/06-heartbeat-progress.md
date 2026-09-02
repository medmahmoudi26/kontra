# 06 — Progress via Temporal activity heartbeats

Status: ready-for-agent
**Tier:** 2 | **Effort:** M | **Depends on:** —

## Problem
Live progress today is stitched together from custom plumbing (host → orchestrator → CLI) and, in
`kontra monitor`, by counting S3 blobs. `RunBatch` is a long-running activity, yet it does not tell
Temporal how far along it is — so the Temporal UI shows an opaque "running" and we glob S3 to guess.

## Native capability to use
Temporal **activity heartbeats**. A long activity calls `RecordActivityHeartbeat(ctx, details)`;
the details are visible in the Web UI and readable via `DescribeWorkflowExecution` /
` describeActivity`. Heartbeats also enable `HeartbeatTimeout` for fast failure detection, and the
details survive as resume state.

## Approach
- In the handler's `RunBatch` activity (`runtime/handler/activity.go`), call `RecordActivityHeartbeat` with
  `{done, total, node}` as units complete (throttle to ~1/sec).
- Set `HeartbeatTimeout` on the activity options so a wedged `RunBatch` (cf. crawl4ai hangs) fails
  fast and retries instead of silently stalling.
- `kontra monitor --state` reads heartbeat details (via an orchestrator endpoint over
  `DescribeWorkflowExecution`) instead of / in addition to S3 blob counts.

## Files
- `runtime/handler/activity.go` (heartbeat), `runtime/handler/workflow.go` (HeartbeatTimeout on activity opts),
  orchestrator endpoint, `cli/monitor.go`

## Verify (local, no fleet)
- Dispatch a multi-unit `RunBatch`; the Temporal UI shows advancing `done/total`.
- `kontra monitor --state` shows the same live counter (matching, or ahead of, S3 blob counts).
- Simulate a hang → `HeartbeatTimeout` fires and the activity retries.

## Risks
- Heartbeat frequency vs. Temporal server load — throttle. Coordinate `runtime/handler/workflow.go` edits
  with issue 04 (same file).

## Comments
