-- +goose Up
-- Old app readers treat absent visibility as public. Keep narrowed private state
-- outside their readable table, including after an executable-only rollback.
ALTER TABLE app_state RENAME TO private_app_state;

-- +goose Down
-- Downgrades with owner state must fail closed rather than restore sharing.
CREATE TEMP TABLE private_app_downgrade_guard (
    state_is_empty INTEGER NOT NULL CHECK (state_is_empty = 1)
);
INSERT INTO private_app_downgrade_guard
    SELECT NOT EXISTS (SELECT 1 FROM private_app_state);
DROP TABLE private_app_downgrade_guard;
ALTER TABLE private_app_state RENAME TO app_state;
