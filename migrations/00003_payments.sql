-- +goose Up

-- Pricing: fee = round_half_up(amount × fee_bps / 10000) + fee_fixed, capped at
-- the captured amount. fee_fixed is in the payment currency's minor units, so
-- 30 means $0.30, ¥30 or 0.030 BHD. That is a simplification; real pricing
-- would set the fixed fee per currency.
ALTER TABLE merchants
    ADD COLUMN fee_bps   integer NOT NULL DEFAULT 290 CHECK (fee_bps BETWEEN 0 AND 10000),
    ADD COLUMN fee_fixed bigint  NOT NULL DEFAULT 30  CHECK (fee_fixed >= 0);

CREATE TABLE payments (
    id                 text        PRIMARY KEY,
    merchant_id        text        NOT NULL REFERENCES merchants (id),
    status             text        NOT NULL CHECK (status IN ('created', 'authorized', 'captured', 'refunded', 'failed')),
    currency           text        NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    -- amount is what was requested. Each later amount is bounded by the one
    -- before it (see payments_amounts). 999,999,999,999 minor units is the cap.
    amount             bigint      NOT NULL CHECK (amount BETWEEN 1 AND 999999999999),
    authorized_amount  bigint      NOT NULL DEFAULT 0,
    captured_amount    bigint      NOT NULL DEFAULT 0,
    refunded_amount    bigint      NOT NULL DEFAULT 0,
    fee_amount         bigint      NOT NULL DEFAULT 0,
    customer_id        text        NOT NULL CHECK (length(customer_id) BETWEEN 1 AND 255),
    card_fingerprint   text        NOT NULL CHECK (length(card_fingerprint) BETWEEN 1 AND 255),
    card_country       text        NOT NULL CHECK (card_country ~ '^[A-Z]{2}$'),
    ip_country         text        NOT NULL CHECK (ip_country ~ '^[A-Z]{2}$'),
    metadata           jsonb       NOT NULL DEFAULT '{}',
    failure_code       text,
    failure_message    text,
    acquirer_reference text,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT payments_amounts CHECK (
        0 <= refunded_amount
        AND refunded_amount <= captured_amount
        AND captured_amount <= authorized_amount
        AND authorized_amount <= amount
        AND 0 <= fee_amount AND fee_amount <= captured_amount
    ),

    -- Each status implies facts about the amounts, so a bug that changes one
    -- without the other cannot commit.
    CONSTRAINT payments_status_amounts CHECK (
        (status = 'created'    AND authorized_amount = 0 AND captured_amount = 0) OR
        (status = 'authorized' AND authorized_amount > 0 AND captured_amount = 0) OR
        (status = 'captured'   AND captured_amount > 0   AND refunded_amount < captured_amount) OR
        (status = 'refunded'   AND captured_amount > 0   AND refunded_amount = captured_amount) OR
        (status = 'failed'     AND captured_amount = 0   AND failure_code IS NOT NULL)
    )
);

CREATE INDEX payments_merchant_id_idx ON payments (merchant_id, created_at);
-- For fraud velocity rules: recent payments by card.
CREATE INDEX payments_card_fingerprint_idx ON payments (card_fingerprint, created_at);
-- For the authorization-expiry sweep.
CREATE INDEX payments_authorized_idx ON payments (updated_at) WHERE status = 'authorized';

CREATE TABLE refunds (
    id         text        PRIMARY KEY,
    payment_id text        NOT NULL REFERENCES payments (id),
    amount     bigint      NOT NULL CHECK (amount > 0),
    currency   text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX refunds_payment_id_idx ON refunds (payment_id);

CREATE TRIGGER refunds_append_only BEFORE UPDATE OR DELETE ON refunds
    FOR EACH ROW EXECUTE FUNCTION ledger_reject_mutation();
CREATE TRIGGER refunds_no_truncate BEFORE TRUNCATE ON refunds
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_reject_mutation();

-- +goose Down
DROP TABLE refunds;
DROP TABLE payments;
ALTER TABLE merchants DROP COLUMN fee_fixed, DROP COLUMN fee_bps;
