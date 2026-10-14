package ghapp

import (
	"context"
	"strconv"
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

// threadFixture builds n threads whose ids order them, so a paging test can say
// which threads came back.
func threadFixture(n int) []testutil.ThreadEntry {
	threads := make([]testutil.ThreadEntry, 0, n)
	for i := 0; i < n; i++ {
		threads = append(threads, testutil.ThreadEntry{
			ID: "PRRT_" + strconv.Itoa(i), Path: "main.go", Line: i + 1,
			Comments: []testutil.CommentEntry{
				testutil.ReviewComment(int64(10+i), 0, "rumil[bot]", "finding "+strconv.Itoa(i)),
			},
		})
	}
	return threads
}

// A pull request with more threads than one page holds is read across pages: a
// later round that lost half the history would repeat findings it cannot see.
func TestReviewThreadsFollowsTheCursor(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	gh.Threads = threadFixture(5)
	gh.ThreadPageSize = 2
	client := newTestClient(t, gh)

	threads, err := client.ReviewThreads(context.Background())
	if err != nil {
		t.Fatalf("ReviewThreads returned error: %v", err)
	}
	if len(threads) != 5 {
		t.Fatalf("read %d threads, want all 5 across three pages", len(threads))
	}
	for i, thread := range threads {
		if want := "PRRT_" + strconv.Itoa(i); thread.ID != want {
			t.Errorf("thread %d is %s, want %s", i, thread.ID, want)
		}
	}
}

// maxThreadPages bounds the read: a pull request with more threads than that is
// one where a later round should narrow with a path rather than read
// everything.
func TestReviewThreadsStopsAtThePageLimit(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	gh.Threads = threadFixture(maxThreadPages * 4)
	gh.ThreadPageSize = 1
	client := newTestClient(t, gh)

	threads, err := client.ReviewThreads(context.Background())
	if err != nil {
		t.Fatalf("ReviewThreads returned error: %v", err)
	}
	if len(threads) != maxThreadPages {
		t.Errorf("read %d threads, want the %d pages the client reads", len(threads), maxThreadPages)
	}
}
