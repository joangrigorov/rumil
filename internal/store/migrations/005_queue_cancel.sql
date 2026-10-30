-- +goose Up
-- +goose NO TRANSACTION
-- A cancelled review is a terminal state of its own: it did not fail, and it
-- did not finish. A cancelled job is how a queued delivery leaves the queue
-- without a worker running it.
ALTER TYPE review_status ADD VALUE IF NOT EXISTS 'CANCELLED';
ALTER TYPE job_status ADD VALUE IF NOT EXISTS 'CANCELLED';

-- cancel_requested is the signal a running worker polls. worker_id is who
-- claimed the job, so the queue view can name it.
ALTER TABLE webhook_jobs
    ADD COLUMN cancel_requested BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN worker_id TEXT;

-- +goose Down
ALTER TABLE webhook_jobs
    DROP COLUMN IF EXISTS worker_id,
    DROP COLUMN IF EXISTS cancel_requested;
