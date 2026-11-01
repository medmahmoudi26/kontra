-- Behaviour that depends on the session, so it runs in a session of its own after migration_tests.sql (a fresh
-- backend), in one transaction that is rolled back.
--
-- 0004 (R §13.5, D317): PL/pgSQL lexes kontra.audit_append's string literals under the calling session's
-- standard_conforming_strings when a backend first runs the function, and keeps what it parsed for the backend's life.
-- With the setting off before that first call, a pattern written with backslashes would refuse an RFC 3339 "at" and
-- accept "dddd-dd-ddTdd:dd:ddZ"; the check holds under either setting.
set local standard_conforming_strings = off;
set local search_path to kontra;
do $$
declare before bigint;
begin
    select count(*) into before from audit;
    perform kontra.audit_append(convert_to('{"at":"2026-11-01T00:00:00Z","event":"test.session"}', 'UTF8'));
    perform kontra.audit_append(convert_to('{"at":"2026-11-01T00:00:01.5+01:00","event":"test.session"}', 'UTF8'));
    begin
        perform kontra.audit_append(convert_to('{"at":"dddd-dd-ddTdd:dd:ddZ","event":"test.session.bad"}', 'UTF8'));
        raise exception 'FAIL: with standard_conforming_strings off, audit_append accepted an "at" of letters';
    exception when invalid_parameter_value then null;
    end;
    if (select count(*) from audit) <> before + 2 then
        raise exception 'FAIL: with standard_conforming_strings off, audit_append did not append the two RFC 3339 rows';
    end if;
end $$;

select 'migration session behaviour: ok';
