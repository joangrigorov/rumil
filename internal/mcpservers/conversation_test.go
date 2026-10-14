package mcpservers_test

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/google/go-github/v69/github"

	"github.com/joangrigorov/rumil/internal/ghapp"
	"github.com/joangrigorov/rumil/internal/mcpservers"
	"github.com/joangrigorov/rumil/internal/testutil"
)

// fakeClient is a go-github client pointed at the fake API.
func fakeClient(t *testing.T, gh *testutil.FakeGitHub) *github.Client {
	t.Helper()

	client := github.NewClient(nil)
	base, err := url.Parse(gh.URL() + "/")
	if err != nil {
		t.Fatalf("parsing the fake API URL: %v", err)
	}
	client.BaseURL = base
	return client
}

// newGithubTools scopes the GitHub tool set to the fake API, which is the
// surface the tests drive it through. The review list is read before the tool
// set is built, which is what the engine does: it is the session's baseline.
func newGithubTools(t *testing.T, gh *testutil.FakeGitHub, botHandle string) *mcpservers.GithubTools {
	t.Helper()

	rumil := ghapp.NewClient(fakeClient(t, gh), gh.Owner, gh.Repo, gh.Number)
	reviews, err := rumil.Reviews(context.Background())
	if err != nil {
		t.Fatalf("reading the fake review list: %v", err)
	}
	return mcpservers.NewGithubTools(rumil, 4096, botHandle, memoryLogger(), reviews)
}

type conversationResult struct {
	Reviews []struct {
		ID        int64  `json:"id"`
		Author    string `json:"author"`
		State     string `json:"state"`
		Commit    string `json:"commit"`
		Body      string `json:"body"`
		IsMine    bool   `json:"is_mine"`
		Submitted string `json:"submitted_at"`
	} `json:"reviews"`
	Threads []struct {
		ID         string `json:"id"`
		Path       string `json:"path"`
		Line       int    `json:"line"`
		IsResolved bool   `json:"is_resolved"`
		IsOutdated bool   `json:"is_outdated"`
		Comments   []struct {
			ID        int64  `json:"id"`
			Author    string `json:"author"`
			CreatedAt string `json:"created_at"`
			Body      string `json:"body"`
		} `json:"comments"`
	} `json:"threads"`
	Conversation []struct {
		ID     int64  `json:"id"`
		Author string `json:"author"`
		Body   string `json:"body"`
	} `json:"conversation"`
	ThreadsOmitted int    `json:"threads_omitted"`
	NextBefore     string `json:"next_before"`
}

// historyFixture is a pull request reviewed once, with one open thread, one
// resolved thread, and a conversation comment: the shape T-020-07 describes.
func historyFixture(t *testing.T) *testutil.FakeGitHub {
	t.Helper()
	gh := testutil.NewFakeGitHub(t)
	gh.ExistingReviews = []testutil.ReviewEntry{
		{
			ID: 1, Author: "rumil[bot]", State: "COMMENTED", CommitID: "aaaa1111",
			Body: "Round one: the retry loop is unbounded and the error is swallowed.",
		},
		{ID: 2, Author: "octocat", State: "COMMENTED", CommitID: "bbbb2222", Body: "Addressed."},
	}
	gh.Threads = []testutil.ThreadEntry{
		{
			ID: "PRRT_open", Path: "main.go", Line: 12,
			Comments: []testutil.CommentEntry{
				testutil.ReviewComment(1, 0, "rumil[bot]", "This line wraps the call without a timeout."),
				testutil.ReviewComment(2, 1, "octocat", "we retry around it"),
			},
		},
		{
			ID: "PRRT_fixed", Path: "service.go", Line: 3, IsResolved: true, IsOutdated: true,
			Comments: []testutil.CommentEntry{
				testutil.ReviewComment(3, 0, "rumil[bot]", "The context is never cancelled."),
				testutil.ReviewComment(4, 3, "octocat", "cancelled upstream now"),
			},
		},
	}
	gh.IssueComments = []testutil.CommentEntry{
		testutil.ReviewComment(5, 0, "octocat", "Please take another look at the retry budget."),
	}
	return gh
}

