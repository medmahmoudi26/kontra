-- Kontra gate schema, revision 0004 (M5).
--
-- Three changes: an operator's appends to the audit chain (D317), the index of a buyer's tasks (D319) and the MCP task
-- mode required (D322), the last two below the function. Every M5 schema change is in this one file, frozen when M5
-- merges.
--
-- An operator's appends to the gate's audit chain (PRD §12; R §13.5; D317). Cloud's control plane writes the gate's
-- services, enrollment_tokens and api_keys rows and revokes keys and bindings, and each such change must be a row of
-- the chain internal/audit keeps (migrations/0001_init.sql audit). kontra.audit_append(row_jcs) appends one, in the
-- caller's transaction, exactly as internal/audit's Append links it: under the transaction-level advisory lock
-- 0x6b6f6e7472610100 (audit.LockID; migrate's is ...0001, and an operator's own locks must avoid both), prev_hash = the
-- hash of the last row by n (null for the first row), hash = sha256(prev_hash || row_jcs) over the bytes given, and
-- returns that hash. The gate's own appends keep using internal/audit; both writers take the one lock, so they make
-- one chain, which audit.Verify checks. Every M5 schema change is in this one file, frozen when M5 merges. Section
-- references are to spec/relay.md (R).
--
-- What the function checks of row_jcs: not empty, UTF-8 JSON, an object with a non-empty string "event" and an "at"
-- in RFC 3339 form (every row carries what happened and when, under the hash). It does not re-canonicalize: the writer
-- sends the row's RFC 8785 (JCS) bytes, and the chain hashes and keeps exactly those. The at column is the database's
-- clock under the lock, when the row was appended; the row's own "at" is the writer's time of its change. The
-- function refuses a transaction above read committed: its snapshot could miss the last committed row, and the row it
-- linked would fork the chain.
--
-- audit_prev_hash makes a fork impossible whoever writes: no two rows may name the same predecessor, and only one row
-- may have none. A writer that links to a stale head (a raw INSERT, or a repeatable read transaction) fails with a
-- unique violation instead of breaking the chain. Building it takes a SHARE lock on audit, which holds the appends of
-- replicas still serving for one read of that table.
--
-- Grants (the operator's, R §13.5). The function is SECURITY DEFINER: it runs as its owner, the role that ran
-- `kontra-gate migrate`, which owns kontra.audit; its search_path is pinned and every name in it qualified. EXECUTE is
-- revoked from PUBLIC. An operator's role that appends needs
--     grant usage on schema kontra to <role>;
--     grant execute on function kontra.audit_append(bytea) to <role>;
-- and no privilege on kontra.audit itself, so it can append only through the function, linked: grant it select on
-- kontra.audit only to read or verify the chain, and never insert, update or delete. A raw insert of a role that still
-- holds INSERT (an operator's interim writer) takes the same lock or meets the unique index.

-- +goose Up
set local search_path to kontra;

create unique index audit_prev_hash on audit (prev_hash) nulls not distinct;

-- +goose StatementBegin
create function audit_append(row_jcs bytea) returns bytea
    language plpgsql volatile security definer
    set search_path = pg_catalog, pg_temp
as $$
declare
    doc  json;
    prev bytea;
    h    bytea;
begin
    if row_jcs is null or length(row_jcs) = 0 then
        raise exception 'kontra.audit_append: row_jcs is empty' using errcode = 'invalid_parameter_value';
    end if;
    if current_setting('transaction_isolation') not in ('read committed', 'read uncommitted') then
        raise exception 'kontra.audit_append: the transaction is %, want read committed (a later snapshot could fork the chain)',
            current_setting('transaction_isolation') using errcode = 'invalid_transaction_state';
    end if;
    begin
        doc := convert_from(row_jcs, 'UTF8')::json;
    exception when others then
        raise exception 'kontra.audit_append: row_jcs is not UTF-8 JSON' using errcode = 'invalid_parameter_value';
    end;
    if json_typeof(doc) is distinct from 'object'
        or json_typeof(doc -> 'event') is distinct from 'string' or (doc ->> 'event') = ''
        or json_typeof(doc -> 'at') is distinct from 'string'
        or (doc ->> 'at') !~ '^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d{1,9})?(Z|[+-]\d\d:\d\d)$' then
        raise exception 'kontra.audit_append: row_jcs is not an object with a string "event" and an RFC 3339 "at"'
            using errcode = 'invalid_parameter_value';
    end if;
    perform pg_catalog.pg_advisory_xact_lock(x'6b6f6e7472610100'::bit(64)::bigint);
    select a.hash into prev from kontra.audit a order by a.n desc limit 1;
    h := pg_catalog.sha256(coalesce(prev, ''::bytea) || row_jcs);
    insert into kontra.audit (at, prev_hash, row_jcs, hash) values (pg_catalog.clock_timestamp(), prev, row_jcs, h);
    return h;
end
$$;
-- +goose StatementEnd
revoke all on function audit_append(bytea) from public;

-- R §2.3, §13.5 (D319; Cloud's D-M5-120): a buyer's tasks across services, newest first. tasks_by_caller and
-- tasks_by_context lead with (tenant_id, service), and tasks_hired is partial on hired, so a read of one buyer's tasks
-- (an operator's: a party's API keys', its OIDC subject's and, by the node buyers R §2.3 writes, its nodes') walked the
-- table. Building it takes a SHARE lock on tasks for one scan (goose runs the file in one transaction, where CREATE
-- INDEX CONCURRENTLY cannot run): the task writes of replicas still serving wait for it, as for 0003's tasks_hired.
create index tasks_by_buyer on tasks (buyer_id, id desc);

-- R §11.5 (D322; issue #6): tasks: required, durable that refuses a plain tools/call, so every call of the service is a
-- gate task Admission sees. Adding the check reads services once.
alter table services drop constraint services_tasks_mode_check;
alter table services add constraint services_tasks_mode_check
    check (tasks_mode in ('durable', 'required', 'passthrough', 'off'));

-- +goose Down
set local search_path to kontra;
-- A required service becomes durable, the mode required refines: it still owns its tasks, and takes plain calls again.
update services set tasks_mode = 'durable' where tasks_mode = 'required';
alter table services drop constraint services_tasks_mode_check;
alter table services add constraint services_tasks_mode_check check (tasks_mode in ('durable', 'passthrough', 'off'));
drop index tasks_by_buyer;
drop function audit_append(bytea);
drop index audit_prev_hash;
