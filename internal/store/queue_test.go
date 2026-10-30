package store

import (
	"context"
	"errors"
	"testing"
)

// The queue shows what is waiting and what is running, names the worker that
// claimed a running job, links its session, and lets both be cancelled.
func TestQueueListsAndCancels(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	if err := db.Enqueue(ctx, sampleJob("delivery-1")); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	if err := db.Enqueue(ctx, sampleJob("delivery-2")); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}

	running, err := db.ClaimJob(ctx, "worker-a")
	if err != nil || running == nil {
		t.Fatalf("ClaimJob returned (%v, %v), want a job", running, err)
	}
	if running.WorkerID != "worker-a" {
		t.Errorf("worker = %q, want the claiming worker", running.WorkerID)
	}

	session, err := db.CreateReviewSession(ctx, NewReviewSession{
		JobID: running.ID, InstallationID: running.InstallationID,
		OrgName: running.OrgName, RepoName: running.RepoName, PRNumber: running.PRNumber,
	})
	if err != nil {
		t.Fatalf("CreateReviewSession returned error: %v", err)
	}

	entries, err := db.ListQueue(ctx, 0)
	if err != nil {
		t.Fatalf("ListQueue returned error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("queue = %d entries, want the running and the queued job", len(entries))
	}
	byID := make(map[string]QueueEntry, len(entries))
	for _, entry := range entries {
		byID[entry.ID] = entry
	}
	runningEntry, ok := byID[running.ID]
	if !ok {
		t.Fatalf("the running job is not in the queue: %+v", entries)
	}
	if runningEntry.Status != StatusRunning || runningEntry.WorkerID != "worker-a" {
		t.Errorf("running entry = %+v, want RUNNING on worker-a", runningEntry)
	}
	if runningEntry.SessionID != session.ID {
		t.Errorf("session = %q, want %q", runningEntry.SessionID, session.ID)
	}

	queued, err := db.JobByDelivery(ctx, "delivery-2")
	if err != nil || queued == nil {
		t.Fatalf("JobByDelivery returned (%v, %v), want the queued job", queued, err)
	}
	if queued.Status != StatusQueued || queued.WorkerID != "" {
		t.Errorf("queued job = %+v, want QUEUED with no worker", queued)
	}

	// A queued job leaves the queue at once.
	cancelled, err := db.RequestJobCancel(ctx, queued.ID)
	if err != nil {
		t.Fatalf("RequestJobCancel(queued) returned error: %v", err)
	}
	if cancelled.Status != StatusCancelled || !cancelled.CancelRequested {
		t.Errorf("cancelled queued job = %+v, want CANCELLED with the flag set", cancelled)
	}
	if _, err := db.RequestJobCancel(ctx, queued.ID); !errors.Is(err, ErrJobFinished) {
		t.Errorf("cancelling a finished job = %v, want ErrJobFinished", err)
	}

	// A running job keeps its status until its worker stops, and the flag is
	// what the worker reads.
	requested, err := db.RequestJobCancel(ctx, running.ID)
	if err != nil {
		t.Fatalf("RequestJobCancel(running) returned error: %v", err)
	}
	if requested.Status != StatusRunning || !requested.CancelRequested {
		t.Errorf("running job after cancel = %+v, want RUNNING with the flag set", requested)
	}
	flag, err := db.JobCancelRequested(ctx, running.ID)
	if err != nil {
		t.Fatalf("JobCancelRequested returned error: %v", err)
	}
	if !flag {
		t.Error("JobCancelRequested = false, want the request")
	}

	entries, err = db.ListQueue(ctx, 0)
	if err != nil {
		t.Fatalf("ListQueue returned error: %v", err)
	}
	if len(entries) != 1 || entries[0].ID != running.ID || !entries[0].CancelRequested {
		t.Errorf("queue after the queued cancel = %+v, want the running job with its flag", entries)
	}

	if err := db.CancelJob(ctx, running.ID, "the review was cancelled by an operator"); err != nil {
		t.Fatalf("CancelJob returned error: %v", err)
	}
	stopped, err := db.JobByID(ctx, running.ID)
	if err != nil {
		t.Fatalf("JobByID returned error: %v", err)
	}
	if stopped.Status != StatusCancelled {
		t.Errorf("status = %q, want CANCELLED", stopped.Status)
	}
	entries, err = db.ListQueue(ctx, 0)
	if err != nil {
		t.Fatalf("ListQueue returned error: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("queue = %+v, want it empty", entries)
	}

	if _, err := db.RequestJobCancel(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrJobNotFound) {
		t.Errorf("cancelling an unknown job = %v, want ErrJobNotFound", err)
	}
}
