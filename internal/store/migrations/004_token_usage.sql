-- +goose Up
-- What the provider reported for a session. All five columns are NULL when the
-- provider reported no usage: zero is a measurement, NULL is its absence.
ALTER TABLE review_sessions
    ADD COLUMN input_tokens BIGINT,
    ADD COLUMN output_tokens BIGINT,
    ADD COLUMN cache_read_tokens BIGINT,
    ADD COLUMN cache_creation_tokens BIGINT,
    ADD COLUMN cost_usd DOUBLE PRECISION;

-- +goose Down
ALTER TABLE review_sessions
    DROP COLUMN IF EXISTS cost_usd,
    DROP COLUMN IF EXISTS cache_creation_tokens,
    DROP COLUMN IF EXISTS cache_read_tokens,
    DROP COLUMN IF EXISTS output_tokens,
    DROP COLUMN IF EXISTS input_tokens;
