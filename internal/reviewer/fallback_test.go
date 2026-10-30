package reviewer

import (
	"context"
	"strings"
	"testing"

	"github.com/joangrigorov/rumil/internal/testutil"
)

// providerUnavailable queues enough failures for kit's retry policy to exhaust
// the primary attempt.
func queueProviderFailures(llm *testutil.FakeLLM, attempts int) {
	for i := 0; i < attempts; i++ {
		llm.QueueFailure("status 503: the provider is unavailable")
	}
}

func queueSubmit(llm *testutil.FakeLLM) {
	llm.QueueToolCall("submit_pr_review", map[string]any{
		"event":        "COMMENT",
		"summary_body": "No findings.",
	})
	llm.QueueText("Review submitted.")
}

func fallbackEvent(observer *captureObserver) map[string]any {
	for _, event := range observer.events {
		if event.Type == "provider_fallback" {
			return event.Payload
		}
	}
	return nil
}

// A primary provider that exhausts its attempts is followed by the configured
// fallback, which finishes the review on the same commit.
func TestFallbackRunsAfterPrimaryProviderFails(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	primary := testutil.NewFakeLLM(t)
	secondary := testutil.NewFakeLLM(t)
	queueProviderFailures(primary, 4)
	queueSubmit(secondary)

	h := newHarness(t, gh, primary, harnessOpts{
		fallback: []string{"chat-b"},
		chatBURL: secondary.URL(),
	})
	observer := &captureObserver{}
	result, err := h.engine.Review(context.Background(), Request{
		Owner: gh.Owner, Repo: gh.Repo, Number: gh.Number,
		SessionID: "session-1", Observer: observer,
	})
	if err != nil {
		t.Fatalf("Review returned error: %v\nlogs:\n%s", err, h.logs.String())
	}
	if !result.ReviewSubmitted {
		t.Fatal("ReviewSubmitted = false, want the fallback to submit the review")
	}
	if result.ProviderID != "chat-b" {
		t.Errorf("ProviderID = %q, want chat-b", result.ProviderID)
	}
	if calls := primary.Count(); calls == 0 {
		t.Error("the primary provider received no request")
	}
	if calls := secondary.Count(); calls == 0 {
		t.Error("the fallback provider received no request")
	}
	if reviews := gh.SubmittedReviews(); len(reviews) != 1 {
		t.Errorf("submitted %d reviews, want 1", len(reviews))
	}

	payload := fallbackEvent(observer)
	if payload == nil {
		t.Fatalf("no provider_fallback event; events: %v", observer.events)
	}
	if payload["from"] != "chat-a" || payload["to"] != "chat-b" {
		t.Errorf("fallback event = %v, want chat-a to chat-b", payload)
	}
}

// A provider that submits a review is never followed by a fallback request.
func TestFallbackDoesNotRunAfterASubmittedReview(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	primary := testutil.NewFakeLLM(t)
	secondary := testutil.NewFakeLLM(t)
	queueSubmit(primary)

	h := newHarness(t, gh, primary, harnessOpts{
		fallback: []string{"chat-b"},
		chatBURL: secondary.URL(),
	})
	observer := &captureObserver{}
	result, err := h.engine.Review(context.Background(), Request{
		Owner: gh.Owner, Repo: gh.Repo, Number: gh.Number, Observer: observer,
	})
	if err != nil {
		t.Fatalf("Review returned error: %v\nlogs:\n%s", err, h.logs.String())
	}
	if result.ProviderID != "chat-a" {
		t.Errorf("ProviderID = %q, want the primary", result.ProviderID)
	}
	if calls := secondary.Count(); calls != 0 {
		t.Errorf("fallback requests = %d, want none after a submitted review", calls)
	}
	if payload := fallbackEvent(observer); payload != nil {
		t.Errorf("provider_fallback event = %v, want none", payload)
	}
}

// When every configured provider fails, the review ends with the provider
// error rather than a review that did not happen.
func TestFallbackChainEndsWhenEveryProviderFails(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	primary := testutil.NewFakeLLM(t)
	secondary := testutil.NewFakeLLM(t)
	queueProviderFailures(primary, 4)
	queueProviderFailures(secondary, 4)

	h := newHarness(t, gh, primary, harnessOpts{
		fallback: []string{"chat-b"},
		chatBURL: secondary.URL(),
	})
	result, err := h.engine.Review(context.Background(), Request{
		Owner: gh.Owner, Repo: gh.Repo, Number: gh.Number,
	})
	if err == nil {
		t.Fatal("Review returned no error, want the provider failure")
	}
	if result == nil || result.ReviewSubmitted {
		t.Fatalf("result = %+v, want an unattempted review", result)
	}
	if result.ProviderID != "chat-b" {
		t.Errorf("ProviderID = %q, want the last attempt", result.ProviderID)
	}
	if calls := secondary.Count(); calls == 0 {
		t.Error("the fallback provider received no request")
	}
	if reviews := gh.SubmittedReviews(); len(reviews) != 0 {
		t.Errorf("submitted %d reviews, want none", len(reviews))
	}
}

// The fallback inherits the tool work the failed attempt did, so it finishes
// the review instead of reading the pull request again.
func TestFallbackCarriesTheEarlierAttemptsTranscript(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	primary := testutil.NewFakeLLM(t)
	secondary := testutil.NewFakeLLM(t)
	primary.QueueToolCall("get_pr_metadata", nil)
	queueProviderFailures(primary, 4)
	queueSubmit(secondary)

	h := newHarness(t, gh, primary, harnessOpts{
		fallback: []string{"chat-b"},
		chatBURL: secondary.URL(),
	})
	if _, err := h.engine.Review(context.Background(), Request{
		Owner: gh.Owner, Repo: gh.Repo, Number: gh.Number,
	}); err != nil {
		t.Fatalf("Review returned error: %v\nlogs:\n%s", err, h.logs.String())
	}

	requests := secondary.Requests()
	if len(requests) == 0 {
		t.Fatal("the fallback provider received no request")
	}
	prompt := requests[0].SystemPrompt
	for _, want := range []string{"<previous_attempt>", "get_pr_metadata", "provider failed"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the fallback prompt lost %q:\n%s", want, prompt)
		}
	}
}

// The continuation block names the provider failure rather than a turn budget.
func TestProviderContinuationNamesTheCause(t *testing.T) {
	block := ContinuationBlock(&Continuation{Reason: ContinuationProvider})
	if !strings.Contains(block, "provider failed") {
		t.Errorf("the block does not name the provider failure:\n%s", block)
	}
	if strings.Contains(block, "agent-turn budget") {
		t.Errorf("the block claims a turn-limit stop:\n%s", block)
	}
}
