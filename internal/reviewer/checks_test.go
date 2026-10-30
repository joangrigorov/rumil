package reviewer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/joangrigorov/rumil/internal/ghapp"
	"github.com/joangrigorov/rumil/internal/testutil"
)

func TestCheckConclusion(t *testing.T) {
	for _, tc := range []struct {
		name       string
		result     *Result
		err        error
		conclusion string
		summary    string
	}{
		{"a cancelled review", nil, context.Canceled, "cancelled", "cancelled"},
		{"a review an operator stopped", nil, ErrReviewCancelled, "cancelled", "cancelled"},
		{"a failed review", nil, errors.New("the provider is down"), "failure", "the provider is down"},
		{"a review that never submitted", &Result{}, nil, "failure", "without submitting"},
		{"an approval", &Result{ReviewSubmitted: true, Event: ghapp.EventApprove, ToolCalls: 4}, nil, "success", "Approved"},
		{"requested changes", &Result{ReviewSubmitted: true, Event: ghapp.EventRequestChanges, ToolCalls: 6}, nil, "action_required", "Changes requested"},
		{"a comment", &Result{ReviewSubmitted: true, Event: ghapp.EventComment, ToolCalls: 3}, nil, "neutral", "Review submitted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conclusion, summary := checkConclusion(tc.result, tc.err)
			if conclusion != tc.conclusion {
				t.Errorf("conclusion = %q, want %q", conclusion, tc.conclusion)
			}
			if !strings.Contains(summary, tc.summary) {
				t.Errorf("summary = %q, want it to contain %q", summary, tc.summary)
			}
		})
	}
}

// A review opens one check run at the head commit, completes it with the
// review's outcome, and links to the session when a public address is set.
func TestReviewReportsACheckRun(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	llm.QueueToolCall("submit_pr_review", map[string]any{
		"event":        "COMMENT",
		"summary_body": "Looks fine.",
	})
	llm.QueueText("Review submitted.")

	h := newHarness(t, gh, llm, harnessOpts{publicURL: "https://rumil.example.com"})

	if _, err := h.engine.Review(context.Background(), Request{
		Owner: gh.Owner, Repo: gh.Repo, Number: gh.Number, SessionID: "session-1",
	}); err != nil {
		t.Fatalf("Review returned error: %v\nlogs:\n%s", err, h.logs.String())
	}

	runs := gh.CheckRunSnapshot()
	if len(runs) != 1 {
		t.Fatalf("check runs = %d, want one", len(runs))
	}
	run := runs[0]
	if run.Name != ghapp.ReviewCheckName {
		t.Errorf("name = %q, want %q", run.Name, ghapp.ReviewCheckName)
	}
	if run.HeadSHA != gh.HeadSHA {
		t.Errorf("head_sha = %q, want %q", run.HeadSHA, gh.HeadSHA)
	}
	if run.Status != "completed" || run.Conclusion != "neutral" {
		t.Errorf("state = %s/%s, want completed/neutral", run.Status, run.Conclusion)
	}
	if run.DetailsURL != "https://rumil.example.com/ui/sessions/session-1" {
		t.Errorf("details_url = %q", run.DetailsURL)
	}
	if !strings.Contains(run.Summary, "Review submitted") {
		t.Errorf("summary = %q, want the review outcome", run.Summary)
	}
}

// A session that never submits completes the check as a failure, so the pull
// request shows why there is no review.
func TestAFailedReviewCompletesTheCheckRun(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	llm.QueueText("I'll compare the files directly.")
	llm.QueueText("Still comparing.")

	h := newHarness(t, gh, llm, harnessOpts{})

	if _, err := h.review(t); !errors.Is(err, ErrNoReviewSubmitted) {
		t.Fatalf("Review error = %v, want ErrNoReviewSubmitted", err)
	}

	runs := gh.CheckRunSnapshot()
	if len(runs) != 1 {
		t.Fatalf("check runs = %d, want one", len(runs))
	}
	if runs[0].Conclusion != "failure" {
		t.Errorf("conclusion = %q, want failure", runs[0].Conclusion)
	}
	if !strings.Contains(runs[0].Summary, "without submitting") {
		t.Errorf("summary = %q, want the no-review reason", runs[0].Summary)
	}
}

// A second attempt at the same commit updates the existing check run instead
// of adding a second one.
func TestAContinuationReusesTheCheckRun(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	for i := 0; i < 2; i++ {
		llm.QueueToolCall("submit_pr_review", map[string]any{
			"event":        "COMMENT",
			"summary_body": "Looks fine.",
		})
		llm.QueueText("Review submitted.")
	}

	h := newHarness(t, gh, llm, harnessOpts{})
	req := Request{Owner: gh.Owner, Repo: gh.Repo, Number: gh.Number, SessionID: "session-1"}
	for _, sessionID := range []string{"session-1", "session-2"} {
		req.SessionID = sessionID
		if _, err := h.engine.Review(context.Background(), req); err != nil {
			t.Fatalf("Review %s returned error: %v\nlogs:\n%s", sessionID, err, h.logs.String())
		}
	}

	runs := gh.CheckRunSnapshot()
	if len(runs) != 1 {
		t.Fatalf("check runs = %d, want the first one updated", len(runs))
	}
	if runs[0].ID != 1 {
		t.Errorf("check run id = %d, want the reused id", runs[0].ID)
	}
	if runs[0].Status != "completed" {
		t.Errorf("status = %q, want completed", runs[0].Status)
	}
}

// An App without the Checks permission still reviews: the check is best effort.
func TestACheckRunPermissionFailureDoesNotFailTheReview(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	gh.CheckStatus = 403
	llm := testutil.NewFakeLLM(t)
	llm.QueueToolCall("submit_pr_review", map[string]any{
		"event":        "COMMENT",
		"summary_body": "Looks fine.",
	})
	llm.QueueText("Review submitted.")

	h := newHarness(t, gh, llm, harnessOpts{})

	result, err := h.review(t)
	if err != nil {
		t.Fatalf("Review returned error: %v\nlogs:\n%s", err, h.logs.String())
	}
	if !result.ReviewSubmitted {
		t.Error("the review did not submit")
	}
	if !strings.Contains(h.logs.String(), "starting the review check run failed") {
		t.Errorf("logs do not record the refused check run:\n%s", h.logs.String())
	}
}
