package server_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/joangrigorov/rumil/internal/store"
	"github.com/joangrigorov/rumil/internal/testutil"
)

// seedFinding records one finding the way a submitted review would.
func seedFinding(t *testing.T, h *harness, reviewID, commentID int64) {
	t.Helper()
	if err := h.db.RecordFindings(context.Background(), []store.Finding{{
		InstallationID: testInstallationID,
		OrgName:        testOrg,
		RepoName:       testRepo,
		PRNumber:       testPRNumber,
		ReviewID:       reviewID,
		CommentID:      commentID,
		Path:           "main.go",
		Line:           12,
		Body:           "this wraps the call without a timeout",
	}}); err != nil {
		t.Fatalf("seeding a finding: %v", err)
	}
}

func findingStats(t *testing.T, h *harness) store.FindingStats {
	t.Helper()
	stats, err := h.db.FindingStatsByModel(context.Background(), store.FindingFilter{})
	if err != nil {
		t.Fatalf("reading finding outcomes: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("stats = %+v, want one row", stats)
	}
	return stats[0]
}

// A reply in a review thread is the clearest signal that somebody read the
// finding. It is recorded before the delivery is dropped as a non-command.
func TestAReplyToAFindingIsRecorded(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{})
	seedFinding(t, h, 7001, 9001)
	handler := h.webhook()

	body := reviewCommentPayload("created", "octocat", "User", "Good catch, fixed in the next commit.", 500, 9001)
	response := h.deliver(t, handler, "pull_request_review_comment", "d-finding-reply", body, sign(webhookSecret, body))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want the delivery ignored (body: %s)", response.Code, response.Body.String())
	}

	if stats := findingStats(t, h); stats.Replied != 1 {
		t.Errorf("replied = %d, want the finding marked answered", stats.Replied)
	}
}

func TestAFindingReactionIsRecorded(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{})
	seedFinding(t, h, 7001, 9001)
	handler := h.webhook()

	payload := basePayload("created")
	payload["reaction"] = map[string]any{"content": "+1"}
	payload["comment"] = map[string]any{
		"id":   9001,
		"user": map[string]any{"login": "octocat", "type": "User"},
	}
	body := mustJSON(payload)

	if response := h.deliver(t, handler, "reaction", "d-finding-reaction", body, sign(webhookSecret, body)); response.Code != http.StatusOK {
		t.Fatalf("status = %d, want the reaction accepted (body: %s)", response.Code, response.Body.String())
	}

	if stats := findingStats(t, h); stats.Reacted != 1 {
		t.Errorf("reacted = %d, want the finding marked reacted to", stats.Reacted)
	}
}

// A dismissed review says its findings were not accepted.
func TestADismissedReviewMarksItsFindings(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{})
	seedFinding(t, h, 7001, 9001)
	handler := h.webhook()

	payload := basePayload("dismissed")
	payload["review"] = map[string]any{"id": 7001}
	body := mustJSON(payload)

	if response := h.deliver(t, handler, "pull_request_review", "d-finding-dismissed", body, sign(webhookSecret, body)); response.Code != http.StatusOK {
		t.Fatalf("status = %d, want the dismissal accepted (body: %s)", response.Code, response.Body.String())
	}

	if stats := findingStats(t, h); stats.Dismissed != 1 {
		t.Errorf("dismissed = %d, want the review's findings marked", stats.Dismissed)
	}
}

// A push after a round is a proxy for "the code changed", not a verdict.
func TestAPushMarksTheRoundsFindings(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{})
	seedFinding(t, h, 7001, 9001)
	handler := h.webhook()

	payload := basePayload("synchronize")
	payload["pull_request"] = map[string]any{"number": testPRNumber}
	body := mustJSON(payload)

	if response := h.deliver(t, handler, "pull_request", "d-finding-push", body, sign(webhookSecret, body)); response.Code != http.StatusOK {
		t.Fatalf("status = %d, want the push ignored as a job and recorded (body: %s)", response.Code, response.Body.String())
	}

	if stats := findingStats(t, h); stats.HeadMoved != 1 {
		t.Errorf("head moved = %d, want the round marked", stats.HeadMoved)
	}
}

// Feedback from an installation Rumil does not serve is not recorded.
func TestFindingFeedbackFromAnotherInstallationIsIgnored(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{})
	seedFinding(t, h, 7001, 9001)
	handler := h.webhook()

	payload := basePayload("created")
	payload["reaction"] = map[string]any{"content": "heart"}
	payload["comment"] = map[string]any{
		"id":   9001,
		"user": map[string]any{"login": "octocat", "type": "User"},
	}
	payload["installation"] = map[string]any{"id": testInstallationID + 1}
	body := mustJSON(payload)

	h.deliver(t, handler, "reaction", "d-finding-other-installation", body, sign(webhookSecret, body))

	if stats := findingStats(t, h); stats.Reacted != 0 {
		t.Errorf("reacted = %d, want another installation's feedback ignored", stats.Reacted)
	}
}
