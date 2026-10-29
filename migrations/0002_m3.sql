-- Kontra gate schema, revision 0002 (M3).
--
-- The claim-check bucket's refs (spec/relay.md §10.4, D167, D181), the peer hop's address of each replica
-- (relay §11.2, D163, D179), one open virtual MCP session per principal and service (relay §11.12, D177) and the cap
-- of MCP sessions per principal and service (relay §11.2, D213).
-- Every M3 schema change is in this one file, frozen when M3 merges. Section references are to
-- spec/relay.md (R) and spec/tunnel.md (T).

-- +goose Up
set local search_path to kontra;

-- A part over 256 KiB is an object in the bucket, logged in its event's payload as a url part
-- `kontra-blob:<ref>` (R §10.4). One task_events.payload_ref cannot name the several large parts of one
-- message, so each ref maps to its task here; a ref is rendered, and served on a blob stream (T §8.3), only
-- for the task its row names (D181). The object's key is <prefix>/<tenant>/<task>/<ref>; retention deletes a
-- task's objects before its rows, and these rows go with the task.
create table blobs (
    ref         text primary key check (ref ~ '^[a-z2-7]{26}$'),   -- 128 random bits, base32, lowercase
    tenant_id   uuid not null,
    task_id     uuid not null references tasks (id) on delete cascade,
    size        bigint not null check (size > 0),
    created_at  timestamptz not null default now()
);
create index blobs_by_task on blobs (task_id);

-- The address another replica reaches this one's peer listener at (R §11.2, `peer.listen`); null without one.
alter table gate_replicas add column peer_addr text;

-- D177: one open virtual session per principal and service; the gate's own /mcp (service null) too.
create unique index mcp_sessions_virtual on mcp_sessions (tenant_id, service, principal) nulls not distinct
    where protocol_version = '2026-07-28' and closed_at is null;

-- D213, D231: the open MCP sessions one principal may hold on a tunnel service (R §11.2, max_sessions_per_principal);
-- an initialize beyond it closes the principal's sessions idle for 5 minutes, or is refused. The index finds a
-- principal's open sessions.
alter table services add column max_sessions_per_principal integer not null default 4
    check (max_sessions_per_principal between 1 and 10000);
create index mcp_sessions_principal on mcp_sessions (tenant_id, service, principal, last_seen_at) where closed_at is null;

-- +goose Down
set local search_path to kontra;
drop index mcp_sessions_principal;
alter table services drop column max_sessions_per_principal;
drop index mcp_sessions_virtual;
alter table gate_replicas drop column peer_addr;
drop table blobs;
