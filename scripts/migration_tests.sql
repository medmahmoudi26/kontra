-- Behaviour the schema itself enforces. Runs after `up`, in one transaction that is rolled back.
set local search_path to kontra;

insert into tenants (id, slug, kind) values ('01900000-0000-7000-8000-000000000001', 'default', 'team');
insert into services (tenant_id, name, kind, backend, source)
    values ('01900000-0000-7000-8000-000000000001', 'hello', 'a2a', '{"tunnel": {}}', 'config');
insert into tasks (id, tenant_id, service, protocol, buyer_id, principal, via, context_id, state, version,
                   snapshot, status_at, first_message_id, first_request)
    values ('01900000-0000-7000-8000-0000000000aa', '01900000-0000-7000-8000-000000000001', 'hello', 'a2a',
            '01900000-0000-7000-8000-0000000000bb', 'key:kk_test', 'api_key', 'ctx-1', 'submitted', 1,
            '{"id": "x"}', now(), 'm-1', sha256('r'));

-- A NUL character in relayed text is stored as is (json, not jsonb).
insert into task_events (task_id, seq, kind, payload)
    values ('01900000-0000-7000-8000-0000000000aa', 1, 'task', '{"text": "a\u0000b"}');

-- So is a NUL character in a served card (services.card is json too, D49).
update services set card = '{"name": "a\u0000b"}' where name = 'hello';

-- task_events is append-only.
do $$ begin
    update task_events set kind = 'status' where seq = 1;
    raise exception 'FAIL: task_events accepted an update';
exception when others then
    if sqlerrm like 'FAIL:%' then raise; end if;
end $$;

-- An enrollment token's scope and service agree; a token lives at most 15 minutes.
do $$ begin
    insert into enrollment_tokens (id, tenant_id, scope, service, token_hash, expires_at)
        values ('01900000-0000-7000-8000-0000000000c1', '01900000-0000-7000-8000-000000000001', 'tenant', 'hello', sha256('t1'), now() + interval '5 minutes');
    raise exception 'FAIL: a tenant-scoped token named a service';
exception when check_violation then null; end $$;
do $$ begin
    insert into enrollment_tokens (id, tenant_id, scope, service, token_hash, expires_at)
        values ('01900000-0000-7000-8000-0000000000c2', '01900000-0000-7000-8000-000000000001', 'service', 'hello', sha256('t2'), now() + interval '1 hour');
    raise exception 'FAIL: a token outlived 15 minutes';
exception when check_violation then null; end $$;

-- A terminal state needs terminal_at; a quoted task needs an expiry.
do $$ begin
    update tasks set state = 'completed' where id = '01900000-0000-7000-8000-0000000000aa';
    raise exception 'FAIL: completed without terminal_at';
exception when check_violation then null; end $$;
do $$ begin
    update tasks set state = 'quoted' where id = '01900000-0000-7000-8000-0000000000aa';
    raise exception 'FAIL: quoted without quote_expires_at';
exception when check_violation then null; end $$;

-- An attempt records its first upstream id only together with the id.
do $$ begin
    insert into attempts (task_id, n, backend, first_upstream_id_at)
        values ('01900000-0000-7000-8000-0000000000aa', 1, 'direct', now());
    raise exception 'FAIL: first_upstream_id_at without upstream_task_id';
exception when check_violation then null; end $$;

-- A running tunnel attempt names its session, key and replica (a NULL outcome does not slip through).
do $$ begin
    insert into attempts (task_id, n, backend) values ('01900000-0000-7000-8000-0000000000aa', 2, 'tunnel');
    raise exception 'FAIL: a running tunnel attempt without a session';
exception when check_violation then null; end $$;
insert into attempts (task_id, n, backend, outcome) values ('01900000-0000-7000-8000-0000000000aa', 3, 'tunnel', 'no_session');

