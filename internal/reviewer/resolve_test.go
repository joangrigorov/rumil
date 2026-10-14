package reviewer

import (
	"slices"
	"strings"
	"testing"

	"github.com/joangrigorov/rumil/internal/testutil"
)

// Closing threads is on by default: the reviewer that can see a finding is gone
// should be able to say so.
func TestResolveThreadsAreOnByDefault(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	submitComment(llm)

	h := newHarness(t, gh, llm, harnessOpts{})
	if _, err := h.review(t); err != nil {
		t.Fatalf("Review returned error: %v\nlogs:\n%s", err, h.logs.String())
	}

	if !slices.Contains(llm.OfferedToolNames(), "resolve_review_thread") {
		t.Errorf("the session was not offered resolve_review_thread, tools: %v", llm.OfferedToolNames())
	}
	prompt := systemPrompt(t, llm)
	if !strings.Contains(prompt, "resolve_review_thread") {
		t.Errorf("the prompt does not name the tool the session has:\n%s", prompt)
	}
}

// review.resolve_threads false takes the tool away, and the rule that names it
// goes with it: a rule the model cannot follow is worse than no rule.
func TestResolveThreadsOffRemovesTheToolAndItsRule(t *testing.T) {
	off := false

	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	submitComment(llm)

	h := newHarness(t, gh, llm, harnessOpts{resolveThreads: &off})
	if _, err := h.review(t); err != nil {
		t.Fatalf("Review returned error: %v\nlogs:\n%s", err, h.logs.String())
	}

	if slices.Contains(llm.OfferedToolNames(), "resolve_review_thread") {
		t.Errorf("the session was offered resolve_review_thread while it is off, tools: %v", llm.OfferedToolNames())
	}
	for _, want := range []string{"list_pr_conversation", "submit_pr_review"} {
		if !slices.Contains(llm.OfferedToolNames(), want) {
			t.Errorf("turning resolution off also took %s away, tools: %v", want, llm.OfferedToolNames())
		}
	}
	prompt := systemPrompt(t, llm)
	if strings.Contains(prompt, "resolve_review_thread") {
		t.Errorf("the prompt still names a tool the session does not have:\n%s", prompt)
	}
	if !strings.Contains(prompt, "list_pr_conversation") {
		t.Errorf("the history rule went with the resolution one:\n%s", prompt)
	}
}

// T-020-09 from the reviewer's side: the metadata the session gets names the
// commit the last review was submitted against, which is what makes a delta
// possible at all.
func TestMetadataCarriesTheLastReviewedCommit(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	gh.ExistingReviews = []testutil.ReviewEntry{
		{ID: 1, Author: "rumil[bot]", State: "COMMENTED", Body: "round one", CommitID: "aaaa1111"},
	}
	llm := testutil.NewFakeLLM(t)
	llm.QueueToolCall("get_pr_metadata", nil)
	submitComment(llm)

	h := newHarness(t, gh, llm, harnessOpts{})
	if _, err := h.review(t); err != nil {
		t.Fatalf("Review returned error: %v\nlogs:\n%s", err, h.logs.String())
	}

	results := llm.ToolResults()
	if len(results) == 0 {
		t.Fatal("the session returned no tool results")
	}
	if !strings.Contains(results[0], `"last_reviewed_commit": "aaaa1111"`) {
		t.Errorf("get_pr_metadata returned:\n%s", results[0])
	}
}
