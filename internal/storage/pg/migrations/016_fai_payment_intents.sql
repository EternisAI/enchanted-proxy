-- +goose Up
CREATE TABLE fai_payment_intents (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL,
    payment_id TEXT NOT NULL UNIQUE,
    product_id TEXT NOT NULL,
    token_address TEXT,
    token_amount DOUBLE PRECISION,
    price_usd DOUBLE PRECISION NOT NULL,
    fai_price DOUBLE PRECISION,
    status TEXT NOT NULL DEFAULT 'pending',  -- pending/completed/expired
    paid_block BIGINT DEFAULT 0,
    tx_hash TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    paid_at TIMESTAMPTZ
);

CREATE INDEX idx_fai_payment_intents_user_id ON fai_payment_intents (user_id);
CREATE INDEX idx_fai_payment_intents_payment_id ON fai_payment_intents (payment_id);
CREATE INDEX idx_fai_payment_intents_status ON fai_payment_intents (status);
CREATE INDEX idx_fai_payment_intents_status_created ON fai_payment_intents (status, created_at);

-- +goose Down
DROP TABLE fai_payment_intents;