-- An attempt's dispatch token is a nullable uuid: null until a dispatch writes one (D77).
do $$ begin
    if not exists (select 1 from information_schema.columns
                    where table_schema = 'kontra' and table_name = 'attempts' and column_name = 'dispatch_id'
                      and data_type = 'uuid' and is_nullable = 'YES') then
        raise exception 'FAIL: attempts.dispatch_id is not a nullable uuid';
    end if;
    if (select dispatch_id from attempts where task_id = '01900000-0000-7000-8000-0000000000aa' and n = 3) is not null then
        raise exception 'FAIL: attempts.dispatch_id has a default';
    end if;
end $$;

-- An elicitation id comes with the session that received it.
do $$ begin
    update tasks set elicitation_id = 'el-1' where id = '01900000-0000-7000-8000-0000000000aa';
    raise exception 'FAIL: elicitation_id without elicitation_session';
exception when check_violation then null; end $$;

-- A service's MCP session has a tenant; the gate's own /mcp may have none.
do $$ begin
    insert into mcp_sessions (client_sid_hash, service, buyer_id, principal, via, protocol_version)
        values (sha256('s1'), 'hello', '01900000-0000-7000-8000-0000000000bb', 'key:kk_test', 'api_key', '2025-11-25');
    raise exception 'FAIL: a service MCP session without a tenant';
exception when check_violation then null; end $$;
insert into mcp_sessions (client_sid_hash, buyer_id, principal, via, protocol_version)
    values (sha256('s2'), '01900000-0000-7000-8000-0000000000bb', 'oidc:https://idp.example/#u1', 'oidc', '2025-11-25');

-- An API key defaults to the hire scope; prefixes are unique.
insert into api_keys (id, tenant_id, buyer_id, prefix, hash)
    values ('01900000-0000-7000-8000-0000000000d1', '01900000-0000-7000-8000-000000000001', '01900000-0000-7000-8000-0000000000bb', 'kk_test', sha256('k1'));
do $$ begin
    if (select scopes from api_keys where prefix = 'kk_test') <> '{hire}' then
        raise exception 'FAIL: api_keys.scopes default is not {hire}';
    end if;
end $$;
do $$ begin
    insert into api_keys (id, tenant_id, buyer_id, prefix, hash)
        values ('01900000-0000-7000-8000-0000000000d2', '01900000-0000-7000-8000-000000000001', '01900000-0000-7000-8000-0000000000bb', 'kk_test', sha256('k2'));
    raise exception 'FAIL: a duplicate api key prefix';
exception when unique_violation then null; end $$;

-- Sealing is off, optional or required; nothing else.
do $$ begin
    update services set sealed = 'true' where name = 'hello';
    raise exception 'FAIL: services.sealed accepted true';
exception when check_violation then null; end $$;

-- 0002: a blob ref is 26 base32 characters, an object is never empty, and a row names an existing task.
insert into blobs (ref, tenant_id, task_id, size)
    values ('aaaaaaaaaaaaaaaaaaaaaaaaaa', '01900000-0000-7000-8000-000000000001', '01900000-0000-7000-8000-0000000000aa', 300000);
do $$ begin
    insert into blobs (ref, tenant_id, task_id, size)
        values ('Not-A-Ref', '01900000-0000-7000-8000-000000000001', '01900000-0000-7000-8000-0000000000aa', 1);
    raise exception 'FAIL: blobs accepted a malformed ref';
exception when check_violation then null; end $$;
do $$ begin
    insert into blobs (ref, tenant_id, task_id, size)
        values ('bbbbbbbbbbbbbbbbbbbbbbbbbb', '01900000-0000-7000-8000-000000000001', '01900000-0000-7000-8000-0000000000aa', 0);
    raise exception 'FAIL: blobs accepted an empty object';
exception when check_violation then null; end $$;
do $$ begin
    insert into blobs (ref, tenant_id, task_id, size)
        values ('cccccccccccccccccccccccccc', '01900000-0000-7000-8000-000000000001', '01900000-0000-7000-8000-0000000000ff', 1);
    raise exception 'FAIL: a blob of a task that does not exist';
