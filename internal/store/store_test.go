package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/joangrigorov/rumil/internal/testutil"
)

// testDB connects to a schema of its own on TEST_DATABASE_URL and applies the
// migrations. Tests skip when the variable is unset, so `go test ./...` still
// passes without a database; CI and docker-compose provide one.
func testDB(t *testing.T) *DB {
	t.Helper()

	dsn := testutil.TestDatabaseURL(t, "rumil_store_test")

	ctx := context.Background()
	db, err := Open(ctx, dsn, true)
	if err != nil {
		t.Fatalf("opening the test database: %v", err)
	}
	t.Cleanup(db.Close)

	// Tests in this package share the schema, so each starts from an empty
	// queue and an empty memory table.
	if _, err := db.pool.Exec(ctx, `TRUNCATE webhook_jobs, memories`); err != nil {
		t.Fatalf("truncating the test tables: %v", err)
	}
	return db
}

func sampleJob(deliveryID string) Job {
	return Job{
		DeliveryID:     deliveryID,
		InstallationID: 98765432,
		EventType:      "pull_request",
		OrgName:        "acme",
		RepoName:       "payments",
		PRNumber:       42,
		Payload:        []byte(`{"action":"review_requested"}`),
	}
}

func TestMigrateCreatesTheSchema(t *testing.T) {
	db := testDB(t)

	// The schema of §4 exists from the first migration, including the tables
	// later phases use, so no phase needs a destructive migration.
	for _, table := range []string{"webhook_jobs", "memories"} {
		var exists bool
		err := db.pool.QueryRow(context.Background(),
			`SELECT exists (SELECT 1 FROM information_schema.tables WHERE table_name = $1)`, table).Scan(&exists)
		if err != nil {
			t.Fatalf("checking for %s: %v", table, err)
		}
		if !exists {
			t.Errorf("table %s does not exist", table)
		}
	}

	var embeddingType string
	err := db.pool.QueryRow(context.Background(), `
		SELECT format_type(atttypid, atttypmod) FROM pg_attribute
		WHERE attrelid = 'memories'::regclass AND attname = 'embedding'`).Scan(&embeddingType)
	if err != nil {
		t.Fatalf("reading the embedding column type: %v", err)
	}
	if embeddingType != "vector(1536)" {
		t.Errorf("memories.embedding is %s, want vector(1536)", embeddingType)
	}

	// The authority a rule carries arrives in a later, additive migration
	// (§4, §10 item 5). What matters is that it is not nullable and that a row
	// which does not name it reads back as recorded context, which is what makes
	// the migration safe on an installation that already holds rules.
	var authorityType, authorityDefault, nullable string
	err = db.pool.QueryRow(context.Background(), `
		SELECT udt_name, coalesce(column_default, ''), is_nullable
		FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'memories' AND column_name = 'authority'`).
		Scan(&authorityType, &authorityDefault, &nullable)
	if err != nil {
		t.Fatalf("reading the authority column: %v", err)
	}
	if authorityType != "memory_authority" {
		t.Errorf("memories.authority is %s, want memory_authority", authorityType)
	}
	if !strings.Contains(authorityDefault, "CONTEXT") {
		t.Errorf("memories.authority defaults to %q, want CONTEXT", authorityDefault)
	}
	if nullable != "NO" {
		t.Error("memories.authority is nullable, want every rule to carry an authority")
	}

	// A promotion reaches one repository, and the table refuses the pair at rest
	// rather than leaving it to the statement that writes it.
	var constraint string
	err = db.pool.QueryRow(context.Background(), `
		SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conrelid = 'memories'::regclass AND conname = 'memories_directive_is_a_repository_rule'`).Scan(&constraint)
	if err != nil {
		t.Fatalf("reading the promotion constraint: %v", err)
	}
	if !strings.Contains(constraint, "DIRECTIVE") || !strings.Contains(constraint, "REPO") {
		t.Errorf("the promotion constraint is %q, want it to refuse DIRECTIVE on anything but REPO", constraint)
	}
}

