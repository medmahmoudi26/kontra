// Package store is the gate's Postgres record: the task projection, the event log, work items and
// attempts (spec/relay.md §4, §5, §8.1, §9, §15; migrations/0001_init.sql).
//
// Two surfaces share one implementation of the write transaction:
//
//   - Store implements a2a-go's taskstore.Store for one service. Every query is scoped by
//     (tenant_id, service, buyer_id) taken from the route and the authenticated principal, never from the
//     request body (PRD §12). It is what a2a-go's handler reads and writes.
//   - Log is the unscoped internal API the lifecycle, push, the SSE writer and the work queue use.
//
// Every change to a task is one statement (spec §5.2): the projection UPDATE with the version check, the
// terminal rule, the caller scope and the dispatch fence (D77); the task_events INSERT at seq = the new
// version; for the first event logged from an upstream's answer to a follow-up, that work item's
// delivered_at (D81); and pg_notify('kontra_task_events', '<task_id>:<seq>'). A stale version is
// taskstore.ErrConcurrentModification, never a merge; PrevVersion 0 is refused the same way. Writes run on
// context.WithoutCancel with a deadline of their own (D8). Snapshots and payloads are `json`, so NUL
// characters survive and bytes passed to Log.Append are stored verbatim (D49).
//
// Puller feeds a2a-go's live subscriptions from the log (eventqueue.Puller); Folder and Log.SnapshotAt
// rebuild the task as of any seq for resume and push (spec §9, D75).
//
// All SQL names the kontra schema explicitly (D37), so the pool's search_path does not matter.
package store