exception when foreign_key_violation then null; end $$;

-- 0002: a replica's peer address is optional.
insert into gate_replicas (replica_id, version, peer_addr) values ('r-peer', 'test', '10.0.0.7:8444');
insert into gate_replicas (replica_id, version) values ('r-nopeer', 'test');

-- 0002 (D177): two open 2026-07-28 sessions of one principal on one service conflict, the gate's own /mcp
-- (service null) included; a closed one, or an older revision's, does not.
insert into mcp_sessions (client_sid_hash, tenant_id, service, buyer_id, principal, via, protocol_version)
    values (sha256('v1'), '01900000-0000-7000-8000-000000000001', 'hello', '01900000-0000-7000-8000-0000000000bb', 'key:kk_test', 'api_key', '2026-07-28');
do $$ begin
    insert into mcp_sessions (client_sid_hash, tenant_id, service, buyer_id, principal, via, protocol_version)
        values (sha256('v2'), '01900000-0000-7000-8000-000000000001', 'hello', '01900000-0000-7000-8000-0000000000bb', 'key:kk_test', 'api_key', '2026-07-28');
    raise exception 'FAIL: two open virtual sessions for one principal and service';
exception when unique_violation then null; end $$;
insert into mcp_sessions (client_sid_hash, tenant_id, service, buyer_id, principal, via, protocol_version, closed_at, close_reason)
    values (sha256('v3'), '01900000-0000-7000-8000-000000000001', 'hello', '01900000-0000-7000-8000-0000000000bb', 'key:kk_test', 'api_key', '2026-07-28', now(), 'idle');
insert into mcp_sessions (client_sid_hash, tenant_id, service, buyer_id, principal, via, protocol_version)
    values (sha256('v4'), '01900000-0000-7000-8000-000000000001', 'hello', '01900000-0000-7000-8000-0000000000bb', 'key:kk_test', 'api_key', '2025-11-25');
insert into mcp_sessions (client_sid_hash, buyer_id, principal, via, protocol_version)
    values (sha256('v5'), '01900000-0000-7000-8000-0000000000bb', 'key:kk_own', 'api_key', '2026-07-28');
do $$ begin
    insert into mcp_sessions (client_sid_hash, buyer_id, principal, via, protocol_version)
        values (sha256('v6'), '01900000-0000-7000-8000-0000000000bb', 'key:kk_own', 'api_key', '2026-07-28');
    raise exception 'FAIL: two open virtual sessions of the gate''s own /mcp for one principal';
exception when unique_violation then null; end $$;

-- 0002 (D213, D231): a service caps its principals' MCP sessions at 4 unless set, and at 1 to 10000.
do $$ begin
    if (select max_sessions_per_principal from services where name = 'hello') <> 4 then
        raise exception 'FAIL: max_sessions_per_principal does not default to 4';
    end if;
end $$;
do $$ begin
    update services set max_sessions_per_principal = 0 where name = 'hello';
    raise exception 'FAIL: a cap of 0 sessions per principal';
exception when check_violation then null; end $$;
do $$ begin
    update services set max_sessions_per_principal = 10001 where name = 'hello';
    raise exception 'FAIL: a cap over 10000 sessions per principal';
exception when check_violation then null; end $$;

-- 0002: a task's blobs go with it.
delete from tasks where id = '01900000-0000-7000-8000-0000000000aa';
do $$ begin
    if exists (select 1 from blobs) then
        raise exception 'FAIL: a deleted task left its blobs rows';
    end if;
end $$;

-- 0003 (R §12.2, D272): a task is not hired unless the gate's /mcp marks it; hired tasks are indexed by buyer.
insert into tasks (id, tenant_id, service, protocol, buyer_id, principal, via, context_id, state, version,
                   snapshot, status_at)
    values ('01900000-0000-7000-8000-0000000000e1', '01900000-0000-7000-8000-000000000001', 'hello', 'a2a',
            '01900000-0000-7000-8000-0000000000bb', 'key:kk_test', 'api_key', 'ctx-h', 'submitted', 1, '{"id": "h"}', now());