func TestEnqueueAndClaim(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	if err := db.Enqueue(ctx, sampleJob("delivery-1")); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}

	status, err := db.CountJobs(ctx, StatusQueued)
	if err != nil {
		t.Fatalf("CountJobs returned error: %v", err)
	}
	if status != 1 {
		t.Errorf("queued jobs = %d, want 1", status)
	}

	job, err := db.ClaimJob(ctx)
	if err != nil {
		t.Fatalf("ClaimJob returned error: %v", err)
	}
	if job == nil {
		t.Fatal("ClaimJob returned no job, want the queued one")
	}
	if job.Status != StatusRunning || job.Attempts != 1 {
		t.Errorf("claimed job is %s with %d attempts, want RUNNING with 1", job.Status, job.Attempts)
	}
	if job.PRNumber != 42 || job.OrgName != "acme" || job.RepoName != "payments" {
		t.Errorf("claimed job lost its coordinates: %+v", job)
	}

	again, err := db.ClaimJob(ctx)
	if err != nil {
		t.Fatalf("second ClaimJob returned error: %v", err)
	}
	if again != nil {
		t.Errorf("ClaimJob returned %s twice, want nothing left to claim", again.DeliveryID)
	}

	if err := db.CompleteJob(ctx, job.ID); err != nil {
		t.Fatalf("CompleteJob returned error: %v", err)
	}
	completed, err := db.JobByDelivery(ctx, "delivery-1")
	if err != nil {
		t.Fatalf("JobByDelivery returned error: %v", err)
	}
	if completed.Status != StatusCompleted {
		t.Errorf("status = %s, want %s", completed.Status, StatusCompleted)
	}
}

func TestEnqueueIsIdempotentPerDelivery(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	if err := db.Enqueue(ctx, sampleJob("delivery-1")); err != nil {
		t.Fatalf("first Enqueue returned error: %v", err)
	}
	err := db.Enqueue(ctx, sampleJob("delivery-1"))
	if !errors.Is(err, ErrDuplicateDelivery) {
		t.Fatalf("second Enqueue returned %v, want ErrDuplicateDelivery", err)
	}

	queued, err := db.CountJobs(ctx, StatusQueued)
	if err != nil {
		t.Fatalf("CountJobs returned error: %v", err)
	}
	if queued != 1 {
		t.Errorf("queued jobs = %d, want the retry to add none", queued)
	}
}

func TestFailJobRecordsTheError(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	if err := db.Enqueue(ctx, sampleJob("delivery-1")); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	job, err := db.ClaimJob(ctx)
	if err != nil || job == nil {
		t.Fatalf("ClaimJob returned (%v, %v), want a job", job, err)
	}

	if err := db.FailJob(ctx, job.ID, errors.New("model refused")); err != nil {
		t.Fatalf("FailJob returned error: %v", err)
	}

	failed, err := db.JobByDelivery(ctx, "delivery-1")
	if err != nil {
		t.Fatalf("JobByDelivery returned error: %v", err)
	}
	if failed.Status != StatusFailed {
		t.Errorf("status = %s, want %s", failed.Status, StatusFailed)
	}
	if failed.LastError != "model refused" {
		t.Errorf("last_error = %q, want the failure cause", failed.LastError)
	}
}

// A process killed mid-review leaves RUNNING rows behind; the next start puts
// them back in the queue (T-010-05).
func TestResetRunningJobs(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	if err := db.Enqueue(ctx, sampleJob("delivery-1")); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	if _, err := db.ClaimJob(ctx); err != nil {
		t.Fatalf("ClaimJob returned error: %v", err)
	}

	moved, err := db.ResetRunningJobs(ctx)
	if err != nil {
		t.Fatalf("ResetRunningJobs returned error: %v", err)
	}
	if moved != 1 {
		t.Errorf("ResetRunningJobs moved %d jobs, want 1", moved)
	}

	job, err := db.JobByDelivery(ctx, "delivery-1")
	if err != nil {
		t.Fatalf("JobByDelivery returned error: %v", err)
	}
	if job.Status != StatusQueued {
		t.Errorf("status = %s, want the interrupted job back in %s", job.Status, StatusQueued)
	}
}
