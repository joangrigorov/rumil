package server

import (
	"context"
	"errors"
	"fmt"
	"testing"
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
