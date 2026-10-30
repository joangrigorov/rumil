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
	// StatusCancelled is a delivery that was stopped before it finished, either
	// while queued or by a running worker reading cancel_requested.
	StatusCancelled = "CANCELLED"
)

// ErrDuplicateDelivery reports a delivery id that is already enqueued, which
// is how GitHub's retries are absorbed.
var ErrDuplicateDelivery = errors.New("webhook delivery already enqueued")

// Errors a cancel request can meet.
var (
	// ErrJobNotFound reports a job id that is not in the table.
	ErrJobNotFound = errors.New("webhook job not found")
	// ErrJobFinished reports a job that already reached a terminal status.
	ErrJobFinished = errors.New("the webhook job already finished")
)

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
	// WorkerID is who claimed a RUNNING job, empty before that.
	WorkerID string
	// CancelRequested is the flag a running worker polls to stop.
	CancelRequested bool
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

const jobColumns = `id, delivery_id, installation_id, event_type, org_name, repo_name,
	pr_number, payload, status, attempts, coalesce(last_error, ''), coalesce(worker_id, ''),
	cancel_requested, created_at, updated_at`

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
func (d *DB) ClaimJob(ctx context.Context, workerID string) (*Job, error) {
	rows, err := d.pool.Query(ctx, `
		UPDATE webhook_jobs SET status = 'RUNNING', attempts = attempts + 1,
		    worker_id = nullif($1, ''), updated_at = now()
		WHERE id = (
			SELECT id FROM webhook_jobs
			WHERE status = 'QUEUED'
			ORDER BY created_at
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING `+jobColumns, workerID)
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

// QueueEntry is one queued or running job as the deck's queue view shows it.
type QueueEntry struct {
	ID              string
	DeliveryID      string
	EventType       string
	OrgName         string
	RepoName        string
	PRNumber        int
	Status          string
	WorkerID        string
	CancelRequested bool
	CreatedAt       time.Time
	UpdatedAt       time.Time
	// SessionID is the latest session the job started, empty before one.
	SessionID string
}

// QueueListMax bounds the queue view. A backlog larger than this is a problem
// the operator reads from the counters, not from one page.
const QueueListMax = 200

// ListQueue returns the jobs waiting or running, oldest first, with the latest
// session each started.
func (d *DB) ListQueue(ctx context.Context, limit int) ([]QueueEntry, error) {
	if limit <= 0 || limit > QueueListMax {
		limit = QueueListMax
	}

	rows, err := d.pool.Query(ctx, `
		SELECT j.id, j.delivery_id, j.event_type, j.org_name, j.repo_name, j.pr_number,
		       j.status, coalesce(j.worker_id, ''), j.cancel_requested, j.created_at, j.updated_at,
		       coalesce(s.id::text, '')
		FROM webhook_jobs j
		LEFT JOIN LATERAL (
			SELECT id FROM review_sessions
			WHERE job_id = j.id
			ORDER BY started_at DESC
			LIMIT 1
		) s ON true
		WHERE j.status IN ('QUEUED', 'RUNNING')
		ORDER BY j.created_at
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("listing the queue: %w", err)
	}
	defer rows.Close()

	entries := make([]QueueEntry, 0, limit)
	for rows.Next() {
		var entry QueueEntry
		if err := rows.Scan(
			&entry.ID, &entry.DeliveryID, &entry.EventType, &entry.OrgName, &entry.RepoName,
			&entry.PRNumber, &entry.Status, &entry.WorkerID, &entry.CancelRequested,
			&entry.CreatedAt, &entry.UpdatedAt, &entry.SessionID,
		); err != nil {
			return nil, fmt.Errorf("scanning a queue row: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing the queue: %w", err)
	}
	return entries, nil
}

// JobByID returns one job, or ErrJobNotFound.
func (d *DB) JobByID(ctx context.Context, id string) (*Job, error) {
	rows, err := d.pool.Query(ctx, `SELECT `+jobColumns+` FROM webhook_jobs WHERE id = $1`, id)
	if err != nil {
		return nil, fmt.Errorf("reading job %s: %w", id, err)
	}
	defer rows.Close()

	job, err := scanJob(rows)
	if err != nil {
		return nil, err
	}
	if job == nil {
		return nil, ErrJobNotFound
	}
	return job, nil
}

// RequestJobCancel asks a job to stop. A QUEUED job leaves the queue at once,
// because no worker is there to read the flag; a RUNNING job gets the flag and
// its worker stops it. A finished job is returned as ErrJobFinished: it is too
// late, and a review it submitted stands.
func (d *DB) RequestJobCancel(ctx context.Context, id string) (*Job, error) {
	rows, err := d.pool.Query(ctx, `
		UPDATE webhook_jobs
		SET cancel_requested = true,
		    status = CASE WHEN status = 'QUEUED' THEN 'CANCELLED'::job_status ELSE status END,
		    updated_at = now()
		WHERE id = $1 AND status IN ('QUEUED', 'RUNNING')
		RETURNING `+jobColumns, id)
	if err != nil {
		return nil, fmt.Errorf("cancelling job %s: %w", id, err)
	}
	defer rows.Close()

	job, err := scanJob(rows)
	if job != nil {
		return job, nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	var status string
	if readErr := d.pool.QueryRow(ctx, `SELECT status FROM webhook_jobs WHERE id = $1`, id).Scan(&status); readErr != nil {
		if errors.Is(readErr, pgx.ErrNoRows) {
			return nil, ErrJobNotFound
		}
		return nil, fmt.Errorf("reading job %s: %w", id, readErr)
	}
	return nil, fmt.Errorf("%w: %s", ErrJobFinished, status)
}

// JobCancelRequested reports whether a worker should stop the job. A job that
// no longer exists has nothing to stop.
func (d *DB) JobCancelRequested(ctx context.Context, id string) (bool, error) {
	var requested bool
	if err := d.pool.QueryRow(ctx, `SELECT cancel_requested FROM webhook_jobs WHERE id = $1`, id).Scan(&requested); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("reading the cancel flag of job %s: %w", id, err)
	}
	return requested, nil
}

// CancelJob records a running job the worker stopped. A job that already
// finished is left as it is.
func (d *DB) CancelJob(ctx context.Context, id, reason string) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE webhook_jobs SET status = $1, last_error = nullif($2, ''), updated_at = now()
		WHERE id = $3 AND status = 'RUNNING'`, StatusCancelled, reason, id)
	if err != nil {
		return fmt.Errorf("marking job %s cancelled: %w", id, err)
	}
	return nil
}

func scanJob(rows pgx.Rows) (*Job, error) {
	if !rows.Next() {
		return nil, rows.Err()
	}

	var job Job
	if err := rows.Scan(
		&job.ID, &job.DeliveryID, &job.InstallationID, &job.EventType, &job.OrgName, &job.RepoName,
		&job.PRNumber, &job.Payload, &job.Status, &job.Attempts, &job.LastError, &job.WorkerID,
		&job.CancelRequested, &job.CreatedAt, &job.UpdatedAt,
	); err != nil {
		return nil, fmt.Errorf("scanning a job row: %w", err)
	}
	return &job, nil
}
