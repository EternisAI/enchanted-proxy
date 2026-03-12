-- +goose Up

-- Revert NUMERIC back to DOUBLE PRECISION. The FAI payment flow uses 5% slippage
-- tolerance, so float64 precision (~15 significant digits) is more than sufficient,
-- and NUMERIC maps to string in Go which adds unnecessary conversion overhead.
ALTER TABLE fai_payment_intents ALTER COLUMN token_amount TYPE DOUBLE PRECISION;
ALTER TABLE fai_payment_intents ALTER COLUMN price_usd TYPE DOUBLE PRECISION;
ALTER TABLE fai_payment_intents ALTER COLUMN fai_price TYPE DOUBLE PRECISION;

-- +goose Down
ALTER TABLE fai_payment_intents ALTER COLUMN token_amount TYPE NUMERIC(30,18);
ALTER TABLE fai_payment_intents ALTER COLUMN price_usd TYPE NUMERIC(18,6);
ALTER TABLE fai_payment_intents ALTER COLUMN fai_price TYPE NUMERIC(30,18);
