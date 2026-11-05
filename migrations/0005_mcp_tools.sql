-- Kontra gate schema, revision 0005 (the tools of /mcp, D338).
--
-- One change: the index a hire's idempotencyKey is found through (spec/relay.md §12.3 Idempotency, D338). A hire with
-- an idempotencyKey mints its A2A messageId from the key, the buyer and the principal; a later hire with the same key
-- answers the task of that first_message_id whatever its service, and refuses one with other arguments, so the lookup
-- reads one buyer's hire tasks by first_message_id across services. tasks_by_first_message leads with (tenant_id,
-- service) and tasks_hired orders by status_at, so without this index the lookup would walk the buyer's tasks.
--
-- Applying it locks tasks against writes for one scan (goose runs the file in one transaction, where CREATE INDEX
-- CONCURRENTLY cannot run): the task writes of replicas still serving wait for it, as for 0003's tasks_hired and
-- 0004's tasks_by_buyer.

-- +goose Up
set local search_path to kontra;

create index tasks_hired_by_message on tasks (buyer_id, first_message_id) where hired;

-- +goose Down
set local search_path to kontra;
drop index tasks_hired_by_message;