// T-020-07: the history view carries the reviews, both threads with their
// resolution state and replies, and the conversation.
func TestT020_07_ListConversationReportsTheHistory(t *testing.T) {
	gh := historyFixture(t)
	tools := newGithubTools(t, gh, "@rumil")

	out := decode[conversationResult](t, mustCall(t, tools.Specs(), "list_pr_conversation", nil))

	if len(out.Reviews) != 2 {
		t.Fatalf("returned %d reviews, want 2: %+v", len(out.Reviews), out.Reviews)
	}
	if !out.Reviews[0].IsMine || !strings.Contains(out.Reviews[0].Body, "Round one") {
		t.Errorf("Rumil's own summary is not marked as its own: %+v", out.Reviews[0])
	}
	if out.Reviews[0].Commit != "aaaa1111" {
		t.Errorf("review commit = %q, want the one it was submitted against", out.Reviews[0].Commit)
	}
	if out.Reviews[1].IsMine {
		t.Errorf("another author's review is marked as Rumil's: %+v", out.Reviews[1])
	}

	if len(out.Threads) != 2 {
		t.Fatalf("returned %d threads, want 2: %+v", len(out.Threads), out.Threads)
	}
	byID := map[string]int{}
	for i, thread := range out.Threads {
		byID[thread.ID] = i
	}
	open := out.Threads[byID["PRRT_open"]]
	if open.IsResolved || open.IsOutdated {
		t.Errorf("the open thread reports %+v, want neither flag", open)
	}
	if len(open.Comments) != 2 || open.Comments[1].Author != "octocat" {
		t.Errorf("the replies did not come back: %+v", open.Comments)
	}
	if open.Comments[0].CreatedAt == "" {
		t.Error("a comment carries no timestamp")
	}
	fixed := out.Threads[byID["PRRT_fixed"]]
	if !fixed.IsResolved || !fixed.IsOutdated {
		t.Errorf("the fixed thread reports %+v, want both flags", fixed)
	}

	if len(out.Conversation) != 1 || !strings.Contains(out.Conversation[0].Body, "retry budget") {
		t.Errorf("the conversation did not come back: %+v", out.Conversation)
	}
}

// The history is big enough to page: newest first, and the response names the
// thread to ask after.
func TestListConversationPagesThreadsNewestFirst(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	gh.Threads = []testutil.ThreadEntry{
		{ID: "PRRT_old", Path: "main.go", Line: 1, Comments: []testutil.CommentEntry{
			testutil.ReviewComment(1, 0, "rumil[bot]", "the oldest finding"),
		}},
		{ID: "PRRT_middle", Path: "main.go", Line: 2, Comments: []testutil.CommentEntry{
			testutil.ReviewComment(5, 0, "rumil[bot]", "a middle finding"),
		}},
		{ID: "PRRT_new", Path: "main.go", Line: 3, Comments: []testutil.CommentEntry{
			testutil.ReviewComment(9, 0, "rumil[bot]", "the newest finding"),
		}},
	}
	tools := newGithubTools(t, gh, "@rumil")

	page := decode[conversationResult](t, mustCall(t, tools.Specs(), "list_pr_conversation", map[string]any{"limit": 2}))
	if len(page.Threads) != 2 {
		t.Fatalf("returned %d threads, want 2", len(page.Threads))
	}
	if page.Threads[0].ID != "PRRT_new" || page.Threads[1].ID != "PRRT_middle" {
		t.Errorf("the page is %s, %s; want newest first", page.Threads[0].ID, page.Threads[1].ID)
	}
	if page.ThreadsOmitted != 1 {
		t.Errorf("threads_omitted = %d, want 1", page.ThreadsOmitted)
	}
	if page.NextBefore != "PRRT_middle" {
		t.Errorf("next_before = %q, want the last thread of the page", page.NextBefore)
	}

	rest := decode[conversationResult](t, mustCall(t, tools.Specs(), "list_pr_conversation", map[string]any{
		"limit": 2, "before": page.NextBefore,
	}))
	if len(rest.Threads) != 1 || rest.Threads[0].ID != "PRRT_old" {
		t.Errorf("the second page is %+v, want the oldest thread", rest.Threads)
	}
	if rest.ThreadsOmitted != 0 || rest.NextBefore != "" {
		t.Errorf("the last page reports omission %d / cursor %q, want neither", rest.ThreadsOmitted, rest.NextBefore)
	}
}

