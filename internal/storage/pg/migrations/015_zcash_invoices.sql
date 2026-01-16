-- +goose Up
CREATE TABLE zcash_invoices (
    id UUID PRIMARY KEY,
    user_id TEXT NOT NULL,
    product_id TEXT NOT NULL,
    amount_zatoshis BIGINT NOT NULL,
    zec_amount DOUBLE PRECISION NOT NULL,
    price_usd DOUBLE PRECISION NOT NULL,
    receiving_address TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',  -- pending/processing/paid/expired
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    paid_at TIMESTAMPTZ
);

CREATE INDEX idx_zcash_invoices_user_id ON zcash_invoices (user_id);
CREATE INDEX idx_zcash_invoices_user_status ON zcash_invoices (user_id, status);
CREATE INDEX idx_zcash_invoices_status_created ON zcash_invoices (status, created_at);

-- +goose Down
DROP TABLE zcash_invoices;
