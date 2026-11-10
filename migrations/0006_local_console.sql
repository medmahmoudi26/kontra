-- Kontra gate schema, revision 0006 (the local development profile's console and the A2A traffic record; D352, D353,
-- D354).
--
-- a2a_exchanges: one row per A2A call the gate received, refused calls included (the a2a-traffic capability).
-- Metadata only: never a body, a header, a part, a credential or a credential's hash; the caller is its principal
-- name and via. No foreign key, so that a call refused for naming an unknown service is recorded with the name it
-- asked for, and so that an exchange's retention (traffic.retention, swept by `at`) never depends on its task's.
-- Rows are written in batches by a writer that drops rather than waits, so the table may miss calls under
-- pressure; the drops are counted by kontra_traffic_dropped_total, never hidden.
--
-- console_tasks: the tasks the local console started. In the local development profile every client is the one
-- anonymous caller, so the caller id cannot tell the console's tasks from another client's; the console replies
-- to, cancels and continues only the tasks listed here, and watches every other one.
--
-- services.retention_s may be 0: `task.retention: 0` keeps the service's tasks forever, and a task that ends
-- under it gets no retention_until, so the retention sweep never selects it (spec/relay.md §5.4).
--
-- The console's conversations are read through tasks_by_context (0001): a context is scoped by
-- (tenant_id, service, buyer_id, context_id) (spec/relay.md §4), so no new index on tasks is needed.

-- +goose Up
set local search_path to kontra;

create table a2a_exchanges (
    id           uuid primary key,                                     -- UUIDv7, minted when the call ends
    at           timestamptz not null,                                 -- when the call arrived
    tenant_id    uuid,                                                 -- null when the call named no known tenant
    service      text not null check (length(service) <= 256),        -- as requested; may name no service
    method       text not null check (length(method) between 1 and 64),  -- SendMessage, GetTask, card, ... or unknown
    binding      text not null check (binding in ('jsonrpc', 'rest')),
    principal    text check (length(principal) between 1 and 256),    -- null before authentication
    via          text check (via in ('api_key', 'oidc', 'mcp', 'node', 'anonymous')),
    task_id      uuid,                                                 -- the task the call named or created
    context_id   text check (length(context_id) between 1 and 256),
    bytes_in     bigint not null check (bytes_in >= 0),
    bytes_out    bigint not null check (bytes_out >= 0),
    duration_ms  integer not null check (duration_ms >= 0),
    http_status  smallint not null check (http_status between 100 and 599),
    rpc_code     integer,                                              -- the JSON-RPC error code, when one was answered
    reason       text check (length(reason) between 1 and 64),         -- the gate's reason for a refusal
    constraint principal_has_via check ((principal is null) = (via is null))
);
create index a2a_exchanges_by_time on a2a_exchanges (at desc, id desc);
create index a2a_exchanges_by_service on a2a_exchanges (tenant_id, service, at desc, id desc);
create index a2a_exchanges_by_task on a2a_exchanges (task_id, at) where task_id is not null;

create table console_tasks (
    task_id     uuid primary key references tasks (id) on delete cascade,
    created_at  timestamptz not null default now()
);

alter table services drop constraint services_retention_s_check,
    add constraint services_retention_s_check check (retention_s >= 0);

-- +goose Down
-- Drops the traffic record and the console's list of its tasks: the console then watches every task and acts on
-- none, as on a gate before 0006.
-- A service kept forever goes back to the default retention; its tasks that ended under 0 keep retention_until null.
set local search_path to kontra;
update services set retention_s = 604800 where retention_s = 0;
alter table services drop constraint services_retention_s_check,
    add constraint services_retention_s_check check (retention_s > 0);
drop table console_tasks;
drop table a2a_exchanges;
