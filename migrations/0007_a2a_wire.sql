-- The A2A wire (OpenSpec change console-a2a-protocol-view; D355-D358): both legs of every A2A call, optionally with
-- their bodies, and where each logged task event came from.
--
-- a2a_exchanges.leg: 'caller' is a call the gate received (every row before 0007), 'callee' a call the gate made to a
-- service's agent. On the callee leg callee_task_id and context_id are the agent's own ids (its task id is any string
-- it chose, so it is text, and task_id stays null) and gate_task_id is the gate task the call works for; on the caller
-- leg both are null (task_id already is the gate's).
-- a2a_version: the A2A version the call spoke ("1.0", "0.3"), when known.
-- request_body / response_body: kept only with traffic.bodies on (D358), cut at traffic.body_max (the *_truncated
-- flags, the *_size columns the full size), credential members redacted, never a header. A stream's answer is its
-- frames (a2a_exchange_frames) and response_body stays null; frames_dropped counts frames past the exchange's total.
--
-- a2a_exchange_frames: one SSE frame of a stream exchange, in the order sent, with its offset from the call's start
-- and its SSE id when it had one. Deleted with its exchange (the traffic retention sweep).
--
-- task_events.origin: who produced the event (D357): {actor: caller|callee|gate, type: the A2A type the actor sent,
-- method: the A2A method it travelled in, exchange_id?, converted_to?, callee_ids?, context_assigned?}. Null on rows
-- written before 0007.

-- +goose Up
set local search_path to kontra;

alter table a2a_exchanges
    add column leg                text not null default 'caller' check (leg in ('caller', 'callee')),
    add column gate_task_id       uuid,
    add column callee_task_id     text check (length(callee_task_id) between 1 and 256),
    add column a2a_version        text check (length(a2a_version) between 1 and 16),
    add column request_body       bytea,
    add column request_size       bigint check (request_size >= 0),
    add column request_truncated  boolean not null default false,
    add column response_body      bytea,
    add column response_size      bigint check (response_size >= 0),
    add column response_truncated boolean not null default false,
    add column frames_dropped     integer not null default 0 check (frames_dropped >= 0),
    add constraint gate_task_on_callee_leg check (leg = 'callee' or (gate_task_id is null and callee_task_id is null)),
    add constraint request_size_with_body check (request_body is null or request_size is not null),
    add constraint response_size_with_body check (response_body is null or response_size is not null);
create index a2a_exchanges_by_gate_task on a2a_exchanges (gate_task_id, at) where gate_task_id is not null;
create index a2a_exchanges_by_context on a2a_exchanges (context_id, at desc) where context_id is not null;

create table a2a_exchange_frames (
    exchange_id  uuid not null references a2a_exchanges (id) on delete cascade,
    n            integer not null check (n >= 0),          -- order sent, from 0
    offset_ms    integer not null check (offset_ms >= 0),  -- from the call's start
    sse_id       text check (length(sse_id) between 1 and 64),
    data         bytea not null,
    size         bigint not null check (size >= 0),        -- the frame's full size
    truncated    boolean not null default false,
    primary key (exchange_id, n)
);

alter table task_events add column origin json;

-- +goose Down
-- Drops the callee leg, the bodies and the events' origins: the record is metadata only again (D353).
set local search_path to kontra;
alter table task_events drop column origin;
drop table a2a_exchange_frames;
delete from a2a_exchanges where leg = 'callee';
drop index a2a_exchanges_by_context;
drop index a2a_exchanges_by_gate_task;
alter table a2a_exchanges
    drop constraint response_size_with_body,
    drop constraint request_size_with_body,
    drop constraint gate_task_on_callee_leg,
    drop column frames_dropped,
    drop column response_truncated,
    drop column response_size,
    drop column response_body,
    drop column request_truncated,
    drop column request_size,
    drop column request_body,
    drop column a2a_version,
    drop column callee_task_id,
    drop column gate_task_id,
    drop column leg;
