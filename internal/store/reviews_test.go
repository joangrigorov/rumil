package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestReviewSessionLifecycle(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	session, err := db.CreateReviewSession(ctx, NewReviewSession{
		InstallationID: 98765432,
		OrgName:        "acme",
		RepoName:       "payments",
		PRNumber:       42,
		Model:          "test-model",
	})
	if err != nil {
		t.Fatalf("CreateReviewSession returned error: %v", err)
	}
	if session.ID == "" {
		t.Fatal("the created session has no id")
	}
	if session.Status != ReviewRunning {
		t.Errorf("status = %q, want %q", session.Status, ReviewRunning)
	}
	if session.StartedAt.IsZero() {
		t.Error("started_at is zero")
	}
	if session.FinishedAt != nil {
		t.Errorf("finished_at = %v, want nil for a running session", session.FinishedAt)
	}

	if err := db.UpdateReviewSessionMetadata(ctx, session.ID, "Fix the flap", "headsha1", "basesha1"); err != nil {
		t.Fatalf("UpdateReviewSessionMetadata returned error: %v", err)
	}

	events := []NewReviewEvent{
		{Seq: 1, At: time.Now().Add(-2 * time.Second), Type: "step_start", Payload: json.RawMessage(`{"step":1}`)},
		{Seq: 2, At: time.Now().Add(-time.Second), Type: "tool_call", Payload: json.RawMessage(`{"name":"read_file"}`)},
	}
	if err := db.AppendReviewEvents(ctx, session.ID, events); err != nil {
		t.Fatalf("AppendReviewEvents returned error: %v", err)
	}
	// A writer that flushes after a restart repeats a seq; the batch is
	// absorbed rather than taking the session down.
	if err := db.AppendReviewEvents(ctx, session.ID, events[:1]); err != nil {
		t.Fatalf("re-appending an event returned error: %v", err)
	}

	stored, err := db.ReviewEventsAfter(ctx, session.ID, 0, 100)
	if err != nil {
		t.Fatalf("ReviewEventsAfter returned error: %v", err)
	}
	if len(stored) != 2 {
		t.Fatalf("stored events = %d, want 2", len(stored))
	}
	if stored[0].Seq != 1 || stored[1].Seq != 2 {
		t.Errorf("seqs = %d, %d, want 1, 2", stored[0].Seq, stored[1].Seq)
	}
	if stored[1].Type != "tool_call" {
		t.Errorf("type = %q, want tool_call", stored[1].Type)
	}
	if string(stored[1].Payload) != `{"name": "read_file"}` {
		t.Errorf("payload = %s, want the stored JSON object", stored[1].Payload)
	}

	after, err := db.ReviewEventsAfter(ctx, session.ID, 1, 100)
	if err != nil {
		t.Fatalf("ReviewEventsAfter(after 1) returned error: %v", err)
	}
	if len(after) != 1 || after[0].Seq != 2 {
		t.Errorf("events after 1 = %+v, want only seq 2", after)
	}

	outcome := ReviewOutcome{
		Status:      ReviewSucceeded,
		Summary:     "The change is sound.",
		ReviewEvent: "COMMENT",
		ToolCalls:   2,
	}
	if err := db.FinishReviewSession(ctx, session.ID, outcome); err != nil {
		t.Fatalf("FinishReviewSession returned error: %v", err)
	}

	finished, err := db.ReviewSessionByID(ctx, session.ID)
	if err != nil {
		t.Fatalf("ReviewSessionByID returned error: %v", err)
	}
	if finished.Status != ReviewSucceeded {
		t.Errorf("status = %q, want %q", finished.Status, ReviewSucceeded)
	}
	if finished.PRTitle != "Fix the flap" || finished.HeadSHA != "headsha1" || finished.BaseSHA != "basesha1" {
		t.Errorf("metadata = %q/%q/%q, want the updated title and shas", finished.PRTitle, finished.HeadSHA, finished.BaseSHA)
	}
	if finished.Summary != outcome.Summary || finished.ReviewEvent != outcome.ReviewEvent {
		t.Errorf("outcome = %q/%q, want %q/%q", finished.Summary, finished.ReviewEvent, outcome.Summary, outcome.ReviewEvent)
	}
	if finished.ToolCalls != 2 {
		t.Errorf("tool_calls = %d, want 2", finished.ToolCalls)
	}
	if finished.FinishedAt == nil {
		t.Error("finished_at is nil")
	}

	list, err := db.ListReviewSessions(ctx, ReviewSessionFilter{})
	if err != nil {
		t.Fatalf("ListReviewSessions returned error: %v", err)
	}
	if len(list) != 1 || list[0].ID != session.ID {
		t.Fatalf("sessions = %+v, want the one session", list)
	}

	matched, err := db.ListReviewSessions(ctx, ReviewSessionFilter{Status: ReviewSucceeded})
	if err != nil {
		t.Fatalf("ListReviewSessions(status) returned error: %v", err)
	}
	if len(matched) != 1 {
		t.Errorf("sessions with status %s = %d, want 1", ReviewSucceeded, len(matched))
	}

	unmatched, err := db.ListReviewSessions(ctx, ReviewSessionFilter{Status: ReviewFailed})
	if err != nil {
		t.Fatalf("ListReviewSessions(failed) returned error: %v", err)
	}
	if len(unmatched) != 0 {
		t.Errorf("sessions with status %s = %d, want 0", ReviewFailed, len(unmatched))
	}

	repository, err := db.ListReviewSessions(ctx, ReviewSessionFilter{OrgName: "acme", RepoName: "payments"})
	if err != nil {
		t.Fatalf("ListReviewSessions(repo) returned error: %v", err)
	}
	if len(repository) != 1 {
		t.Errorf("sessions for acme/payments = %d, want 1", len(repository))
	}

	other, err := db.ListReviewSessions(ctx, ReviewSessionFilter{OrgName: "acme", RepoName: "other"})
	if err != nil {
		t.Fatalf("ListReviewSessions(other repo) returned error: %v", err)
	}
	if len(other) != 0 {
		t.Errorf("sessions for acme/other = %d, want 0", len(other))
	}
}

