-- +goose Up
-- A review session can now be a subagent of another session. The kind keeps the
-- two apart everywhere the deck and the continuation logic read a parent: a
-- subagent's parent is the review that spawned it, not an earlier attempt at
-- the same review. The label, task, paths and ordinal are the child's own
-- record of what it was asked to review.
ALTER TABLE review_sessions
    ADD COLUMN kind TEXT NOT NULL DEFAULT 'REVIEW',
    ADD COLUMN label TEXT NOT NULL DEFAULT '',
    ADD COLUMN task TEXT NOT NULL DEFAULT '',
    ADD COLUMN paths JSONB NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN ordinal INT NOT NULL DEFAULT 0;

ALTER TABLE review_sessions
    ADD CONSTRAINT review_sessions_kind_check CHECK (kind IN ('REVIEW', 'SUBAGENT'));

CREATE INDEX idx_sessions_parent_kind
    ON review_sessions (parent_session_id, kind, started_at);

-- +goose Down
DROP INDEX IF EXISTS idx_sessions_parent_kind;
ALTER TABLE review_sessions
    DROP CONSTRAINT IF EXISTS review_sessions_kind_check,
    DROP COLUMN IF EXISTS ordinal,
    DROP COLUMN IF EXISTS paths,
    DROP COLUMN IF EXISTS task,
    DROP COLUMN IF EXISTS label,
    DROP COLUMN IF EXISTS kind;
