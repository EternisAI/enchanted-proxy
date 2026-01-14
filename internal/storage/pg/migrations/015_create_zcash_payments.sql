-- +goose Up
-- +goose StatementBegin
CREATE TABLE zcash_payments (
    invoice_id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL,
    product_id TEXT NOT NULL,
    amount_zat BIGINT NOT NULL,
    redeemed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_zcash_payments_user_id ON zcash_payments(user_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS zcash_payments;
-- +goose StatementEnd
