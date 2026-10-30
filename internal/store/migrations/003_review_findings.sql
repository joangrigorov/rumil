-- +goose Up
-- One row per inline comment Rumil posted. The row keeps the GitHub comment id
-- so later feedback can be attached to the finding it answers, and the flags
-- are timestamps rather than a status because a finding can be replied to,
-- reacted to and then dismissed.
CREATE TABLE review_findings (
    id BIGSERIAL PRIMARY KEY,
    session_id UUID REFERENCES review_sessions(id) ON DELETE SET NULL,
    installation_id BIGINT NOT NULL,
    org_name TEXT NOT NULL,
    repo_name TEXT NOT NULL,
    pr_number INTEGER NOT NULL,
    review_id BIGINT NOT NULL DEFAULT 0,
    comment_id BIGINT NOT NULL UNIQUE,
    path TEXT NOT NULL,
    line INTEGER NOT NULL,
    body TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    replied_at TIMESTAMPTZ,
    resolved_at TIMESTAMPTZ,
    reaction TEXT,
    dismissed_at TIMESTAMPTZ,
    head_moved_at TIMESTAMPTZ
);

CREATE INDEX idx_findings_repo ON review_findings (org_name, repo_name, pr_number);
CREATE INDEX idx_findings_review ON review_findings (review_id);
CREATE INDEX idx_findings_created ON review_findings (created_at);

-- +goose Down
DROP TABLE IF EXISTS review_findings;
