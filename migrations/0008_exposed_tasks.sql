-- Kontra gate schema, revision 0008 (a callee's Message passes through; D362, OpenSpec change
-- relay-message-passthrough).
--
-- tasks.exposed_at: when the task became visible to its caller. The gate records every accepted message as a task
-- at admission (fences, attempts, retries, re-attach and resend hang on it), but a callee that answers with a
-- Message has made no task: the caller receives that Message, and the record stays provisional (null) for good.
-- A task is exposed, in the transaction of the event that causes it, when the callee answers with a Task or a task
-- event, when a gate-written status ends or answers the turn before any callee answer, and at admission when the
-- caller asked returnImmediately (or the task is a /mcp hire's, which is the gate's own). Every caller read
-- (GetTask, ListTasks, SubscribeToTask, cancel, follow-ups, push configs) sees exposed tasks only.
--
-- Every task created before 0008 is backfilled exposed at its creation, so nothing visible disappears. ListTasks'
-- index, tasks_by_caller, becomes partial on exposed tasks. tasks_by_context stays whole: the local console reads a
-- context's interactions through it, message-only ones included (they show as Caller SendMessage, Callee Message).

-- +goose Up
set local search_path to kontra;

alter table tasks add column exposed_at timestamptz;
update tasks set exposed_at = created_at;

drop index tasks_by_caller;
create index tasks_by_caller on tasks (tenant_id, service, buyer_id, status_at desc, id desc) where exposed_at is not null;

-- +goose Down
-- The old gate ignores the column; dropping it shows message-only records as completed tasks again (D5).
set local search_path to kontra;

drop index tasks_by_caller;
create index tasks_by_caller on tasks (tenant_id, service, buyer_id, status_at desc, id desc);
alter table tasks drop column exposed_at;
