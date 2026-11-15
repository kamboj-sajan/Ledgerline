-- +goose Up

-- Where a merchant wants events delivered. The signing secret is stored in
-- plaintext because HMAC needs the secret itself (a hash would not do).
-- Production would encrypt it with a key from a KMS; see README.
--
-- During rotation the previous secret stays valid until
-- previous_secret_expires_at, and deliveries are signed with both.
CREATE TABLE webhook_endpoints (
    id                         text        PRIMARY KEY,
    merchant_id                text        NOT NULL REFERENCES merchants (id),
    url                        text        NOT NULL CHECK (url ~ '^https?://' AND length(url) <= 2048),
    secret                     text        NOT NULL,
    previous_secret            text,
    previous_secret_expires_at timestamptz,
    enabled                    boolean     NOT NULL DEFAULT true,
    created_at                 timestamptz NOT NULL DEFAULT now(),
    CHECK ((previous_secret IS NULL) = (previous_secret_expires_at IS NULL))
);

CREATE INDEX webhook_endpoints_merchant_idx ON webhook_endpoints (merchant_id) WHERE enabled;

-- One row per (event, endpoint), created in the same transaction as the
-- event. Delivery is at-least-once: a row is 'delivered' only after the
-- endpoint answered 2xx, so a crash in between means it is sent again.
CREATE TABLE webhook_deliveries (
    id              bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    event_id        text        NOT NULL REFERENCES events (id),
    endpoint_id     text        NOT NULL REFERENCES webhook_endpoints (id),
    status          text        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'delivered', 'dead')),
    -- Attempts are counted when a dispatcher claims the row, so an attempt
    -- that crashed mid-send still counts toward the limit.
    attempts        integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    last_status     integer,
    last_error      text,
    delivered_at    timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT webhook_deliveries_once_per_endpoint UNIQUE (event_id, endpoint_id),
    CHECK ((status = 'delivered') = (delivered_at IS NOT NULL))
);

-- The dispatcher's claim query: due pending rows, oldest first.
CREATE INDEX webhook_deliveries_due_idx ON webhook_deliveries (next_attempt_at) WHERE status = 'pending';

-- +goose Down
DROP TABLE webhook_deliveries;
DROP TABLE webhook_endpoints;
