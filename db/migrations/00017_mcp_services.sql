-- +goose Up
-- Hosted MCP service definitions owned by this host. Columns that queries
-- select on are real columns; the rest of the definition is the JSON record.
CREATE TABLE mcp_services (
    id TEXT PRIMARY KEY CHECK (length(id) = 32),
    name TEXT NOT NULL UNIQUE CHECK (length(name) BETWEEN 1 AND 63),
    revision INTEGER NOT NULL CHECK (revision > 0),
    transport TEXT NOT NULL CHECK (transport IN ('stdio', 'http')),
    record TEXT NOT NULL CHECK (json_valid(record))
) STRICT;

-- Application instances of those services, with the launch and stop intent
-- that recovery needs to adopt or refuse an exact worker.
CREATE TABLE mcp_instances (
    id TEXT PRIMARY KEY CHECK (length(id) = 32),
    service_id TEXT NOT NULL,
    record TEXT NOT NULL CHECK (json_valid(record))
) STRICT;
CREATE INDEX mcp_instances_service ON mcp_instances (service_id);

-- +goose Down
DROP INDEX mcp_instances_service;
DROP TABLE mcp_instances;
DROP TABLE mcp_services;
