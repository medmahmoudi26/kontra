-- Kontra gate schema, revision 0009 (the caller/callee vocabulary; OpenSpec change caller-callee-rename).
--
-- A rename only: no table is rewritten and no behaviour changes. Column and index renames are catalog changes,
-- and column privileges follow a renamed column. The old and the new gate cannot share this schema: stop every
-- replica, migrate, then start the new one.
--
--   buyer_id (api_keys, tasks, mcp_sessions, calls)  -> caller_id
--   tasks.hired                                      -> tasks.sent_by_mcp (created by /mcp's send_message tool)
--   tunnels.hire_principal                           -> tunnels.caller_principal
--   services.seller_keys                             -> services.service_keys
--   api_keys.scopes: the scope hire                  -> call (the default and every stored key)
--
-- Two stored values follow the names: a queued work item's owner (work_items.payload owner.buyerId, read back by
-- the gate when it dispatches) and the traffic method of the service key set route (a2a_exchanges.method seller_jwks).
-- The audit chain and task_events.origin are history and keep what they recorded.

-- +goose Up
set local search_path to kontra;

alter table api_keys rename column buyer_id to caller_id;
alter table tasks rename column buyer_id to caller_id;
alter table mcp_sessions rename column buyer_id to caller_id;
alter table calls rename column buyer_id to caller_id;
alter table tasks rename column hired to sent_by_mcp;
alter table tunnels rename column hire_principal to caller_principal;
alter table services rename column seller_keys to service_keys;

alter index api_keys_by_buyer rename to api_keys_by_caller;
alter index calls_by_buyer rename to calls_by_caller;
alter index tasks_by_buyer rename to tasks_by_caller_id;
alter index tasks_hired rename to tasks_sent_by_mcp;
alter index tasks_hired_by_message rename to tasks_sent_by_mcp_by_message;

alter table api_keys alter column scopes set default '{call}';
update api_keys set scopes = array_replace(scopes, 'hire', 'call') where 'hire' = any(scopes);

update work_items
   set payload = jsonb_set(payload::jsonb #- '{owner,buyerId}', '{owner,callerId}', payload::jsonb #> '{owner,buyerId}')::json
 where payload::jsonb #> '{owner}' ? 'buyerId';
update a2a_exchanges set method = 'service_jwks' where method = 'seller_jwks';

-- +goose Down
set local search_path to kontra;

update a2a_exchanges set method = 'seller_jwks' where method = 'service_jwks';
update work_items
   set payload = jsonb_set(payload::jsonb #- '{owner,callerId}', '{owner,buyerId}', payload::jsonb #> '{owner,callerId}')::json
 where payload::jsonb #> '{owner}' ? 'callerId';

update api_keys set scopes = array_replace(scopes, 'call', 'hire') where 'call' = any(scopes);
alter table api_keys alter column scopes set default '{hire}';

alter index tasks_sent_by_mcp_by_message rename to tasks_hired_by_message;
alter index tasks_sent_by_mcp rename to tasks_hired;
alter index tasks_by_caller_id rename to tasks_by_buyer;
alter index calls_by_caller rename to calls_by_buyer;
alter index api_keys_by_caller rename to api_keys_by_buyer;

alter table services rename column service_keys to seller_keys;
alter table tunnels rename column caller_principal to hire_principal;
alter table tasks rename column sent_by_mcp to hired;
alter table calls rename column caller_id to buyer_id;
alter table mcp_sessions rename column caller_id to buyer_id;
alter table tasks rename column caller_id to buyer_id;
alter table api_keys rename column caller_id to buyer_id;
