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

func TestReviewSessionContinuationRecordsTheAttempt(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	parent, err := db.CreateReviewSession(ctx, NewReviewSession{
		InstallationID: 98765432,
		OrgName:        "acme",
		RepoName:       "payments",
		PRNumber:       42,
		Model:          "test-model",
	})
	if err != nil {
		t.Fatalf("creating the parent session: %v", err)
	}
	if err := db.UpdateReviewSessionMetadata(ctx, parent.ID, "Fix the flap", "headsha1", "basesha1"); err != nil {
		t.Fatalf("updating the parent metadata: %v", err)
	}
	outcome := ReviewOutcome{
		Status:           ReviewFailed,
		Summary:          "I had read the diff and was checking the retry loop.",
		ToolCalls:        4,
		Error:            "review session ended without submitting a review for acme/payments#42 after 6 steps and 4 tool calls (turn limit 6 reached)",
		TurnLimitReached: true,
	}
	if err := db.FinishReviewSession(ctx, parent.ID, outcome); err != nil {
		t.Fatalf("finishing the parent session: %v", err)
	}

	parent, err = db.ReviewSessionByID(ctx, parent.ID)
	if err != nil {
		t.Fatalf("reading the parent session: %v", err)
	}
	if !parent.TurnLimitReached {
		t.Error("turn_limit_reached = false, want the recorded cause")
	}
	if parent.ParentSessionID != "" {
		t.Errorf("parent_session_id = %q, want empty on a first attempt", parent.ParentSessionID)
	}
	if err := parent.Continuable(); err != nil {
		t.Fatalf("the turn-limited session is not continuable: %v", err)
	}

	requestedAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	child, err := db.CreateReviewSession(ctx, NewReviewSession{
		InstallationID:  98765432,
		OrgName:         "acme",
		RepoName:        "payments",
		PRNumber:        42,
		Model:           "test-model",
		ParentSessionID: parent.ID,
		RequestedBy:     "alice",
		RequestedAt:     &requestedAt,
	})
	if err != nil {
		t.Fatalf("creating the continuation session: %v", err)
	}
	if child.ParentSessionID != parent.ID {
		t.Errorf("parent_session_id = %q, want %q", child.ParentSessionID, parent.ID)
	}
	if child.RequestedBy != "alice" {
		t.Errorf("requested_by = %q, want alice", child.RequestedBy)
	}
	if child.RequestedAt == nil || !child.RequestedAt.Equal(requestedAt) {
		t.Errorf("requested_at = %v, want %v", child.RequestedAt, requestedAt)
	}

	found, err := db.ReviewSessionContinuation(ctx, parent.ID)
	if err != nil {
		t.Fatalf("ReviewSessionContinuation returned error: %v", err)
	}
	if found == nil || found.ID != child.ID {
		t.Fatalf("continuation = %+v, want session %s", found, child.ID)
	}

	none, err := db.ReviewSessionContinuation(ctx, child.ID)
	if err != nil {
		t.Fatalf("ReviewSessionContinuation(child) returned error: %v", err)
	}
	if none != nil {
		t.Errorf("continuation of the child = %+v, want nil", none)
	}
}