func TestListConversationFiltersByPathAndResolution(t *testing.T) {
	gh := historyFixture(t)
	tools := newGithubTools(t, gh, "@rumil")

	byPath := decode[conversationResult](t, mustCall(t, tools.Specs(), "list_pr_conversation", map[string]any{"path": "service.go"}))
	if len(byPath.Threads) != 1 || byPath.Threads[0].ID != "PRRT_fixed" {
		t.Errorf("the path filter returned %+v, want only the thread on that file", byPath.Threads)
	}

	unresolved := decode[conversationResult](t, mustCall(t, tools.Specs(), "list_pr_conversation", map[string]any{"include_resolved": false}))
	if len(unresolved.Threads) != 1 || unresolved.Threads[0].ID != "PRRT_open" {
		t.Errorf("include_resolved false returned %+v, want only the open thread", unresolved.Threads)
	}
}

// T-020-08: a fix lets Rumil close its own thread, and the reason lands in the
// thread so the resolution is not silent.
func TestT020_08_ResolveReviewThreadClosesRumilsOwnThread(t *testing.T) {
	gh := historyFixture(t)
	tools := newGithubTools(t, gh, "@rumil")

	out := decode[map[string]any](t, mustCall(t, tools.Specs(), "resolve_review_thread", map[string]any{
		"thread_id": "PRRT_open",
		"reason":    "the retry loop now bounds the wait, so the missing timeout is gone",
	}))

	if resolved, _ := out["resolved"].(bool); !resolved {
		t.Errorf("resolve_review_thread returned %+v, want the thread closed", out)
	}
	if resolved := gh.Resolved(); len(resolved) != 1 || resolved[0] != "PRRT_open" {
		t.Errorf("resolved %v, want PRRT_open", resolved)
	}

	// The reason is posted into the thread, which is the comment that opened it.
	posted := gh.PostedComments()
	if len(posted) != 1 {
		t.Fatalf("posted %d comments, want the reason: %+v", len(posted), posted)
	}
	if posted[0].InReplyTo != 1 || !strings.Contains(posted[0].Body, "retry loop now bounds") {
		t.Errorf("the reason landed as %+v, want a reply on the thread's root", posted[0])
	}
	if cap, _ := out["cap"].(float64); cap != 20 {
		t.Errorf("the result reports cap %v, want 20", out["cap"])
	}
}

// T-020-08 (second half): another author's thread is not Rumil's to close.
func TestT020_08_ResolveReviewThreadRefusesAnotherAuthorsThread(t *testing.T) {
	gh := historyFixture(t)
	gh.Threads = append(gh.Threads, testutil.ThreadEntry{
		ID: "PRRT_theirs", Path: "main.go", Line: 40,
		Comments: []testutil.CommentEntry{
			testutil.ReviewComment(7, 0, "octocat", "should this be a batch call?"),
		},
	})
	tools := newGithubTools(t, gh, "@rumil")

	_, err := callTool(t, tools.Specs(), "resolve_review_thread", map[string]any{
		"thread_id": "PRRT_theirs",
		"reason":    "not my thread",
	})
	if err == nil || !strings.Contains(err.Error(), "only Rumil's own threads") {
		t.Errorf("error = %v, want a refusal naming the thread's author", err)
	}
	if resolved := gh.Resolved(); len(resolved) != 0 {
		t.Errorf("resolved %v, want no mutation", resolved)
	}
	if posted := gh.PostedComments(); len(posted) != 0 {
		t.Errorf("posted %+v, want nothing said about a refusal", posted)
	}
}

