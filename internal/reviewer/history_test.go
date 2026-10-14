package reviewer

import (
	"strings"
	"testing"
	"time"

	"github.com/joangrigorov/rumil/internal/ghapp"
	"github.com/joangrigorov/rumil/internal/testutil"
)

// historyTime is the timestamp the rendered threads carry. The block lists
// reviews in the order the api returned them, so the timestamps only matter to
// the code that decides which comment opened a thread.
var historyTime = time.Date(2026, 1, 2, 15, 0, 0, 0, time.UTC)

func review(id int64, author, state, commit, body string) ghapp.Review {
	return ghapp.Review{ID: id, Author: author, State: state, CommitID: commit, Body: body}
}

func thread(id, path string, line int, resolved bool, comments ...ghapp.ThreadComment) ghapp.ReviewThread {
	return ghapp.ReviewThread{ID: id, Path: path, Line: line, IsResolved: resolved, Comments: comments}
}

func threadComment(author, body string) ghapp.ThreadComment {
	return ghapp.ThreadComment{Author: author, Body: body, CreatedAt: historyTime}
}

// T-020-07: the block carries the earlier rounds' summaries and the threads
// they left open, and leaves the resolved ones to the history tool.
func TestRenderHistoryCarriesTheSummariesAndTheOpenThreads(t *testing.T) {
	got := renderHistory(
		[]ghapp.Review{
			review(1, "rumil[bot]", "CHANGES_REQUESTED", "aaaa1111", "Round one: the retry loop is unbounded."),
			review(2, "octocat", "COMMENTED", "bbbb2222", "Addressed."),
		},
		[]ghapp.ReviewThread{
			thread("PRRT_open", "main.go", 12, false, threadComment("rumil[bot]", "This line wraps the call without a timeout.")),
			thread("PRRT_fixed", "service.go", 3, true, threadComment("rumil[bot]", "The context is never cancelled.")),
		},
		"@rumil",
	)

	for _, want := range []string{
		"<prior_reviews>",
		"Round one: the retry loop is unbounded.",
		"rumil[bot] (you) (CHANGES_REQUESTED, commit aaaa111)",
		"octocat (COMMENTED, commit bbbb222)",
		"PRRT_open on main.go:12",
		"This line wraps the call without a timeout.",
		"left out; list_pr_conversation returns them",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the history block lost %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "The context is never cancelled.") {
		t.Errorf("a resolved thread's text is in the block:\n%s", got)
	}
	if !strings.HasSuffix(got, "</prior_reviews>") {
		t.Errorf("the block does not close:\n%s", got)
	}
}

// A pull request nobody has reviewed yet carries no block, and a block that
// would be enormous is cut rather than pasted in whole.
func TestRenderHistoryBoundsItself(t *testing.T) {
	if got := renderHistory(nil, nil, "@rumil"); got != "" {
		t.Errorf("a first review carries a block: %q", got)
	}

	reviews := make([]ghapp.Review, 0, maxHistoryReviews+5)
	for i := 0; i < maxHistoryReviews+5; i++ {
		reviews = append(reviews, review(int64(i), "octocat", "COMMENTED", "aaaa1111", strings.Repeat("a", historyLineCap*2)))
	}
	threads := make([]ghapp.ReviewThread, 0, maxHistoryThreads+5)
	for i := 0; i < maxHistoryThreads+5; i++ {
		threads = append(threads, thread("PRRT_"+strings.Repeat("b", i+1), "main.go", i, false,
			threadComment("rumil[bot]", strings.Repeat("c", historyLineCap*2))))
	}

	got := renderHistory(reviews, threads, "@rumil")
	if len(got) > historyBlockCap+2*historyLineCap {
		t.Errorf("the block is %d bytes, want it bounded", len(got))
	}
	if !strings.Contains(got, "and more") {
		t.Errorf("the block does not say it left something out:\n%s", got)
	}
}

// T-020-07 (second half): the open thread and the earlier summary are in front
// of the model before it decides anything, so a later round opens knowing what
// is already open.
func TestT020_07_SystemPromptCarriesTheOpenThreadAndTheSummary(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	gh.ExistingReviews = []testutil.ReviewEntry{
		{ID: 1, Author: "rumil[bot]", State: "CHANGES_REQUESTED", CommitID: "aaaa1111", Body: "Round one: the retry loop is unbounded."},
	}
	gh.Threads = []testutil.ThreadEntry{
		{
			ID: "PRRT_open", Path: "main.go", Line: 12,
			Comments: []testutil.CommentEntry{
				testutil.ReviewComment(1, 0, "rumil[bot]", "This line wraps the call without a timeout."),
			},
		},
		{
			ID: "PRRT_fixed", Path: "service.go", Line: 3, IsResolved: true,
			Comments: []testutil.CommentEntry{
				testutil.ReviewComment(3, 0, "rumil[bot]", "The context is never cancelled."),
			},
		},
	}
	llm := testutil.NewFakeLLM(t)
	submitComment(llm)

	h := newHarness(t, gh, llm, harnessOpts{})
	if _, err := h.review(t); err != nil {
		t.Fatalf("Review returned error: %v\nlogs:\n%s", err, h.logs.String())
	}

	prompt := systemPrompt(t, llm)
	for _, want := range []string{
		"<prior_reviews>",
		"Round one: the retry loop is unbounded.",
		"PRRT_open on main.go:12",
		"This line wraps the call without a timeout.",
		"1 resolved thread is left out",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the system prompt lost %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "The context is never cancelled.") {
		t.Errorf("a resolved thread reached the prompt as if it were open:\n%s", prompt)
	}
}

// A pull request no earlier round reviewed is reviewed on the default prompt:
// no empty block, and nothing to say about one.
func TestFirstReviewCarriesNoHistoryBlock(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	submitComment(llm)

	h := newHarness(t, gh, llm, harnessOpts{})
	if _, err := h.review(t); err != nil {
		t.Fatalf("Review returned error: %v\nlogs:\n%s", err, h.logs.String())
	}
	if strings.Contains(systemPrompt(t, llm), "<prior_reviews>\n") {
		t.Errorf("a first review carries a history block:\n%s", systemPrompt(t, llm))
	}
}

// A history that cannot be read costs the review its context, not its run.
func TestReviewSurvivesAnUnreadableHistory(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	gh.GraphQLErrors = []string{"something went wrong"}
	gh.ExistingReviews = []testutil.ReviewEntry{
		{ID: 1, Author: "rumil[bot]", State: "COMMENTED", CommitID: "aaaa1111", Body: "Round one."},
	}
	llm := testutil.NewFakeLLM(t)
	submitComment(llm)

	h := newHarness(t, gh, llm, harnessOpts{})
	res, err := h.review(t)
	if err != nil {
		t.Fatalf("Review returned error: %v\nlogs:\n%s", err, h.logs.String())
	}
	if !res.ReviewSubmitted {
		t.Error("ReviewSubmitted = false, want the review to run without its history")
	}
	prompt := systemPrompt(t, llm)
	// The reviews read fine, so what the round said is still there; only the
	// threads are missing, and the block says so by not listing any.
	if !strings.Contains(prompt, "Round one.") {
		t.Errorf("the reviews read fine, but the summary is missing:\n%s", prompt)
	}
	if strings.Contains(prompt, "PRRT_") || strings.Contains(prompt, "on main.go") {
		t.Errorf("the threads failed to read, but they are in the prompt:\n%s", prompt)
	}
	if !strings.Contains(h.logs.String(), "review threads for the prompt failed") {
		t.Errorf("logs do not record the failed thread read:\n%s", h.logs.String())
	}
}
