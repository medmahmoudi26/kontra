-- Kontra gate schema, revision 0003 (M4).
--
-- The tasks the gate's own /mcp creates with hire, which its MCP Tasks see (spec/relay.md §12.2, DM4-5); the one GET
-- stream that delivers a hire task's notifications/elicitation/complete (R §12.3, DM4-21); the live anonymous tenants
-- of a network (T §6.2, DM4-10); the anon- slug, reserved to anonymous tenants (T §6.1, DM4-20); and the plain MCP
-- calls in flight to an anonymous tenant's services (T §6.2, DM4-20). Every M4 schema
-- change is in this one file, frozen when M4 merges. Section references are to spec/relay.md (R) and spec/tunnel.md
-- (T).
--
-- Applying it locks tasks. goose runs the file in one transaction, and its first ALTER TABLE takes ACCESS EXCLUSIVE on
-- tasks until the commit, so the task reads and writes of replicas still serving (a rolling update) wait for it. Under
-- that lock it scans tasks once, to build tasks_hired (CREATE INDEX CONCURRENTLY cannot run in a transaction); the
-- check on elicitation_notified_at costs no second scan (NOT VALID, below). Run `kontra-gate migrate` for it when a
-- pause of one sequential read of tasks is acceptable, or before the replicas start.

-- +goose Up
set local search_path to kontra;

-- R §12.2 (DM4-5): a task hire created. The MCP Tasks of /mcp see only these, of the principal's buyer, across
-- services and newest first (tasks/list). A constant default adds the column without rewriting the table.
alter table tasks add column hired boolean not null default false;
create index tasks_hired on tasks (buyer_id, status_at desc, id desc) where hired;

-- R §12.3 (DM4-21): when a GET stream of the session that received the -32042 claimed the task's
-- notifications/elicitation/complete. A replica writes the notification only if its
-- `update … set elicitation_notified_at = now() where id = $1 and elicitation_notified_at is null` won, so one stream
-- of the session gets it (MCP 2025-11-25 transports: a message is never broadcast across streams). Only a task that
-- sent a URL elicitation has one to claim. The check is added NOT VALID, so adding it does not scan tasks under the
-- lock above: the column is new, so it is null on every existing row and each satisfies the check, and Postgres
-- enforces the check on every row inserted or updated after.
alter table tasks add column elicitation_notified_at timestamptz;
alter table tasks add constraint elicitation_notified_has_id check (elicitation_notified_at is null or elicitation_id is not null) not valid;

-- T §6.2 (DM4-10): a network's live anonymous tenants, counted when a hello would open one more.
create index tenants_anonymous_network on tenants (created_from) where kind = 'anonymous' and deleted_at is null;

-- T §6.1 (DM4-20): a tenant is anonymous if and only if its slug is anon- and 16 lowercase hex digits, so no account
-- or team can take an anonymous tenant's name, and an anonymous tenant always has one.
alter table tenants add constraint anonymous_slug check ((kind = 'anonymous') = (slug ~ '^anon-[0-9a-f]{16}$'));

-- T §6.2 (DM4-20): one row per plain MCP tools/call being relayed to an anonymous tenant's service, at most
-- tunnel.anonymous.calls per tenant, counted gate-wide under a lock on the tenant's row. The row is deleted when the
-- call's answer has been relayed; a replica that dies takes its rows with its gate_replicas row (T §11), and the purge
-- takes a tenant's.
create table anon_calls (
    id          uuid primary key,
    tenant_id   uuid not null references tenants (id) on delete cascade,
    replica_id  text not null references gate_replicas (replica_id) on delete cascade,
    started_at  timestamptz not null default now()
);
create index anon_calls_by_tenant on anon_calls (tenant_id);
create index anon_calls_by_replica on anon_calls (replica_id);

-- +goose Down
set local search_path to kontra;
drop table anon_calls;
alter table tenants drop constraint anonymous_slug;
drop index tenants_anonymous_network;
alter table tasks drop constraint elicitation_notified_has_id;
alter table tasks drop column elicitation_notified_at;
drop index tasks_hired;
alter table tasks drop column hired;
