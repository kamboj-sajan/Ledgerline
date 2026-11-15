-- +goose Up

-- Double-entry ledger. Amounts are signed BIGINT minor units: a debit is
-- positive and a credit is negative, so "balanced" means SUM(amount) = 0 per
-- currency. See docs/adr/0001-signed-amounts.md.
--
-- The database enforces the ledger's invariants on its own, independently of
-- the Go code:
--   * every journal transaction has at least 2 entries and sums to zero per
--     currency, checked at COMMIT by deferred constraint triggers;
--   * an entry's currency equals its account's currency (composite FK);
--   * accounts, journal transactions and entries are append-only.

CREATE TABLE accounts (
    id          text        PRIMARY KEY,
    merchant_id text        REFERENCES merchants (id),
    code        text        NOT NULL,
    type        text        NOT NULL CHECK (type IN ('asset', 'liability', 'revenue', 'expense')),
    currency    text        NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    created_at  timestamptz NOT NULL DEFAULT now(),

    -- One account per (merchant, code, currency). Platform accounts have a NULL
    -- merchant_id, and NULLS NOT DISTINCT makes those unique too (by default
    -- every NULL counts as distinct, which would allow duplicates).
    CONSTRAINT accounts_key UNIQUE NULLS NOT DISTINCT (merchant_id, code, currency),

    -- Target of the composite foreign key from entries (see below).
    CONSTRAINT accounts_id_currency UNIQUE (id, currency),

    -- The chart of accounts. The code fixes the account type and whether the
    -- account belongs to a merchant, so a merchant_payable can never be
    -- created as an asset or without a merchant.
    CONSTRAINT accounts_chart CHECK (
        (code = 'merchant_payable'     AND type = 'liability' AND merchant_id IS NOT NULL) OR
        (code = 'processor_receivable' AND type = 'asset'     AND merchant_id IS NULL) OR
        (code = 'bank_cash'            AND type = 'asset'     AND merchant_id IS NULL) OR
        (code = 'fee_revenue'          AND type = 'revenue'   AND merchant_id IS NULL) OR
        (code = 'network_fee_expense'  AND type = 'expense'   AND merchant_id IS NULL)
    )
);

CREATE TABLE journal_transactions (
    id           text        PRIMARY KEY,
    kind         text        NOT NULL CHECK (kind IN ('capture', 'refund', 'settlement', 'reversal')),
    -- The payment this transaction belongs to. The foreign key is added in the
    -- same migration that creates the payments table.
    payment_id   text,
    -- An identifier from outside the ledger, e.g. a bank settlement reference.
    -- UNIQUE makes posting the same external event twice impossible, so
    -- re-running reconciliation can never double-post. NULLs do not conflict.
    external_ref text        CONSTRAINT journal_transactions_external_ref_key UNIQUE,
    -- Set only on reversals; UNIQUE means a transaction can be reversed once.
    reverses_id  text        CONSTRAINT journal_transactions_reverses_id_key UNIQUE
                             REFERENCES journal_transactions (id),
    created_at   timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT journal_transactions_reversal_link CHECK ((kind = 'reversal') = (reverses_id IS NOT NULL))
);

CREATE INDEX journal_transactions_payment_id_idx ON journal_transactions (payment_id);

CREATE TABLE entries (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    transaction_id text   NOT NULL REFERENCES journal_transactions (id),
    account_id     text   NOT NULL,
    amount         bigint NOT NULL CONSTRAINT entries_amount_nonzero CHECK (amount <> 0),
    currency       text   NOT NULL,

    -- An entry's currency must equal its account's currency. A composite
    -- foreign key enforces this declaratively, with no trigger.
    CONSTRAINT entries_account_currency FOREIGN KEY (account_id, currency)
        REFERENCES accounts (id, currency)
);

CREATE INDEX entries_transaction_id_idx ON entries (transaction_id);
-- Balances are SUM(amount) per account; INCLUDE lets that be an index-only scan.
CREATE INDEX entries_account_id_idx ON entries (account_id) INCLUDE (amount);

-- Balance check ------------------------------------------------------------

-- +goose StatementBegin
CREATE FUNCTION ledger_assert_balanced(txn_id text) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
    n_entries  bigint;
    bad_ccy    text;
    bad_total  numeric;
BEGIN
    SELECT count(*) INTO n_entries FROM entries WHERE transaction_id = txn_id;
    IF n_entries < 2 THEN
        RAISE EXCEPTION 'journal transaction % has % entries; at least 2 are required', txn_id, n_entries
            USING ERRCODE = 'check_violation', CONSTRAINT = 'journal_transaction_min_entries';
    END IF;

    -- sum() over bigint returns numeric, so this cannot overflow.
    SELECT currency, sum(amount) INTO bad_ccy, bad_total
    FROM entries
    WHERE transaction_id = txn_id
    GROUP BY currency
    HAVING sum(amount) <> 0
    LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'journal transaction % does not balance: % entries sum to %', txn_id, bad_ccy, bad_total
            USING ERRCODE = 'check_violation', CONSTRAINT = 'journal_transaction_balanced';
    END IF;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION ledger_check_transaction_row() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM ledger_assert_balanced(NEW.id);
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION ledger_check_entry_row() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM ledger_assert_balanced(NEW.transaction_id);
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- DEFERRABLE INITIALLY DEFERRED: the checks run at COMMIT, after all entries of
-- a transaction have been inserted, not after each INSERT statement.
-- Two triggers are needed. The one on journal_transactions catches a
-- transaction with no entries at all, which no entry trigger would ever see.
-- The one on entries catches entries added to a transaction that was
-- committed earlier.
CREATE CONSTRAINT TRIGGER journal_transactions_balanced
    AFTER INSERT ON journal_transactions
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_check_transaction_row();

CREATE CONSTRAINT TRIGGER entries_balanced
    AFTER INSERT ON entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_check_entry_row();

-- Append-only ----------------------------------------------------------------

-- +goose StatementBegin
CREATE FUNCTION ledger_reject_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% on % is not allowed: the ledger is append-only; post a reversing transaction instead', TG_OP, TG_TABLE_NAME
        USING ERRCODE = 'restrict_violation', CONSTRAINT = TG_TABLE_NAME || '_append_only';
END;
$$;
-- +goose StatementEnd

-- Row triggers do not fire on TRUNCATE, so it gets its own statement trigger.
CREATE TRIGGER accounts_append_only BEFORE UPDATE OR DELETE ON accounts
    FOR EACH ROW EXECUTE FUNCTION ledger_reject_mutation();
CREATE TRIGGER accounts_no_truncate BEFORE TRUNCATE ON accounts
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_reject_mutation();
CREATE TRIGGER journal_transactions_append_only BEFORE UPDATE OR DELETE ON journal_transactions
    FOR EACH ROW EXECUTE FUNCTION ledger_reject_mutation();
CREATE TRIGGER journal_transactions_no_truncate BEFORE TRUNCATE ON journal_transactions
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_reject_mutation();
CREATE TRIGGER entries_append_only BEFORE UPDATE OR DELETE ON entries
    FOR EACH ROW EXECUTE FUNCTION ledger_reject_mutation();
CREATE TRIGGER entries_no_truncate BEFORE TRUNCATE ON entries
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_reject_mutation();

-- +goose Down
DROP TABLE entries;
DROP TABLE journal_transactions;
DROP TABLE accounts;
DROP FUNCTION ledger_reject_mutation();
DROP FUNCTION ledger_check_entry_row();
DROP FUNCTION ledger_check_transaction_row();
DROP FUNCTION ledger_assert_balanced(text);
