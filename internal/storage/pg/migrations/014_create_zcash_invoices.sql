-- +goose Up
CREATE TABLE zcash_invoices (
    invoice_id TEXT PRIMARY KEY,
    address TEXT NOT NULL UNIQUE,
    expected_zat BIGINT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    paid_txid TEXT NULL,
    paid_height INTEGER NULL,
    paid_zat BIGINT NULL,
    paid_at TIMESTAMPTZ NULL
);

CREATE INDEX idx_zcash_invoices_unpaid ON zcash_invoices(paid_txid) WHERE paid_txid IS NULL;
CREATE INDEX idx_zcash_invoices_created ON zcash_invoices(created_at);

-- +goose Down
DROP TABLE IF EXISTS zcash_invoices;
