package server

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/joangrigorov/rumil/internal/reviewer"
	"github.com/joangrigorov/rumil/internal/store"
)

// A job that spent its own budget is recorded as failed rather than left
// RUNNING: the work runs on context.WithoutCancel, so the deadline is the job's
// own and cannot be cured by running it again.
func TestClassifyFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want jobFailure
	}{
		{
			name: "the job spent its budget",
			err:  fmt.Errorf("running the review session for acme/payments#42: %w", context.DeadlineExceeded),
			want: jobFailed,
		},
		{
			name: "the deadline arrived wrapped in a transport error",
			err:  fmt.Errorf("calling the model: %w", context.DeadlineExceeded),
			want: jobFailed,
		},
		{
			name: "the process is shutting down",
			err:  fmt.Errorf("running the review session: %w", context.Canceled),
			want: jobRetry,
		},
		{
			name: "anything else is a failure",
			err:  errors.New("github app not installed"),
			want: jobFailed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyFailure(tc.err); got != tc.want {
				t.Errorf("classifyFailure(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// A session that produced no review is FAILED even though it has a partial
// summary, and the partial text is still recorded for the observation deck.
func TestSessionOutcomeFailsAnUnsubmittedReview(t *testing.T) {
	result := &reviewer.Result{
		Summary:   "I'll start by reading the pull request metadata.",
		ToolCalls: 61,
		Steps:     25,
	}
	err := fmt.Errorf("%w: the model stopped", reviewer.ErrNoReviewSubmitted)

	outcome := sessionOutcome(result, err)
	if outcome.Status != store.ReviewFailed {
		t.Errorf("status = %q, want %q", outcome.Status, store.ReviewFailed)
	}
	if outcome.Error == "" {
		t.Error("error is empty, want the cause recorded")
	}
	if outcome.Summary != result.Summary {
		t.Errorf("summary = %q, want the partial text %q", outcome.Summary, result.Summary)
	}
	if outcome.ToolCalls != result.ToolCalls {
		t.Errorf("tool calls = %d, want %d", outcome.ToolCalls, result.ToolCalls)
	}
}