// A first pass leaves its findings open: closing threads is what a second pass
// does once it can see whether the fix landed.
func TestResolveReviewThreadIsRefusedOnTheFirstReview(t *testing.T) {
	gh := historyFixture(t)
	gh.ExistingReviews = nil
	tools := newGithubTools(t, gh, "@rumil")

	_, err := callTool(t, tools.Specs(), "resolve_review_thread", map[string]any{
		"thread_id": "PRRT_open",
		"reason":    "fixed",
	})
	if err == nil || !strings.Contains(err.Error(), "second review") {
		t.Errorf("error = %v, want a refusal explaining the first review keeps its findings open", err)
	}
	if resolved := gh.Resolved(); len(resolved) != 0 {
		t.Errorf("resolved %v, want no mutation", resolved)
	}
}

func TestResolveReviewThreadStopsAtTheCap(t *testing.T) {
	gh := historyFixture(t)
	gh.Threads = nil
	for i := 0; i < 21; i++ {
		id := string(rune('a' + i))
		gh.Threads = append(gh.Threads, testutil.ThreadEntry{
			ID: "PRRT_" + id, Path: "main.go", Line: i + 1,
			Comments: []testutil.CommentEntry{
				testutil.ReviewComment(int64(100+i), 0, "rumil[bot]", "finding "+id),
			},
		})
	}
	tools := newGithubTools(t, gh, "@rumil")

	for i := 0; i < 20; i++ {
		id := string(rune('a' + i))
		if _, err := callTool(t, tools.Specs(), "resolve_review_thread", map[string]any{
			"thread_id": "PRRT_" + id, "reason": "fixed",
		}); err != nil {
			t.Fatalf("resolution %d returned error: %v", i+1, err)
		}
	}
	_, err := callTool(t, tools.Specs(), "resolve_review_thread", map[string]any{
		"thread_id": "PRRT_u", "reason": "fixed",
	})
	if err == nil || !strings.Contains(err.Error(), "cap") {
		t.Errorf("error = %v, want the twentieth resolution to be the last", err)
	}
	if resolved := gh.Resolved(); len(resolved) != 20 {
		t.Errorf("resolved %d threads, want 20", len(resolved))
	}
}

// T-020-09: the metadata names the commit the last review was submitted
// against, so a later round can ask for the delta instead of the whole diff.
func TestT020_09_MetadataNamesTheLastReviewedCommit(t *testing.T) {
	gh := historyFixture(t)
	gh.ExistingReviews = append(gh.ExistingReviews, testutil.ReviewEntry{
		ID: 3, Author: "rumil[bot]", State: "COMMENTED", CommitID: "cccc3333",
		Body: "Round two: the retry budget is still unbounded.",
	})
	tools := newGithubTools(t, gh, "@rumil")

	out := decode[map[string]any](t, mustCall(t, tools.Specs(), "get_pr_metadata", nil))
	if got, _ := out["last_reviewed_commit"].(string); got != "cccc3333" {
		t.Errorf("last_reviewed_commit = %q, want the newest review by Rumil", got)
	}
}

func TestT020_09_DiffSinceReadsOnlyTheNewCommits(t *testing.T) {
	gh := historyFixture(t)
	gh.SinceDiff = strings.Join([]string{
		"diff --git a/main.go b/main.go",
		"index 2222222..3333333 100644",
		"--- a/main.go",
		"+++ b/main.go",
		"@@ -10,4 +10,6 @@ func main() {",
		" \tif err != nil {",
		" \t\tlog.Fatal(err)",
		" \t}",
		"+	// retry the settlement call",
		"+	settle(ctx)",
		" \tdefer conn.Close()",
		"",
	}, "\n")
	tools := newGithubTools(t, gh, "@rumil")

	out := mustCall(t, tools.Specs(), "get_pr_diff", map[string]any{"since": "bbbb2222cccc3333"})
	if !strings.Contains(out, "diff of the commits after bbbb222") {
		t.Errorf("the header does not name the delta:\n%s", out)
	}
	if !strings.Contains(out, "settle(ctx)") {
		t.Errorf("the delta diff is missing its change:\n%s", out)
	}
	if !strings.Contains(strings.Join(gh.RequestLog(), "\n"), "/compare/") {
		t.Errorf("the delta did not go through the compare endpoint:\n%s", strings.Join(gh.RequestLog(), "\n"))
	}
}