do $$ begin
    if (select hired from tasks where id = '01900000-0000-7000-8000-0000000000e1') then
        raise exception 'FAIL: tasks.hired defaults to true';
    end if;
    if to_regclass('kontra.tasks_hired') is null then
        raise exception 'FAIL: no tasks_hired index';
    end if;
end $$;
update tasks set hired = true where id = '01900000-0000-7000-8000-0000000000e1';

-- 0003 (R §12.3, D288): a notification is claimed once, and only for a task that sent a URL elicitation.
do $$ begin
    update tasks set elicitation_notified_at = now() where id = '01900000-0000-7000-8000-0000000000e1';
    raise exception 'FAIL: elicitation_notified_at without elicitation_id';
exception when check_violation then null; end $$;
update tasks set elicitation_id = 'el-h', elicitation_session = sha256('s-h') where id = '01900000-0000-7000-8000-0000000000e1';
do $$
declare first int; second int;
begin
    update tasks set elicitation_notified_at = now()
        where id = '01900000-0000-7000-8000-0000000000e1' and elicitation_notified_at is null;
    get diagnostics first = row_count;
    update tasks set elicitation_notified_at = now()
        where id = '01900000-0000-7000-8000-0000000000e1' and elicitation_notified_at is null;
    get diagnostics second = row_count;
    if first <> 1 or second <> 0 then
        raise exception 'FAIL: the notification claim won % then % times', first, second;
    end if;
end $$;

-- 0003 (T §6.1, D287): a tenant is anonymous if and only if its slug is anon- and 16 lowercase hex digits.
insert into tenants (id, slug, kind, expires_at, created_from)
    values ('01900000-0000-7000-8000-0000000000f1', 'anon-0123456789abcdef', 'anonymous', now() + interval '2 hours', '198.51.100.0/24');
do $$ begin
    insert into tenants (id, slug, kind) values ('01900000-0000-7000-8000-0000000000f2', 'anon-fedcba9876543210', 'account');
    raise exception 'FAIL: an account took an anonymous slug';
exception when check_violation then null; end $$;
do $$ begin
    insert into tenants (id, slug, kind) values ('01900000-0000-7000-8000-0000000000f3', 'anon-fedcba9876543210', 'team');
    raise exception 'FAIL: a team took an anonymous slug';
exception when check_violation then null; end $$;
do $$ begin
    insert into tenants (id, slug, kind, expires_at, created_from)
        values ('01900000-0000-7000-8000-0000000000f4', 'anon-7f3a', 'anonymous', now() + interval '2 hours', '198.51.100.0/24');
    raise exception 'FAIL: an anonymous tenant with a short slug';
exception when check_violation then null; end $$;
do $$ begin
    insert into tenants (id, slug, kind, expires_at, created_from)
        values ('01900000-0000-7000-8000-0000000000f5', 'acme2', 'anonymous', now() + interval '2 hours', '2001:db8:1::/48');
    raise exception 'FAIL: an anonymous tenant without the anon- slug';
exception when check_violation then null; end $$;
do $$ begin
    update tenants set kind = 'account', expires_at = null where id = '01900000-0000-7000-8000-0000000000f1';
    raise exception 'FAIL: an anonymous tenant became an account under its anon- slug';
exception when check_violation then null; end $$;
insert into tenants (id, slug, kind) values ('01900000-0000-7000-8000-0000000000f6', 'anonymous-users', 'account');
do $$ begin
    if to_regclass('kontra.tenants_anonymous_network') is null then
        raise exception 'FAIL: no tenants_anonymous_network index';
    end if;
end $$;

