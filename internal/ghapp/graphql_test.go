package ghapp

import (
	"context"
	"strings"
	"testing"

	"github.com/joangrigorov/rumil/internal/testutil"
)

// T-020-07 (client half): the thread query returns resolution state, the
// replies, and the comment that opened each thread.
func TestReviewThreadsReportsResolutionState(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	gh.Threads = []testutil.ThreadEntry{
		{
			ID: "PRRT_open", Path: "main.go", Line: 12,
			Comments: []testutil.CommentEntry{
				testutil.ReviewComment(1, 0, "rumil[bot]", "This line wraps the call without a timeout."),
				testutil.ReviewComment(2, 1, "octocat", "we retry around it"),
			},
		},
		{
			ID: "PRRT_resolved", Path: "service.go", Line: 3, IsResolved: true, IsOutdated: true,
			Comments: []testutil.CommentEntry{
				testutil.ReviewComment(3, 0, "rumil[bot]", "The context is never cancelled."),
			},
		},
	}
	client := newTestClient(t, gh)

	threads, err := client.ReviewThreads(context.Background())
	if err != nil {
		t.Fatalf("ReviewThreads returned error: %v", err)
	}
	if len(threads) != 2 {
		t.Fatalf("read %d threads, want 2", len(threads))
	}

	open := threads[0]
	if open.IsResolved || open.IsOutdated {
		t.Errorf("the open thread reports %+v, want neither resolved nor outdated", open)
	}
	if len(open.Comments) != 2 || open.Comments[1].Author != "octocat" {
		t.Errorf("the replies did not come back: %+v", open.Comments)
	}
	root, ok := open.Root()
	if !ok || root.Author != "rumil[bot]" {
		t.Errorf("Root() = %+v (%v), want Rumil's comment", root, ok)
	}
	if open.Newest().IsZero() {
		t.Error("the thread carries no comment timestamps")
	}

	resolved := threads[1]
	if !resolved.IsResolved || !resolved.IsOutdated {
		t.Errorf("the resolved thread reports %+v, want both flags", resolved)
	}
}

// A thread the query returns as unresolved is what keeps a finding open across
// rounds, so the state has to come from the query rather than from a guess.
func TestReviewThreadsTrustsTheResolutionFlag(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	gh.Threads = []testutil.ThreadEntry{
		{ID: "PRRT_1", Path: "main.go", Line: 12, IsResolved: true, Comments: []testutil.CommentEntry{
			testutil.ReviewComment(1, 0, "rumil[bot]", "older thread"),
		}},
	}
	client := newTestClient(t, gh)

	threads, err := client.ReviewThreads(context.Background())
	if err != nil {
		t.Fatalf("ReviewThreads returned error: %v", err)
	}
	if len(threads) != 1 || !threads[0].IsResolved {
		t.Errorf("threads = %+v, want the resolved one", threads)
	}
}

// T-020-08 (client half): resolving goes through the mutation, and GitHub's
// answer decides whether it counts.
func TestResolveReviewThread(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	gh.Threads = []testutil.ThreadEntry{
		{ID: "PRRT_open", Path: "main.go", Line: 12, Comments: []testutil.CommentEntry{
			testutil.ReviewComment(1, 0, "rumil[bot]", "This line wraps the call without a timeout."),
		}},
	}
	client := newTestClient(t, gh)

	if err := client.ResolveReviewThread(context.Background(), "PRRT_open"); err != nil {
		t.Fatalf("ResolveReviewThread returned error: %v", err)
	}
	if resolved := gh.Resolved(); len(resolved) != 1 || resolved[0] != "PRRT_open" {
		t.Errorf("resolved %v, want PRRT_open", resolved)
	}

	threads, err := client.ReviewThreads(context.Background())
	if err != nil {
		t.Fatalf("ReviewThreads returned error: %v", err)
	}
	if !threads[0].IsResolved {
		t.Error("the thread is still open after the mutation")
	}
}

func TestResolveReviewThreadNeedsAnID(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	client := newTestClient(t, gh)

	if err := client.ResolveReviewThread(context.Background(), ""); err == nil {
		t.Error("ResolveReviewThread accepted an empty thread id")
	}
	if resolved := gh.Resolved(); len(resolved) != 0 {
		t.Errorf("resolved %v, want no call", resolved)
	}
}

// GraphQL answers a refused operation with HTTP 200 and an errors array, so the
// client has to read the array rather than the status.
func TestGraphQLErrorsAreReported(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	gh.GraphQLErrors = []string{"Resource not accessible by integration"}
	client := newTestClient(t, gh)

	err := client.ResolveReviewThread(context.Background(), "PRRT_open")
	if err == nil {
		t.Fatal("ResolveReviewThread hid a graphql error")
	}
	if !strings.Contains(err.Error(), "Resource not accessible by integration") {
		t.Errorf("error = %v, want the message github sent", err)
	}
	if _, err := client.ReviewThreads(context.Background()); err == nil {
		t.Error("ReviewThreads hid a graphql error")
	}
}
