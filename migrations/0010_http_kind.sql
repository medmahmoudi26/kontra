-- Kontra gate schema, revision 0010 (spike http-tunnel): a service of kind http, a raw HTTP service a node
-- serves and the gate passes through at its own host, WebSocket upgrades included.

-- +goose Up
set local search_path to kontra;

alter table services drop constraint services_kind_check;
alter table services add constraint services_kind_check check (kind in ('a2a', 'mcp', 'http'));

-- +goose Down
set local search_path to kontra;

delete from services where kind = 'http';
alter table services drop constraint services_kind_check;
alter table services add constraint services_kind_check check (kind in ('a2a', 'mcp'));
