-- Kontra gate schema, revision 0001.
--
-- Owned by the public repository and applied by `kontra-gate migrate` (pressly/goose single-file format,
-- docs/decisions.md D28). Every table lives in the schema `kontra` (D37), so an operator's own tables
-- (Kontra Cloud) can share the database without a name ever colliding; the operator migrates its own.
-- Postgres 16 or later. Every id is minted by the gate (UUIDv7 in Go; Postgres before 18 has no
-- uuidv7()), so no table defaults an id. `kontra-gate migrate` creates the schema itself before goose runs
-- and names goose's table `kontra.goose_db_version` (goose creates it before any migration, D28).
-- Event payloads, snapshots and the served card are `json`, not `jsonb`: the gate stores what it relays
-- without reinterpreting it, and jsonb refuses a NUL character that A2A and MCP text may carry (D49).
--
-- Section references are to spec/relay.md (R) and spec/tunnel.md (T).

-- +goose Up
create schema if not exists kontra;
set local search_path to kontra;

-- Tenants (R §1). A team gate has one ("default"); Cloud has one per account and one per anonymous
-- session, which expires (note 2 §2).
create table tenants (
    id            uuid primary key,
    slug          text not null unique check (slug ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'),
    kind          text not null check (kind in ('team', 'account', 'anonymous')),
    created_at    timestamptz not null default now(),
    expires_at    timestamptz,
    -- Anonymous sessions only: the requesting network (IPv4 /24, IPv6 /48), never the address (T §6.2).
    created_from  cidr,
    deleted_at    timestamptz,                         -- anonymous tenants: kept as a tombstone, so a slug is never reused (T §6.1)
    constraint anonymous_tenants_expire check (kind <> 'anonymous' or (expires_at is not null and created_from is not null))
);
create index tenants_anonymous_expiry on tenants (expires_at) where kind = 'anonymous';

-- Services (R §1, §3, §11, §13.4): an A2A agent or an MCP server behind the gate.
create table services (
    tenant_id        uuid not null references tenants (id) on delete cascade,
    name             text not null check (name ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'),
    kind             text not null check (kind in ('a2a', 'mcp')),
    backend          jsonb not null,                    -- {"tunnel": {}} | {"a2a": {"url", "allow_private"}} | {"mcp": {"url", "allow_private"}}
    allow            text[] not null default '{}',      -- R §2.3 principals; deny by default
    tools            jsonb,                             -- R §11.3 {"allow": [glob], "deny": [glob]}; mcp only
    tasks_mode       text check (tasks_mode in ('durable', 'passthrough', 'off')),   -- R §11.5; mcp only
    sealed           text not null default 'off' check (sealed in ('off', 'optional', 'required')),
                                                        -- ext-sealed.md §1: config `sealed: true` loads as 'required', `{required: false}` as 'optional'
    retries          integer not null default 2 check (retries between 0 and 10),
    timeout_s        integer not null default 3600 check (timeout_s > 0),
    retention_s      integer not null default 604800 check (retention_s > 0),
    annotations      jsonb not null default '{}',       -- R §13.4 operator data, never interpreted
    seller_keys      jsonb,                             -- R §3: JWKS (or {"url"}) the seller's card signatures are verified against
    session_idle_s   integer not null default 3600 check (session_idle_s > 0),   -- R §11.2; mcp only in effect
    listed           boolean not null default false,    -- R §12.2 search visibility outside the tenant
    card             json,                              -- the served card (R §3), a2a only; json: jsonb refuses U+0000, reorders keys (D49)
    upstream_card    bytea,                             -- the upstream card exactly as fetched (R §3, D13)
    card_hash        bytea,                             -- sha256 over the JCS of the upstream card
    card_fetched_at  timestamptz,
    source           text not null check (source in ('config', 'db')),
    disabled_at      timestamptz,                       -- removed from config / unpublished; rows are not deleted
    created_at       timestamptz not null default now(),
    updated_at       timestamptz not null default now(),
    primary key (tenant_id, name),
    constraint mcp_only_keys check (kind = 'mcp' or (tools is null and tasks_mode is null))
);

-- Node identity keys (T §2), one row per key per gate. key_id is derived from the public key.
create table tunnel_keys (
    key_id          text primary key check (key_id ~ '^k1_[a-z2-7]{26}$'),
    tenant_id       uuid not null references tenants (id) on delete cascade,
    pubkey          bytea not null unique check (length(pubkey) = 32),   -- X25519
    node_name       text not null check (length(node_name) between 1 and 64),
    enrolled_at     timestamptz not null default now(),
    revoked_at      timestamptz,
    revoked_reason  text check (revoked_reason in ('owner', 'operator', 'rotated', 'expired')),
    rotated_to      text references tunnel_keys (key_id),
    overlap_until   timestamptz,                       -- T §12: a rotated key stays valid for in-flight tasks
    unique (key_id, tenant_id)
);

-- Which services a key may host (T §5). The service comes from this table, never from a request.
create table key_services (
    key_id       text not null,
    tenant_id    uuid not null,
    service      text not null,
    enrolled_at  timestamptz not null default now(),
    revoked_at   timestamptz,
    primary key (key_id, service),
    foreign key (key_id, tenant_id) references tunnel_keys (key_id, tenant_id) on delete cascade,
    foreign key (tenant_id, service) references services (tenant_id, name) on delete cascade
);
create index key_services_by_service on key_services (tenant_id, service);

-- One-time enrollment tokens (T §5): single use, 15 minutes, bound to one service, or (operator-issued,
-- D40) to the tenant: such a token enrolls the key for the services the node declares. Stored as sha256.
create table enrollment_tokens (
    id           uuid primary key,
    tenant_id    uuid not null references tenants (id) on delete cascade,
    scope        text not null check (scope in ('service', 'tenant')),
    service      text,
    bind_services text[],                              -- tenant scope: the service names it may bind (T §5)
    token_hash   bytea not null unique check (length(token_hash) = 32),
    created_at   timestamptz not null default now(),
    expires_at   timestamptz not null,
    used_at      timestamptz,
    used_by_key  text references tunnel_keys (key_id) on delete set null,
    foreign key (tenant_id, service) references services (tenant_id, name) on delete cascade,
    constraint fifteen_minutes check (expires_at <= created_at + interval '15 minutes'),
    constraint used_by_needs_used_at check (used_by_key is null or used_at is not null),
    constraint scope_names_service check ((scope = 'service') = (service is not null)),
    constraint tenant_scope_lists_services check (scope = 'service' or (bind_services is not null and cardinality(bind_services) > 0))
);

-- Gate replicas (T §11, R §6.1): heartbeats, so tunnels of a dead replica are never selected.
create table gate_replicas (
    replica_id    text primary key,
    version       text not null,
    started_at    timestamptz not null default now(),
    last_seen_at  timestamptz not null default now(),
    draining_at   timestamptz
);

-- Live tunnel sessions (T §11). Deleted when the socket closes or its replica is reaped.
create table tunnels (
    session_id       uuid primary key,
    key_id           text not null references tunnel_keys (key_id) on delete cascade,
    tenant_id        uuid not null references tenants (id) on delete cascade,
    replica_id       text not null references gate_replicas (replica_id) on delete cascade,
    services         text[] not null,                  -- what this session serves: enrolled ∩ declared
    hire_principal   text,                             -- R §2.3: principal of node-opened streams
    cli_version      text not null,
    connected_at     timestamptz not null default now(),
    last_ping_at     timestamptz not null default now()
);
create index tunnels_by_key on tunnels (key_id);
create index tunnels_by_replica on tunnels (replica_id);
create index tunnels_by_tenant on tunnels (tenant_id);

-- API keys (R §2.3). Stored as sha256; the prefix is what people see.
create table api_keys (
    id            uuid primary key,
    tenant_id     uuid not null references tenants (id) on delete cascade,
    buyer_id      uuid not null,
    name          text not null default '',
    prefix        text not null unique check (prefix ~ '^[A-Za-z0-9_]{4,32}$'),   -- the visible start of the key; unique, it names the principal (R §2.3)
    hash          bytea not null unique check (length(hash) = 32),
    scopes        text[] not null default '{hire}',       -- R §2.3: hire, enroll:<service>, enroll:*
    created_at    timestamptz not null default now(),
    expires_at    timestamptz,
    revoked_at    timestamptz,
    last_used_at  timestamptz
);
create index api_keys_by_buyer on api_keys (buyer_id);

-- Gate tasks (R §4, §5): the projection a2a-go's taskstore.Store reads and writes.
create table tasks (
    id                uuid primary key,
    tenant_id         uuid not null,
    service           text not null,
    protocol          text not null check (protocol in ('a2a', 'mcp')),   -- what dispatch speaks upstream
    buyer_id          uuid not null,
    principal         text not null,
    via               text not null check (via in ('api_key', 'oidc', 'mcp', 'node', 'anonymous')),
    context_id        text not null check (length(context_id) between 1 and 256),
    first_message_id  text check (length(first_message_id) between 1 and 256),   -- A2A: the first message's messageId
    first_request     bytea check (length(first_request) = 32),                   -- sha256 of that request, for resend detection (A2A: R §15, D74; hire: R §12.3)
    state             text not null check (state in ('quoted', 'submitted', 'working', 'input-required',
                                                     'auth-required', 'completed', 'failed', 'canceled', 'rejected')),
    version           bigint not null check (version >= 1),               -- seq of the last event; 0 is never used
    snapshot          json not null,
    status_at         timestamptz not null,                               -- the logged status.timestamp; ListTasks order (R §5.2)
    elicitation_id    text unique,                                        -- R §12.3, for notifications/elicitation/complete
    elicitation_session bytea check (length(elicitation_session) = 32),   -- the mcp_sessions.client_sid_hash that received the -32042
    quote_expires_at  timestamptz,
    deadline          timestamptz,                                        -- R §6.5, fixed at submission
    created_at        timestamptz not null default now(),
    submitted_at      timestamptz,
    updated_at        timestamptz not null default now(),
    terminal_at       timestamptz,
    retention_until   timestamptz,
    foreign key (tenant_id, service) references services (tenant_id, name) on delete cascade,
    constraint resend_key_pair check ((first_message_id is null) = (first_request is null)),
    constraint elicitation_pair check ((elicitation_id is null) = (elicitation_session is null)),
    constraint quoted_has_expiry check (state <> 'quoted' or quote_expires_at is not null),
    constraint terminal_has_time check ((state in ('completed', 'failed', 'canceled', 'rejected')) = (terminal_at is not null))
);
-- Caller-facing reads are scoped by (tenant, service, buyer): no protocol lists across buyers (PRD §12).
-- A service's owner may see the tasks of its own services through the operator's API (R §13.5, D39).
-- No index names updated_at, so artifact-only and metadata-only updates stay HOT-eligible (D49).
alter table tasks set (fillfactor = 70);
create index tasks_by_caller on tasks (tenant_id, service, buyer_id, status_at desc, id desc);
create index tasks_by_service on tasks (tenant_id, service, status_at desc, id desc);
create index tasks_by_first_message on tasks (tenant_id, service, buyer_id, first_message_id) where first_message_id is not null;
create index tasks_by_first_request on tasks (tenant_id, service, buyer_id, first_request) where first_request is not null;
create index tasks_by_context on tasks (tenant_id, service, buyer_id, context_id, status_at desc, id desc);
create index tasks_open on tasks (created_at) where terminal_at is null;
create index tasks_expired on tasks (retention_until) where retention_until is not null;

-- Attempts (R §4, §6.4): one placement of a gate task on one upstream; its first dispatch, each follow-up turn and
-- each re-attach are dispatches on it (dispatch_id). Upstream ids live only here.
create table attempts (
    task_id               uuid not null references tasks (id) on delete cascade,
    n                     integer not null check (n >= 1),
    backend               text not null check (backend in ('tunnel', 'direct')),
    tunnel_session        uuid,                          -- no foreign key: sessions are deleted when they close
    key_id                text,
    replica_id            text,                          -- R §4: where the dispatch runs; routes abort and forward_cancel
    dispatch_id           uuid,                          -- R §6.4: the running dispatch's fence, fresh per dispatch (D77)
    upstream_task_id      text,
    upstream_context_id   text,
    upstream_session      text,                          -- R §11.5: the MCP session the dispatch opened
    started_at            timestamptz not null default now(),
    first_upstream_id_at  timestamptz,
    ended_at              timestamptz,                   -- R §4: with outcome, when the attempt ends; never at input-required
    outcome               text check (outcome in ('completed', 'failed', 'canceled', 'rejected', 'interrupted',
                                                  'no_session', 'tunnel_lost', 'upstream_unreachable', 'upstream_error',
                                                  'service_unavailable', 'timeout', 'sealed_rejected', 'aborted')),
    primary key (task_id, n),
    constraint first_id_with_id check ((first_upstream_id_at is null) = (upstream_task_id is null)),
    constraint tunnel_has_session check (backend <> 'tunnel' or coalesce(outcome, '') = 'no_session'
                                         or (tunnel_session is not null and key_id is not null and replica_id is not null))
);

-- The event log (R §5.2): one row per change, seq = the task's version after it. Append-only.
create table task_events (
    task_id      uuid not null references tasks (id) on delete cascade,
    seq          bigint not null check (seq >= 1),
    kind         text not null check (kind in ('task', 'status', 'artifact', 'message', 'metadata',
                                               'input_request', 'input_response', 'result')),
    payload      json,
    payload_ref  text,                                   -- R §10.4: bucket key for parts over 256 KiB, never a URL
    created_at   timestamptz not null default now(),
    primary key (task_id, seq),
    constraint has_body check (payload is not null or payload_ref is not null)
);

-- task_events is append-only (PRD §12): rows are inserted and, at retention, deleted; never updated.
-- +goose StatementBegin
create function task_events_append_only() returns trigger language plpgsql as $$
begin
    raise exception 'task_events is append-only';
end;
$$;
-- +goose StatementEnd
create trigger task_events_append_only before update on task_events
    for each row execute function task_events_append_only();

-- Work queue payloads by reference (R §8.1, D6): never in Temporal history. Sanitized: no credentials.
create table work_items (
    id           uuid primary key,
    task_id      uuid not null references tasks (id) on delete cascade,
    kind         text not null check (kind in ('execute', 'cancel')),
    payload      json not null,
    created_at   timestamptz not null default now(),
    consumed_at  timestamptz,
    delivered_at timestamptz                         -- R §6.4: the upstream first answered this follow-up (D81)
);
create index work_items_by_task on work_items (task_id);

-- Push configs (R §10). config holds the caller's webhook credentials: never logged, never exported.
create table push_configs (
    task_id        uuid not null references tasks (id) on delete cascade,
    id             text not null check (length(id) between 1 and 128),
    config         jsonb not null,
    delivered_seq  bigint not null default 0,
    failed_at      timestamptz,
    created_at     timestamptz not null default now(),
    primary key (task_id, id)
);

-- MCP client sessions (R §11.2). The client's Mcp-Session-Id is stored as its sha256.
create table mcp_sessions (
    client_sid_hash      bytea primary key check (length(client_sid_hash) = 32),
    tenant_id            uuid,                           -- the service's tenant; for the gate's own /mcp, the principal's (null when it has none)
    service              text,                           -- null for the gate's own /mcp (R §12)
    buyer_id             uuid not null,
    principal            text not null,
    via                  text not null check (via in ('api_key', 'oidc', 'mcp', 'node', 'anonymous')),
    upstream_sid         text,
    tunnel_session       uuid,
    replica_id           text,
    protocol_version     text not null,
    client_capabilities  jsonb not null default '{}',
    created_at           timestamptz not null default now(),
    last_seen_at         timestamptz not null default now(),
    closed_at            timestamptz,
    close_reason         text check (close_reason in ('deleted', 'idle', 'tunnel_lost', 'revoked', 'upstream_closed')),
    foreign key (tenant_id, service) references services (tenant_id, name) on delete cascade,
    constraint service_has_tenant check (service is null or tenant_id is not null)
);
create index mcp_sessions_open on mcp_sessions (tenant_id, service, buyer_id) where closed_at is null;
create index mcp_sessions_idle on mcp_sessions (last_seen_at) where closed_at is null;
create index mcp_sessions_by_service on mcp_sessions (tenant_id, service);

-- Calls (R §11.4): one row per MCP tools/call. Only what is outside sealed content. No price. The operator deletes them.
create table calls (
    id            uuid primary key,
    tenant_id     uuid not null,
    service       text not null,
    tool          text not null check (length(tool) between 1 and 128),
    buyer_id      uuid not null,
    principal     text not null,
    mcp_session   bytea,                                 -- mcp_sessions.client_sid_hash
    task_id       uuid,                                  -- set for task-augmented calls
    bytes_in      bigint not null check (bytes_in >= 0),
    bytes_out     bigint not null check (bytes_out >= 0),
    duration_ms   integer not null check (duration_ms >= 0),
    is_error      boolean,                               -- null: outcome not observed (passthrough task-augmented call, R §11.5)
    at            timestamptz not null default now(),
    foreign key (tenant_id, service) references services (tenant_id, name) on delete cascade
);
create index calls_by_service on calls (tenant_id, service, at);
create index calls_by_buyer on calls (buyer_id, at);

-- Audit hash chain (PRD §12): enrollments, revocations, key and service changes, sealed-part rejections.
-- Appends are serialized (an advisory transaction lock); the chain is verified by prev_hash, not by n
-- (a sequence has gaps); hash = sha256(prev_hash || row_jcs), over the exact bytes kept. The signed
-- hourly checkpoint is v4.1.
create table audit (
    n          bigserial primary key,
    at         timestamptz not null default now(),
    prev_hash  bytea check (length(prev_hash) = 32),
    row_jcs    bytea not null,
    hash       bytea not null unique check (length(hash) = 32)
);

-- +goose Down
set local search_path to kontra;
drop table audit;
drop table calls;
drop table mcp_sessions;
drop table push_configs;
drop table work_items;
drop table task_events;
drop function task_events_append_only();
drop table attempts;
drop table tasks;
drop table api_keys;
drop table tunnels;
drop table gate_replicas;
drop table enrollment_tokens;
drop table key_services;
drop table tunnel_keys;
drop table services;
drop table tenants;
-- The schema stays: it also holds goose's version table (kontra.goose_db_version).
