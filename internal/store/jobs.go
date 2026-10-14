package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// JobStatus values of the job_status enum.
const (
	StatusQueued    = "QUEUED"
	StatusRunning   = "RUNNING"
	StatusCompleted = "COMPLETED"
	StatusFailed    = "FAILED"
)

// ErrDuplicateDelivery reports a delivery id that is already enqueued, which
// is how GitHub's retries are absorbed.
var ErrDuplicateDelivery = errors.New("webhook delivery already enqueued")

// uniqueViolation is the Postgres SQLSTATE for a unique index conflict.
const uniqueViolation = "23505"

// Job is one row of webhook_jobs.
type Job struct {
	ID             string
	DeliveryID     string
	InstallationID int64
	EventType      string
	OrgName        string
	RepoName       string
	PRNumber       int
	Payload        []byte
	Status         string
	Attempts       int
	LastError      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

const jobColumns = `id, delivery_id, installation_id, event_type, org_name, repo_name,
	pr_number, payload, status, attempts, coalesce(last_error, ''), created_at, updated_at`

// Enqueue stores a queued job. A repeated delivery id returns
// ErrDuplicateDelivery so the caller can answer the retry without queueing the
// work twice.
func (d *DB) Enqueue(ctx context.Context, job Job) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO webhook_jobs (delivery_id, installation_id, event_type, org_name, repo_name, pr_number, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		job.DeliveryID, job.InstallationID, job.EventType, job.OrgName, job.RepoName, job.PRNumber, job.Payload)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			return ErrDuplicateDelivery
		}
		return fmt.Errorf("enqueueing delivery %s: %w", job.DeliveryID, err)
	}
	return nil
}

// ClaimJob takes the oldest queued job and marks it RUNNING, incrementing its
// attempt count. It returns nil when nothing is waiting.
//
// The inner SELECT holds the row lock with SKIP LOCKED, so concurrent workers
// never claim the same job.
func (d *DB) ClaimJob(ctx context.Context) (*Job, error) {
	rows, err := d.pool.Query(ctx, `
		UPDATE webhook_jobs SET status = 'RUNNING', attempts = attempts + 1, updated_at = now()
		WHERE id = (
			SELECT id FROM webhook_jobs
			WHERE status = 'QUEUED'
			ORDER BY created_at
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING `+jobColumns)
	if err != nil {
		return nil, fmt.Errorf("claiming a job: %w", err)
	}
	defer rows.Close()

	job, err := scanJob(rows)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return job, nil
}

// CompleteJob marks a job COMPLETED.
func (d *DB) CompleteJob(ctx context.Context, id string) error {
	return d.finishJob(ctx, StatusCompleted, id, "")
}

// FailJob records why a job failed and marks it FAILED.
func (d *DB) FailJob(ctx context.Context, id string, cause error) error {
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	return d.finishJob(ctx, StatusFailed, id, message)
}

func (d *DB) finishJob(ctx context.Context, status, id, lastError string) error {
	tag, err := d.pool.Exec(ctx, `
		UPDATE webhook_jobs SET status = $1, last_error = nullif($2, ''), updated_at = now()
		WHERE id = $3`, status, lastError, id)
	if err != nil {
		return fmt.Errorf("marking job %s %s: %w", id, status, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("job %s does not exist", id)
	}
	return nil
}

// ResetRunningJobs returns jobs left RUNNING by an interrupted process to
// QUEUED, so a SIGKILL mid-review does not strand them. It runs once at
// startup and reports how many rows it moved.
func (d *DB) ResetRunningJobs(ctx context.Context) (int64, error) {
	tag, err := d.pool.Exec(ctx, `
		UPDATE webhook_jobs
		SET status = 'QUEUED', updated_at = now(),
		    last_error = 'reset to QUEUED after an interrupted run'
		WHERE status = 'RUNNING'`)
	if err != nil {
		return 0, fmt.Errorf("resetting running jobs: %w", err)
	}
	return tag.RowsAffected(), nil
}

// JobByDelivery returns the job for a delivery id, or nil when there is none.
func (d *DB) JobByDelivery(ctx context.Context, deliveryID string) (*Job, error) {
	rows, err := d.pool.Query(ctx, `SELECT `+jobColumns+` FROM webhook_jobs WHERE delivery_id = $1`, deliveryID)
	if err != nil {
		return nil, fmt.Errorf("reading delivery %s: %w", deliveryID, err)
	}
	defer rows.Close()

	job, err := scanJob(rows)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return job, nil
}

// CountJobs returns how many jobs hold the given status.
func (d *DB) CountJobs(ctx context.Context, status string) (int, error) {
	var count int
	if err := d.pool.QueryRow(ctx, `SELECT count(*) FROM webhook_jobs WHERE status = $1`, status).Scan(&count); err != nil {
		return 0, fmt.Errorf("counting %s jobs: %w", status, err)
	}
	return count, nil
}

func scanJob(rows pgx.Rows) (*Job, error) {
	if !rows.Next() {
		return nil, rows.Err()
	}

	var job Job
	if err := rows.Scan(
		&job.ID, &job.DeliveryID, &job.InstallationID, &job.EventType, &job.OrgName, &job.RepoName,
		&job.PRNumber, &job.Payload, &job.Status, &job.Attempts, &job.LastError, &job.CreatedAt, &job.UpdatedAt,
	); err != nil {
		return nil, fmt.Errorf("scanning a job row: %w", err)
	}
	return &job, nil
}