func TestDiffSinceExplainsAnUnchangedDelta(t *testing.T) {
	gh := historyFixture(t)
	gh.SinceDiff = ""
	tools := newGithubTools(t, gh, "@rumil")

	// An empty comparison is not an error: nothing was pushed, and the model
	// should read the current code rather than re-read a diff it already saw.
	for _, args := range []map[string]any{
		{"since": "bbbb2222cccc3333"},
		{"since": "bbbb2222cccc3333", "file_path": "main.go"},
	} {
		out := mustCall(t, tools.Specs(), "get_pr_diff", args)
		if !strings.Contains(out, "nothing changed after that commit") {
			t.Errorf("an empty delta reads as:\n%s", out)
		}
	}
}

func TestDiffSinceExplainsAFileOutsideTheDelta(t *testing.T) {
	gh := historyFixture(t)
	gh.SinceDiff = strings.Join([]string{
		"diff --git a/service.go b/service.go",
		"index 2222222..3333333 100644",
		"--- a/service.go",
		"+++ b/service.go",
		"@@ -1,3 +1,4 @@ package main",
		" ",
		"+// settle retries in the caller.",
		" func settle(ctx context.Context) error {",
		"",
	}, "\n")
	tools := newGithubTools(t, gh, "@rumil")

	out := mustCall(t, tools.Specs(), "get_pr_diff", map[string]any{"since": "bbbb2222cccc3333", "file_path": "main.go"})
	if !strings.Contains(out, "main.go has no changes after bbbb222") {
		t.Errorf("an unchanged file reads as:\n%s", out)
	}

	out = mustCall(t, tools.Specs(), "get_pr_diff", map[string]any{"since": "bbbb2222cccc3333", "file_path": "service.go"})
	if !strings.Contains(out, "settle retries in the caller") {
		t.Errorf("the changed file did not come back:\n%s", out)
	}
}

func TestResolveReviewThreadNeedsItsArguments(t *testing.T) {
	gh := historyFixture(t)
	tools := newGithubTools(t, gh, "@rumil")

	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"no thread", map[string]any{"reason": "fixed"}, "thread_id is required"},
		{"no reason", map[string]any{"thread_id": "PRRT_open"}, "reason is required"},
		{"unknown thread", map[string]any{"thread_id": "PRRT_nope", "reason": "fixed"}, "no review thread"},
	} {
		_, err := callTool(t, tools.Specs(), "resolve_review_thread", tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s returned %v, want an error containing %q", tc.name, err, tc.want)
		}
	}
}

func TestListConversationClampsTheLimit(t *testing.T) {
	gh := historyFixture(t)
	tools := newGithubTools(t, gh, "@rumil")

	out := mustCall(t, tools.Specs(), "list_pr_conversation", map[string]any{"limit": json.Number("5000")})
	decoded := decode[conversationResult](t, out)
	if len(decoded.Threads) != 2 {
		t.Errorf("returned %d threads, want both of the fixture's", len(decoded.Threads))
	}

	// A limit below the minimum falls back to the default rather than to one.
	out = mustCall(t, tools.Specs(), "list_pr_conversation", map[string]any{"limit": 0})
	if len(decode[conversationResult](t, out).Threads) != 2 {
		t.Error("a zero limit did not fall back to the default")
	}
}

// The review list is read once per session and cached, so a model that reads
// the history twice sees one history.
func TestListConversationIsStableAcrossCalls(t *testing.T) {
	gh := historyFixture(t)
	tools := newGithubTools(t, gh, "@rumil")

	first := mustCall(t, tools.Specs(), "list_pr_conversation", nil)
	second := mustCall(t, tools.Specs(), "list_pr_conversation", nil)
	if first != second {
		t.Errorf("two identical calls returned different views:\n%s\n%s", first, second)
	}
}
