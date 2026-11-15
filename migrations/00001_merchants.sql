-- +goose Up

-- Merchants are the API's tenants; every other table is scoped to one.
-- Each merchant has exactly one API key, and only its SHA-256 hash is stored.
-- The key is 256 bits of randomness, so a fast hash is enough: there is
-- nothing to brute-force, unlike a human-chosen password.
CREATE TABLE merchants (
    id           text        PRIMARY KEY,
    name         text        NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    api_key_hash bytea       NOT NULL UNIQUE CHECK (length(api_key_hash) = 32),
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE merchants;
