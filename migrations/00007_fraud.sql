-- +goose Up

-- The fraud engine's verdict, recorded at authorization. NULL until then.
ALTER TABLE payments
    ADD COLUMN fraud_decision text CHECK (fraud_decision IN ('allow', 'review', 'block')),
    ADD COLUMN fraud_rules    text[] NOT NULL DEFAULT '{}',
    -- A payment failed for fraud must carry the block decision that failed it.
    ADD CONSTRAINT payments_fraud_failure CHECK (failure_code IS DISTINCT FROM 'fraud_suspected' OR fraud_decision = 'block');

-- +goose Down
ALTER TABLE payments DROP CONSTRAINT payments_fraud_failure, DROP COLUMN fraud_rules, DROP COLUMN fraud_decision;