func TestReviewSessionContinuable(t *testing.T) {
	cases := []struct {
		name    string
		session ReviewSession
		want    error
	}{
		{
			name:    "a turn-limited failure",
			session: ReviewSession{Status: ReviewFailed, TurnLimitReached: true},
		},
		{
			name:    "a failure recorded before the marker existed",
			session: ReviewSession{Status: ReviewFailed, Error: "review session ended without submitting a review (turn limit 25 reached)"},
		},
		{
			name:    "a session that submitted a review",
			session: ReviewSession{Status: ReviewSucceeded, ReviewEvent: "COMMENT"},
			want:    ErrReviewAlreadySubmitted,
		},
		{
			name:    "a failed session that still submitted a review",
			session: ReviewSession{Status: ReviewFailed, ReviewEvent: "COMMENT", TurnLimitReached: true},
			want:    ErrReviewAlreadySubmitted,
		},
		{
			name:    "a session that is still running",
			session: ReviewSession{Status: ReviewRunning, TurnLimitReached: true},
			want:    ErrReviewNotFailed,
		},
		{
			name:    "a failure with another cause",
			session: ReviewSession{Status: ReviewFailed, Error: "the GitHub App is not installed"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.session.Continuable(); !errors.Is(err, tc.want) {
				t.Errorf("Continuable() = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestReviewTranscriptEventsKeepsWhatTheSessionLearned(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	session, err := db.CreateReviewSession(ctx, NewReviewSession{
		InstallationID: 98765432,
		OrgName:        "acme",
		RepoName:       "payments",
		PRNumber:       42,
	})
	if err != nil {
		t.Fatalf("creating the session: %v", err)
	}

	now := time.Now()
	events := []NewReviewEvent{
		{Seq: 1, At: now, Type: "tool_call", Payload: json.RawMessage(`{"id":"call_1","name":"get_pr_metadata","args":{}}`)},
		{Seq: 2, At: now, Type: "reasoning_delta", Payload: json.RawMessage(`{"delta":"thinking"}`)},
		{Seq: 3, At: now, Type: "tool_result", Payload: json.RawMessage(`{"id":"call_1","name":"get_pr_metadata","result":"the change adds retries"}`)},
		{Seq: 4, At: now, Type: "step_start", Payload: json.RawMessage(`{"step":2}`)},
		{Seq: 5, At: now, Type: "message_end", Payload: json.RawMessage(`{"content":"I should check the retry bound."}`)},
		{Seq: 6, At: now, Type: "compaction", Payload: json.RawMessage(`{"summary":"the retry helper is bounded"}`)},
	}
	if err := db.AppendReviewEvents(ctx, session.ID, events); err != nil {
		t.Fatalf("appending events: %v", err)
	}

	all, err := db.ReviewTranscriptEvents(ctx, session.ID, 0)
	if err != nil {
		t.Fatalf("ReviewTranscriptEvents returned error: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("transcript events = %d, want the four that carry context: %+v", len(all), all)
	}
	for i, want := range []string{"tool_call", "tool_result", "message_end", "compaction"} {
		if all[i].Type != want {
			t.Errorf("event %d is %q, want %q", i, all[i].Type, want)
		}
	}

	recent, err := db.ReviewTranscriptEvents(ctx, session.ID, 2)
	if err != nil {
		t.Fatalf("ReviewTranscriptEvents(limit 2) returned error: %v", err)
	}
	if len(recent) != 2 || recent[0].Seq != 5 || recent[1].Seq != 6 {
		t.Errorf("recent transcript = %+v, want seqs 5 and 6 in order", recent)
	}
}

// A finished session keeps what the provider reported; a provider that reported
// nothing leaves the columns NULL rather than zero.
func TestReviewSessionUsage(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	create := func(pr int) ReviewSession {
		t.Helper()
		session, err := db.CreateReviewSession(ctx, NewReviewSession{
			InstallationID: 98765432,
			OrgName:        "acme",
			RepoName:       "payments",
			PRNumber:       pr,
			Model:          "test-model",
		})
		if err != nil {
			t.Fatalf("CreateReviewSession returned error: %v", err)
		}
		return session
	}

	cost := 0.0123
	priced := create(1)
	if err := db.FinishReviewSession(ctx, priced.ID, ReviewOutcome{
		Status: ReviewSucceeded,
		Usage: &TokenUsage{
			InputTokens: 1000, OutputTokens: 250, CacheReadTokens: 400,
			CacheCreationTokens: 50, CostUSD: &cost,
		},
	}); err != nil {
		t.Fatalf("FinishReviewSession returned error: %v", err)
	}

	stored, err := db.ReviewSessionByID(ctx, priced.ID)
	if err != nil {
		t.Fatalf("ReviewSessionByID returned error: %v", err)
	}
	if stored.Usage == nil {
		t.Fatal("the stored session lost its usage")
	}
	want := TokenUsage{
		InputTokens: 1000, OutputTokens: 250, CacheReadTokens: 400,
		CacheCreationTokens: 50,
	}
	if stored.Usage.InputTokens != want.InputTokens ||
		stored.Usage.OutputTokens != want.OutputTokens ||
		stored.Usage.CacheReadTokens != want.CacheReadTokens ||
		stored.Usage.CacheCreationTokens != want.CacheCreationTokens {
		t.Errorf("usage = %+v, want %+v", *stored.Usage, want)
	}
	if stored.Usage.CostUSD == nil || *stored.Usage.CostUSD != cost {
		t.Errorf("cost = %v, want %v", stored.Usage.CostUSD, cost)
	}
	if stored.Usage.TotalTokens() != 1700 {
		t.Errorf("total tokens = %d, want 1700", stored.Usage.TotalTokens())
	}

	unreported := create(2)
	if err := db.FinishReviewSession(ctx, unreported.ID, ReviewOutcome{Status: ReviewSucceeded}); err != nil {
		t.Fatalf("FinishReviewSession returned error: %v", err)
	}
	stored, err = db.ReviewSessionByID(ctx, unreported.ID)
	if err != nil {
		t.Fatalf("ReviewSessionByID returned error: %v", err)
	}
	if stored.Usage != nil {
		t.Errorf("usage = %+v, want nil when nothing was reported", *stored.Usage)
	}

	totals, err := db.ReviewSessionUsage(ctx, ReviewSessionFilter{OrgName: "acme", RepoName: "payments"})
	if err != nil {
		t.Fatalf("ReviewSessionUsage returned error: %v", err)
	}
	if totals.Sessions != 2 || totals.Reported != 1 {
		t.Errorf("sessions/reported = %d/%d, want 2/1", totals.Sessions, totals.Reported)
	}
	if totals.TotalTokens() != 1700 {
		t.Errorf("total tokens = %d, want 1700", totals.TotalTokens())
	}
	if totals.CostReported != 1 || totals.CostUSD != cost {
		t.Errorf("cost = %v over %d sessions, want %v over one", totals.CostUSD, totals.CostReported, cost)
	}
}
