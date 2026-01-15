-- +goose Up
-- +goose StatementBegin
CREATE TABLE apple_transactions (
    original_transaction_id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL,
    product_id TEXT NOT NULL,
    tier TEXT NOT NULL,
    redeemed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_apple_transactions_user_id ON apple_transactions(user_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS apple_transactions;
-- +goose StatementEnd
