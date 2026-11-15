-- +goose Up

-- Idempotency keys, scoped per merchant (docs/adr/0004-idempotency.md).
--
-- A key is stored only once a request has started executing, so a request
-- rejected before that (bad auth, bad input, key in use) never uses it up.
-- Single-transaction endpoints insert the row already 'finished', in the same
-- transaction as their effects. Authorize, which calls the acquirer between
-- transactions, first stores 'started' with a lock and finishes later.
CREATE TABLE idempotency_keys (
    merchant_id     text        NOT NULL REFERENCES merchants (id),
    key             text        NOT NULL CHECK (length(key) BETWEEN 1 AND 255),
    request_method  text        NOT NULL,
    request_path    text        NOT NULL,
    -- SHA-256 of method, path and canonical JSON body.
    request_hash    bytea       NOT NULL CHECK (length(request_hash) = 32),
    recovery_point  text        NOT NULL CHECK (recovery_point IN ('started', 'finished')),
    -- While a multi-transaction request runs: when it took the key, and a
    -- random token it must still hold to finish (a fencing token, so a request
    -- whose lock went stale and was taken over cannot overwrite the result).
    locked_at       timestamptz,
    lock_token      text,
    payment_id      text,
    response_status integer,
    response_body   bytea,
    created_at      timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL DEFAULT now() + interval '24 hours',
    PRIMARY KEY (merchant_id, key),
    CONSTRAINT idempotency_keys_finished_has_response
        CHECK ((recovery_point = 'finished') = (response_status IS NOT NULL AND response_body IS NOT NULL)),
    CONSTRAINT idempotency_keys_lock_only_while_started
        CHECK (recovery_point = 'started' OR (locked_at IS NULL AND lock_token IS NULL)),
    CONSTRAINT idempotency_keys_lock_pair
        CHECK ((locked_at IS NULL) = (lock_token IS NULL))
);

CREATE INDEX idempotency_keys_expires_at_idx ON idempotency_keys (expires_at);

-- The transactional outbox (docs/adr/0005-outbox.md, M3): every committed
-- payment state change inserts exactly one event in the same transaction, so
-- an event exists if and only if the change committed.
CREATE TABLE events (
    id          text        PRIMARY KEY,
    merchant_id text        NOT NULL REFERENCES merchants (id),
    type        text        NOT NULL,
    payment_id  text        REFERENCES payments (id),
    data        jsonb       NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX events_merchant_idx ON events (merchant_id, created_at, id);
CREATE INDEX events_payment_idx ON events (payment_id);

CREATE TRIGGER events_append_only BEFORE UPDATE OR DELETE ON events
    FOR EACH ROW EXECUTE FUNCTION ledger_reject_mutation();
CREATE TRIGGER events_no_truncate BEFORE TRUNCATE ON events
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_reject_mutation();

-- +goose Down
DROP TABLE events;
DROP TABLE idempotency_keys;
