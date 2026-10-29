-- +goose Up
-- The first schema change after a release could reach a deployed database, so
-- it gets its own migration. A session that continues an earlier one records
-- the attempt it came from, who asked for it, and why the earlier attempt
-- stopped, which is what lets both surfaces offer a continuation without
-- parsing the error text.
ALTER TABLE review_sessions
    ADD COLUMN parent_session_id UUID REFERENCES review_sessions(id) ON DELETE SET NULL,
    ADD COLUMN turn_limit_reached BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN requested_by TEXT,
    ADD COLUMN requested_at TIMESTAMPTZ;

CREATE INDEX idx_sessions_parent ON review_sessions (parent_session_id);

-- +goose Down
DROP INDEX IF EXISTS idx_sessions_parent;
ALTER TABLE review_sessions
    DROP COLUMN IF EXISTS parent_session_id,
    DROP COLUMN IF EXISTS turn_limit_reached,
    DROP COLUMN IF EXISTS requested_by,
    DROP COLUMN IF EXISTS requested_at;