-- 0003 (T §6.2, D287): an anonymous call's row goes with its replica (a replica that dies frees its slots) and with
-- its tenant.
insert into gate_replicas (replica_id, version) values ('r-anon-calls', 'test');
insert into anon_calls (id, tenant_id, replica_id)
    values ('01900000-0000-7000-8000-0000000000c1', '01900000-0000-7000-8000-0000000000f1', 'r-anon-calls');
delete from gate_replicas where replica_id = 'r-anon-calls';
do $$ begin
    if exists (select 1 from anon_calls where id = '01900000-0000-7000-8000-0000000000c1') then
        raise exception 'FAIL: an anonymous call row outlived its replica';
    end if;
end $$;

-- 0004 (R §13.5, D317): kontra.audit_append links a row as internal/audit does, sha256(prev_hash || row_jcs), under
-- the chain's lock, and returns the hash; the first row has no prev_hash.
do $$
declare h1 bytea; h2 bytea; r record;
begin
    h1 := kontra.audit_append(convert_to('{"at":"2026-11-01T00:00:00.000000Z","event":"test.one"}', 'UTF8'));
    h2 := kontra.audit_append(convert_to('{"at":"2026-11-01T00:00:01+01:00","event":"test.two","n":2}', 'UTF8'));
    select * into r from audit order by n limit 1;
    if r.prev_hash is not null or r.hash <> h1 or h1 <> sha256(r.row_jcs) then
        raise exception 'FAIL: the first appended row is not linked as the chain links it';
    end if;
    select * into r from audit order by n desc limit 1;
    if r.prev_hash <> h1 or r.hash <> h2 or h2 <> sha256(h1 || r.row_jcs) then
        raise exception 'FAIL: the second appended row is not sha256(prev || row)';
    end if;
end $$;
-- What it refuses: nothing appended for each.
do $$
declare bad text; before bigint;
begin
    select count(*) into before from audit;
    foreach bad in array array['', '[1]', '"x"', '{"event":"x"}', '{"at":"2026-11-01T00:00:00Z"}', '{"event":"","at":"2026-11-01T00:00:00Z"}',
        '{"event":1,"at":"2026-11-01T00:00:00Z"}', '{"event":"x","at":"yesterday"}', '{"event":"x","at":"2026-11-01 00:00:00Z"}', '{"event":"x",'] loop
        begin
            perform kontra.audit_append(convert_to(bad, 'UTF8'));
            raise exception 'FAIL: audit_append accepted %', bad;
        exception when invalid_parameter_value then null;
        end;
    end loop;
    begin
        perform kontra.audit_append(null);
        raise exception 'FAIL: audit_append accepted null';
    exception when invalid_parameter_value then null;
    end;
    begin
        perform kontra.audit_append('\xff7b7d'::bytea);
        raise exception 'FAIL: audit_append accepted bytes that are not UTF-8';
    exception when invalid_parameter_value then null;
    end;
    if (select count(*) from audit) <> before then
        raise exception 'FAIL: a refused row was appended';
    end if;
end $$;
-- No two rows name the same predecessor, and only the first names none (audit_prev_hash).
do $$ begin
    insert into audit (prev_hash, row_jcs, hash) values ((select hash from audit order by n limit 1), 'x', sha256('fork'));
    raise exception 'FAIL: a second row linked to the first row';
exception when unique_violation then null; end $$;
do $$ begin
    insert into audit (prev_hash, row_jcs, hash) values (null, 'x', sha256('second first'));
    raise exception 'FAIL: a second row without a predecessor';
exception when unique_violation then null; end $$;
-- The function runs as its owner and nobody else may call it unless granted.
do $$ begin
    if has_function_privilege('public', 'kontra.audit_append(bytea)', 'execute') then
        raise exception 'FAIL: PUBLIC may execute kontra.audit_append';
    end if;
    if not (select prosecdef from pg_proc where oid = 'kontra.audit_append(bytea)'::regprocedure) then
        raise exception 'FAIL: kontra.audit_append is not security definer';
    end if;
end $$;

select 'migration behaviour: ok';
