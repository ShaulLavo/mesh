-- +goose Up
DROP TABLE edge_routes;
DROP TABLE edge_snapshots;
DROP TABLE edge_outbox;
DROP TABLE tunnel_claims;
DROP TABLE tunnel_highwater;
DROP TABLE tunnel_outbox;
ALTER TABLE services DROP COLUMN public_name;
ALTER TABLE services DROP COLUMN wake_on_request;
ALTER TABLE cached_services DROP COLUMN public_name;
ALTER TABLE cached_services DROP COLUMN wake_on_request;
ALTER TABLE app_names RENAME COLUMN public_name TO hostname;
