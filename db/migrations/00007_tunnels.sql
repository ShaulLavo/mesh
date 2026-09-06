-- +goose Up
CREATE TABLE tunnel_highwater (
    claimant_id TEXT PRIMARY KEY CHECK (length(claimant_id) = 43),
    sequence INTEGER NOT NULL CHECK (sequence > 0),
    digest TEXT NOT NULL CHECK (length(digest) = 64)
) STRICT;

CREATE TABLE tunnel_claims (
    public_name TEXT PRIMARY KEY CHECK (length(public_name) BETWEEN 1 AND 253),
    claimant_id TEXT NOT NULL REFERENCES tunnel_highwater(claimant_id)
) STRICT;

CREATE INDEX tunnel_claims_by_owner ON tunnel_claims(claimant_id);

CREATE TABLE tunnel_outbox (
    target_id TEXT NOT NULL CHECK (length(target_id) = 43),
    claimant_id TEXT NOT NULL CHECK (length(claimant_id) = 43),
    sequence INTEGER NOT NULL CHECK (sequence > 0),
    action TEXT,
    public_name TEXT,
    canonical BLOB,
    digest TEXT,
    signature BLOB,
    PRIMARY KEY (target_id, claimant_id),
    CHECK (
        (action IS NULL AND public_name IS NULL AND canonical IS NULL AND digest IS NULL AND signature IS NULL)
        OR
        (action IS NOT NULL AND public_name IS NOT NULL AND canonical IS NOT NULL AND digest IS NOT NULL AND signature IS NOT NULL
         AND action IN ('create', 'release') AND length(public_name) BETWEEN 1 AND 253
         AND length(canonical) BETWEEN 1 AND 4032 AND length(digest) = 64 AND length(signature) = 64)
    )
) STRICT;

-- +goose Down
DROP TABLE tunnel_outbox;
DROP INDEX tunnel_claims_by_owner;
DROP TABLE tunnel_claims;
DROP TABLE tunnel_highwater;