func TestReviewSessionNotFound(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	missing := "00000000-0000-0000-0000-000000000000"
	if _, err := db.ReviewSessionByID(ctx, missing); !errors.Is(err, ErrReviewSessionNotFound) {
		t.Errorf("ReviewSessionByID error = %v, want ErrReviewSessionNotFound", err)
	}
	if err := db.FinishReviewSession(ctx, missing, ReviewOutcome{Status: ReviewSucceeded}); !errors.Is(err, ErrReviewSessionNotFound) {
		t.Errorf("FinishReviewSession error = %v, want ErrReviewSessionNotFound", err)
	}
	if err := db.UpdateReviewSessionMetadata(ctx, missing, "title", "", ""); !errors.Is(err, ErrReviewSessionNotFound) {
		t.Errorf("UpdateReviewSessionMetadata error = %v, want ErrReviewSessionNotFound", err)
	}
}

func TestInterruptRunningReviewSessions(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	running, err := db.CreateReviewSession(ctx, NewReviewSession{
		InstallationID: 98765432,
		OrgName:        "acme",
		RepoName:       "payments",
		PRNumber:       42,
	})
	if err != nil {
		t.Fatalf("creating the running session: %v", err)
	}
	finished, err := db.CreateReviewSession(ctx, NewReviewSession{
		InstallationID: 98765432,
		OrgName:        "acme",
		RepoName:       "payments",
		PRNumber:       43,
	})
	if err != nil {
		t.Fatalf("creating the finished session: %v", err)
	}
	if err := db.FinishReviewSession(ctx, finished.ID, ReviewOutcome{Status: ReviewSucceeded}); err != nil {
		t.Fatalf("finishing the session: %v", err)
	}

	interrupted, err := db.InterruptRunningReviewSessions(ctx)
	if err != nil {
		t.Fatalf("InterruptRunningReviewSessions returned error: %v", err)
	}
	if interrupted != 1 {
		t.Errorf("interrupted = %d, want 1", interrupted)
	}

	row, err := db.ReviewSessionByID(ctx, running.ID)
	if err != nil {
		t.Fatalf("ReviewSessionByID returned error: %v", err)
	}
	if row.Status != ReviewFailed {
		t.Errorf("status = %q, want %q", row.Status, ReviewFailed)
	}
	if row.Error == "" {
		t.Error("the interrupted session has no cause")
	}
	if row.FinishedAt == nil {
		t.Error("the interrupted session has no finish time")
	}

	kept, err := db.ReviewSessionByID(ctx, finished.ID)
	if err != nil {
		t.Fatalf("ReviewSessionByID returned error: %v", err)
	}
	if kept.Status != ReviewSucceeded {
		t.Errorf("the finished session moved to %q", kept.Status)
	}

	again, err := db.InterruptRunningReviewSessions(ctx)
	if err != nil {
		t.Fatalf("the second InterruptRunningReviewSessions returned error: %v", err)
	}
	if again != 0 {
		t.Errorf("second interrupt = %d, want 0", again)
	}
}

func TestListReviewSessionsIsNewestFirstAndBounded(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	for _, number := range []int{1, 2, 3} {
		if _, err := db.CreateReviewSession(ctx, NewReviewSession{
			InstallationID: 98765432,
			OrgName:        "acme",
			RepoName:       "payments",
			PRNumber:       number,
		}); err != nil {
			t.Fatalf("creating session %d: %v", number, err)
		}
	}

	sessions, err := db.ListReviewSessions(ctx, ReviewSessionFilter{Limit: 2})
	if err != nil {
		t.Fatalf("ListReviewSessions returned error: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("sessions = %d, want the limit of 2", len(sessions))
	}
	if sessions[0].PRNumber != 3 || sessions[1].PRNumber != 2 {
		t.Errorf("order = #%d, #%d, want #3, #2", sessions[0].PRNumber, sessions[1].PRNumber)
	}
}
