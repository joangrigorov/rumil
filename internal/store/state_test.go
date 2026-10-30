package store

import (
	"context"
	"testing"
)

// The queue gauges come from the rows, not from a worker's memory, so a
// claimed job moves from queued to running and back on the next read.
func TestQueueStateCountsQueuedAndRunning(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	state, err := db.QueueState(ctx)
	if err != nil {
		t.Fatalf("QueueState returned error: %v", err)
	}
	if state.Queued != 0 || state.Running != 0 || state.OldestQueued != 0 {
		t.Fatalf("an empty queue reads as %+v, want zeroes", state)
	}

	for _, delivery := range []string{"d-state-1", "d-state-2"} {
		if err := db.Enqueue(ctx, sampleJob(delivery)); err != nil {
			t.Fatalf("enqueueing %s: %v", delivery, err)
		}
	}
	state, err = db.QueueState(ctx)
	if err != nil {
		t.Fatalf("QueueState returned error: %v", err)
	}
	if state.Queued != 2 || state.Running != 0 {
		t.Errorf("state = %+v, want 2 queued", state)
	}
	if state.OldestQueued <= 0 {
		t.Errorf("oldest queued age = %v, want a positive age", state.OldestQueued)
	}

	if job, err := db.ClaimJob(ctx, "worker-state"); err != nil {
		t.Fatalf("ClaimJob returned error: %v", err)
	} else if job == nil {
		t.Fatal("ClaimJob returned no job")
	}
	state, err = db.QueueState(ctx)
	if err != nil {
		t.Fatalf("QueueState returned error: %v", err)
	}
	if state.Queued != 1 || state.Running != 1 {
		t.Errorf("state = %+v, want 1 queued and 1 running", state)
	}
}

// Session counts follow the row's status, including the transition to the
// terminal status when the review finishes.
func TestSessionStateCountsByStatus(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	session, err := db.CreateReviewSession(ctx, NewReviewSession{
		InstallationID: 98765432, OrgName: testOrg, RepoName: testRepo, PRNumber: 42,
	})
	if err != nil {
		t.Fatalf("CreateReviewSession returned error: %v", err)
	}

	state, err := db.SessionState(ctx)
	if err != nil {
		t.Fatalf("SessionState returned error: %v", err)
	}
	if state.Running != 1 || state.Succeeded != 0 || state.Failed != 0 {
		t.Fatalf("state = %+v, want 1 running", state)
	}

	if err := db.FinishReviewSession(ctx, session.ID, ReviewOutcome{Status: ReviewSucceeded}); err != nil {
		t.Fatalf("FinishReviewSession returned error: %v", err)
	}
	state, err = db.SessionState(ctx)
	if err != nil {
		t.Fatalf("SessionState returned error: %v", err)
	}
	if state.Running != 0 || state.Succeeded != 1 || state.Failed != 0 {
		t.Errorf("state = %+v, want 1 succeeded", state)
	}
}
