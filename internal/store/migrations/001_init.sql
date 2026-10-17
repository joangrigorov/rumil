-- +goose Up
-- The whole schema lives in this one migration until 0.1.0. Rumil is still
-- being built and holds no data worth keeping, so a schema change edits this
-- file rather than adding a migration that unpicks an earlier one. A second
-- migration appears only when there is data to keep, and never a destructive
-- one (§4).
--
-- The schema is pinned because an extension is database-wide: a bare
-- CREATE EXTENSION installs into the first schema on the caller's search_path,
-- which is a different schema for every test package.
CREATE EXTENSION IF NOT EXISTS vector WITH SCHEMA public;
CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;

CREATE TYPE memory_scope AS ENUM ('REPO', 'ORG');
CREATE TYPE memory_status AS ENUM ('PENDING', 'APPROVED', 'REJECTED');
CREATE TYPE job_status AS ENUM ('QUEUED', 'RUNNING', 'COMPLETED', 'FAILED');

-- A rule's authority over the repository's own guidelines. CONTEXT is what a
-- rule is recorded as: context that the guidelines outrank. DIRECTIVE is set
-- only at the approval gate, by a maintainer, and puts one rule above RUMIL.md
-- for the case it names.
CREATE TYPE memory_authority AS ENUM ('CONTEXT', 'DIRECTIVE');
CREATE TYPE review_status AS ENUM ('RUNNING', 'SUCCEEDED', 'FAILED');

CREATE TABLE webhook_jobs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    delivery_id     TEXT UNIQUE NOT NULL,         -- X-GitHub-Delivery header for idempotency
    installation_id BIGINT NOT NULL,
    event_type      TEXT NOT NULL,                -- 'pull_request', 'issue_comment', 'pull_request_review_comment'
    org_name        TEXT NOT NULL,
    repo_name       TEXT NOT NULL,
    pr_number       INT NOT NULL,
    payload         JSONB NOT NULL,
    status          job_status NOT NULL DEFAULT 'QUEUED',
    attempts        INT NOT NULL DEFAULT 0,
    last_error      TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_jobs_poll ON webhook_jobs (status, created_at) WHERE status = 'QUEUED';

CREATE TABLE memories (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    installation_id BIGINT NOT NULL,              -- Multi-tenant SaaS partition key
    org_name        TEXT NOT NULL,
    repo_name       TEXT,                         -- Nullable when scope = 'ORG' (though origin repo is still recorded)
    scope           memory_scope NOT NULL DEFAULT 'REPO',
    status          memory_status NOT NULL DEFAULT 'PENDING',
    authority       memory_authority NOT NULL DEFAULT 'CONTEXT',
    path_glob       TEXT,                         -- Optional file glob e.g. 'internal/payments/**'
    rule_text       TEXT NOT NULL,                -- Distilled standalone engineering rule
    rationale       TEXT,                         -- Why this rule exists
    source_pr       INT NOT NULL,
    source_comment  TEXT NOT NULL,                -- Permalink or comment ID
    requested_by    TEXT NOT NULL,                -- GitHub login of requester
    reviewed_by     TEXT,                         -- Moderator identifier
    -- Declared without a width on purpose: the width is that of the embedding
    -- provider in the configuration, which SQL cannot read. internal/store
    -- sizes the column and builds the index that searches it at startup, from
    -- the width the configuration resolves to.
    embedding       vector NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_memories_tenant_lookup ON memories (installation_id, org_name, status, scope);

-- A promotion reaches one repository, so the pair is refused at rest as well as
-- by the statement that writes it: a reader can rely on a DIRECTIVE rule being a
-- repository's own, whatever wrote the row.
ALTER TABLE memories
    ADD CONSTRAINT memories_directive_is_a_repository_rule
        CHECK (authority <> 'DIRECTIVE' OR scope = 'REPO');

-- A review session is one run of the reviewer, recorded for the observation
-- deck. The job is optional: a session can outlive the job row it came from,
-- and a one-off `rumil review` run has no job at all.
CREATE TABLE review_sessions (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id          UUID REFERENCES webhook_jobs(id) ON DELETE SET NULL,
    installation_id BIGINT NOT NULL,
    org_name        TEXT NOT NULL,
    repo_name       TEXT NOT NULL,
    pr_number       INT NOT NULL,
    pr_title        TEXT,
    head_sha        TEXT,
    base_sha        TEXT,
    model           TEXT,
    status          review_status NOT NULL DEFAULT 'RUNNING',
    summary         TEXT,
    review_event    TEXT,
    tool_calls      INT NOT NULL DEFAULT 0,
    error           TEXT,
    started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at     TIMESTAMPTZ
);

CREATE INDEX idx_sessions_recent ON review_sessions (started_at DESC);
CREATE INDEX idx_sessions_repo ON review_sessions (org_name, repo_name, started_at DESC);

-- One row per event the session emitted, in the order it emitted them. The seq
-- is the session's own counter: it is what SSE resumes from and what makes a
-- redelivered event idempotent.
CREATE TABLE review_events (
    id          BIGSERIAL PRIMARY KEY,
    session_id  UUID NOT NULL REFERENCES review_sessions(id) ON DELETE CASCADE,
    seq         INT NOT NULL,
    at          TIMESTAMPTZ NOT NULL,
    type        TEXT NOT NULL,
    payload     JSONB NOT NULL,
    UNIQUE (session_id, seq)
);

CREATE INDEX idx_events_session ON review_events (session_id, seq);

-- +goose Down
DROP TABLE IF EXISTS review_events;
DROP TABLE IF EXISTS review_sessions;
DROP TYPE IF EXISTS review_status;
DROP TABLE IF EXISTS memories;
DROP TABLE IF EXISTS webhook_jobs;
DROP TYPE IF EXISTS job_status;
DROP TYPE IF EXISTS memory_authority;
DROP TYPE IF EXISTS memory_status;
DROP TYPE IF EXISTS memory_scope;
