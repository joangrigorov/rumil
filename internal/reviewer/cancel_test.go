package reviewer

import (
	"context"
	"errors"
	"testing"

	"github.com/joangrigorov/rumil/internal/testutil"
)

// A cancellation is read between steps, so the model makes no further request
// and no partial review is submitted.
func TestReviewStopsWhenCancelledBetweenSteps(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	llm.QueueToolCall("get_pr_metadata", map[string]any{})
	llm.QueueToolCall("submit_pr_review", map[string]any{
		"event": "COMMENT", "summary_body": "Looks fine.",
	})
	llm.QueueText("Review submitted.")

	h := newHarness(t, gh, llm, harnessOpts{})

	// The first two checks are the first step and its tool call; by the second
	// step the operator has clicked cancel.
	checks := 0
	result, err := h.engine.Review(context.Background(), Request{
		Owner: gh.Owner, Repo: gh.Repo, Number: gh.Number,
		Cancelled: func() bool {
			checks++
			return checks > 2
		},
	})
	if !errors.Is(err, ErrReviewCancelled) {
		t.Fatalf("Review error = %v, want ErrReviewCancelled\nlogs:\n%s", err, h.logs.String())
	}
	if result == nil {
		t.Fatal("Review returned no result")
	}
	if result.ReviewSubmitted {
		t.Error("the cancelled review submitted")
	}
	if llm.Count() != 1 {
		t.Errorf("model requests = %d, want one: the cancel stops before the next step", llm.Count())
	}
	if len(gh.SubmittedReviews()) != 0 {
		t.Error("the cancelled review reached GitHub")
	}
}

// Once a review is posted, a cancellation that arrives afterwards is ignored:
// there is nothing left to stop, and the review is not taken back.
func TestACancelAfterSubmitDoesNotUndoTheReview(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	llm.QueueToolCall("submit_pr_review", map[string]any{
		"event": "COMMENT", "summary_body": "Looks fine.",
	})
	llm.QueueText("Review submitted.")

	h := newHarness(t, gh, llm, harnessOpts{})

	result, err := h.engine.Review(context.Background(), Request{
		Owner: gh.Owner, Repo: gh.Repo, Number: gh.Number,
		Cancelled: func() bool { return len(gh.SubmittedReviews()) > 0 },
	})
	if err != nil {
		t.Fatalf("Review returned error: %v\nlogs:\n%s", err, h.logs.String())
	}
	if !result.ReviewSubmitted {
		t.Error("the review did not submit")
	}
	if len(gh.SubmittedReviews()) != 1 {
		t.Errorf("submitted reviews = %d, want one", len(gh.SubmittedReviews()))
	}
}
