-- +goose Up

-- Change float columns to NUMERIC for exact precision
ALTER TABLE fai_payment_intents ALTER COLUMN token_amount TYPE NUMERIC(30,18);
ALTER TABLE fai_payment_intents ALTER COLUMN price_usd TYPE NUMERIC(18,6);
ALTER TABLE fai_payment_intents ALTER COLUMN fai_price TYPE NUMERIC(30,18);

-- Add CHECK constraint on status to enforce valid lifecycle states
ALTER TABLE fai_payment_intents ADD CONSTRAINT fai_payment_intents_status_check
    CHECK (status IN ('pending', 'completed', 'expired'));

-- +goose Down
ALTER TABLE fai_payment_intents DROP CONSTRAINT IF EXISTS fai_payment_intents_status_check;
ALTER TABLE fai_payment_intents ALTER COLUMN token_amount TYPE DOUBLE PRECISION;
ALTER TABLE fai_payment_intents ALTER COLUMN price_usd TYPE DOUBLE PRECISION;
ALTER TABLE fai_payment_intents ALTER COLUMN fai_price TYPE DOUBLE PRECISION;
