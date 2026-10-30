package server

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/joangrigorov/rumil/internal/store"
	"github.com/joangrigorov/rumil/internal/testutil"
)

// The watcher turns the database flag into the atomic bool the engine reads
// between steps.
func TestCancelWatcherSeesTheRequest(t *testing.T) {
	ctx := context.Background()
	dsn := testutil.TestDatabaseURL(t, "cancel_watcher")
	db, err := store.Open(ctx, dsn, true, 0)
	if err != nil {
		t.Fatalf("opening the test database: %v", err)
	}
	t.Cleanup(db.Close)

	if err := db.Enqueue(ctx, store.Job{
		DeliveryID: "delivery-1", InstallationID: 1, EventType: "pull_request",
		OrgName: "acme", RepoName: "payments", PRNumber: 42, Payload: []byte(`{}`),
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	job, err := db.ClaimJob(ctx, "worker-a")
	if err != nil || job == nil {
		t.Fatalf("ClaimJob returned (%v, %v), want a job", job, err)
	}

	watcher := newCancelWatcher(ctx, db, job.ID, slog.New(slog.DiscardHandler))
	defer watcher.Close()
	if watcher.Stopped() {
		t.Fatal("a fresh watcher reports a cancellation")
	}

	if _, err := db.RequestJobCancel(ctx, job.ID); err != nil {
		t.Fatalf("RequestJobCancel returned error: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for !watcher.Stopped() {
		if time.Now().After(deadline) {
			t.Fatal("the watcher did not see the cancellation within the poll interval")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
