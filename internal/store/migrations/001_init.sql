-- +goose Up
CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TYPE memory_scope AS ENUM ('REPO', 'ORG');
CREATE TYPE memory_status AS ENUM ('PENDING', 'APPROVED', 'REJECTED');
CREATE TYPE job_status AS ENUM ('QUEUED', 'RUNNING', 'COMPLETED', 'FAILED');

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
    path_glob       TEXT,                         -- Optional file glob e.g. 'internal/payments/**'
    rule_text       TEXT NOT NULL,                -- Distilled standalone engineering rule
    rationale       TEXT,                         -- Why this rule exists
    source_pr       INT NOT NULL,
    source_comment  TEXT NOT NULL,                -- Permalink or comment ID
    requested_by    TEXT NOT NULL,                -- GitHub login of requester
    reviewed_by     TEXT,                         -- Moderator identifier
    embedding       vector(1536) NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_memories_tenant_lookup ON memories (installation_id, org_name, status, scope);
CREATE INDEX idx_memories_embedding ON memories USING hnsw (embedding vector_cosine_ops);

-- +goose Down
DROP TABLE IF EXISTS memories;
DROP TABLE IF EXISTS webhook_jobs;
DROP TYPE IF EXISTS job_status;
DROP TYPE IF EXISTS memory_status;
DROP TYPE IF EXISTS memory_scope;
