package store

import (
	"context"
	"testing"
	"time"
)

func TestFindingOutcomes(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	session, err := db.CreateReviewSession(ctx, NewReviewSession{
		InstallationID: 98765432,
		OrgName:        "acme",
		RepoName:       "payments",
		PRNumber:       42,
		Model:          "model-a",
	})
	if err != nil {
		t.Fatalf("CreateReviewSession returned error: %v", err)
	}

	findings := []Finding{
		{SessionID: session.ID, InstallationID: 1, OrgName: "acme", RepoName: "payments", PRNumber: 42, ReviewID: 7001, CommentID: 9001, Path: "main.go", Line: 12, Body: "first"},
		{SessionID: session.ID, InstallationID: 1, OrgName: "acme", RepoName: "payments", PRNumber: 42, ReviewID: 7001, CommentID: 9002, Path: "main.go", Line: 20, Body: "second"},
		{SessionID: session.ID, InstallationID: 1, OrgName: "acme", RepoName: "payments", PRNumber: 42, ReviewID: 7002, CommentID: 9003, Path: "service.go", Line: 8, Body: "third"},
		{InstallationID: 1, OrgName: "acme", RepoName: "payments", PRNumber: 42, ReviewID: 7002, CommentID: 9004, Path: "service.go", Line: 9, Body: "unrecorded"},
	}
	if err := db.RecordFindings(ctx, findings); err != nil {
		t.Fatalf("RecordFindings returned error: %v", err)
	}
	// A retried delivery repeats the comments; the comment id keeps one row.
	if err := db.RecordFindings(ctx, findings); err != nil {
		t.Fatalf("re-recording findings returned error: %v", err)
	}

	if err := db.MarkFindingReplied(ctx, 9001); err != nil {
		t.Fatalf("MarkFindingReplied returned error: %v", err)
	}
	if err := db.MarkFindingsResolved(ctx, []int64{9003}); err != nil {
		t.Fatalf("MarkFindingsResolved returned error: %v", err)
	}
	if err := db.MarkFindingReaction(ctx, 9002, "+1"); err != nil {
		t.Fatalf("MarkFindingReaction returned error: %v", err)
	}
	if err := db.MarkReviewFindingsDismissed(ctx, 7002); err != nil {
		t.Fatalf("MarkReviewFindingsDismissed returned error: %v", err)
	}
	if err := db.MarkPRFindingsHeadMoved(ctx, "acme", "payments", 42); err != nil {
		t.Fatalf("MarkPRFindingsHeadMoved returned error: %v", err)
	}

	stats, err := db.FindingStatsByModel(ctx, FindingFilter{})
	if err != nil {
		t.Fatalf("FindingStatsByModel returned error: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("stats = %+v, want one row per model", stats)
	}
	byModel := make(map[string]FindingStats, len(stats))
	for _, row := range stats {
		byModel[row.Model] = row
	}
	recorded := byModel["model-a"]
	if recorded.Findings != 3 || recorded.Replied != 1 || recorded.Resolved != 1 || recorded.Reacted != 1 || recorded.Dismissed != 1 || recorded.HeadMoved != 3 {
		t.Errorf("model-a = %+v, want 3 findings with one of each signal and three moved", recorded)
	}
	unrecorded := byModel[""]
	if unrecorded.Findings != 1 || unrecorded.HeadMoved != 1 {
		t.Errorf("unrecorded = %+v, want the finding, with the push counted", unrecorded)
	}

	filtered, err := db.FindingStatsByModel(ctx, FindingFilter{OrgName: "acme", RepoName: "ledger"})
	if err != nil {
		t.Fatalf("FindingStatsByModel with a repository filter returned error: %v", err)
	}
	if len(filtered) != 0 {
		t.Errorf("another repository returned %+v, want nothing", filtered)
	}

	future := time.Now().Add(time.Hour)
	fresh, err := db.FindingStatsByModel(ctx, FindingFilter{Since: &future})
	if err != nil {
		t.Fatalf("FindingStatsByModel with a since filter returned error: %v", err)
	}
	if len(fresh) != 0 {
		t.Errorf("a future since returned %+v, want nothing", fresh)
	}
}
