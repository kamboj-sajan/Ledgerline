-- +goose Up

-- The simulated card network. In production this would be someone else's
-- system; here it lives in its own schema so it can share the Compose
-- database. Ledgerline's own code never reads or writes these tables in its
-- transactions: the simulator commits on its own, like a remote service
-- would, so a crash between "the network authorized" and "we recorded it"
-- is a real possibility to test against.
CREATE SCHEMA acquirer;

CREATE TABLE acquirer.authorizations (
    -- The caller's idempotency key. Ledgerline sends its payment ID, so
    -- retrying an authorization can never create a second one.
    idempotency_key   text        PRIMARY KEY,
    -- The merchant-side reference (also our payment ID). Kept separately so
    -- a test can detect a caller that used a fresh key per retry: that would
    -- show up as two authorizations for one payment_reference.
    payment_reference text        NOT NULL,
    amount            bigint      NOT NULL,
    currency          text        NOT NULL,
    card_fingerprint  text        NOT NULL,
    approved          boolean     NOT NULL,
    reference         text        UNIQUE,
    decline_code      text,
    created_at        timestamptz NOT NULL DEFAULT now(),
    CHECK (approved = (reference IS NOT NULL)),
    CHECK (approved = (decline_code IS NULL))
);

CREATE INDEX authorizations_payment_reference_idx ON acquirer.authorizations (payment_reference);

-- Every call, including replays and simulated failures, for tests to inspect.
CREATE TABLE acquirer.calls (
    id              bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    idempotency_key text        NOT NULL,
    outcome         text        NOT NULL CHECK (outcome IN ('approved', 'declined', 'replayed', 'unavailable', 'timeout')),
    called_at       timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP SCHEMA acquirer CASCADE;
