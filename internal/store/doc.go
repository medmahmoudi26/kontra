// Package store is the gate's Postgres record: the task projection, the event log, work items and
// attempts (spec/relay.md §4, §5, §8.1, §15; migrations/0001_init.sql).
//
// Two surfaces share one implementation of the write transaction:
//
//   - Store implements a2a-go's taskstore.Store for one service. Every query is scoped by
//     (tenant_id, service, buyer_id) taken from the route and the authenticated principal, never from the
//     request body (PRD §12). It is what a2a-go's handler reads and writes.
//   - Log is the unscoped internal API the lifecycle, push, the SSE writer and the work queue use.
//
// Every change to a task is one transaction (spec §5.2): the projection UPDATE with the version check and
// the terminal rule, the task_events INSERT at seq = the new version, and pg_notify('kontra_task_events',
// '<task_id>:<seq>'). A stale version is taskstore.ErrConcurrentModification, never a merge; PrevVersion 0
// is refused the same way. Writes run on context.WithoutCancel with a deadline of their own (D8).
// Snapshots and payloads are `json`, so NUL characters and duplicate keys in relayed content survive (D49).
package store
