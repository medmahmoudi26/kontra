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

-- 0002: a task's blobs go with it.
delete from tasks where id = '01900000-0000-7000-8000-0000000000aa';
do $$ begin
    if exists (select 1 from blobs) then
        raise exception 'FAIL: a deleted task left its blobs rows';
    end if;
end $$;

select 'migration behaviour: ok';
