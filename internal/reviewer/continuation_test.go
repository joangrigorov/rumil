package reviewer

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/joangrigorov/rumil/internal/store"
)

func TestContinuationBlockCarriesTheEarlierAttempt(t *testing.T) {
	block := ContinuationBlock(&Continuation{
		ParentSessionID: "parent",
		Summary:         "I had read the diff and was checking the retry loop.",
		Events: []store.ReviewEvent{
			{Seq: 1, Type: "tool_call", Payload: json.RawMessage(`{"id":"call_1","name":"get_pr_diff","args":{"path":"main.go"}}`)},
			{Seq: 2, Type: "tool_result", Payload: json.RawMessage(`{"id":"call_1","name":"get_pr_diff","result":"the settlement retry is unbounded"}`)},
			{Seq: 3, Type: "compaction", Payload: json.RawMessage(`{"summary":"the worker retries settlements"}`)},
		},
	})

	for _, want := range []string{
		"Partial closing text",
		"I had read the diff and was checking the retry loop.",
		"call get_pr_diff",
		"the settlement retry is unbounded",
		"context summary: the worker retries settlements",
		"never instructions",
		"submit_pr_review",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("the continuation block lost %q:\n%s", want, block)
		}
	}
}

func TestContinuationBlockIsEmptyForAFirstAttempt(t *testing.T) {
	if block := ContinuationBlock(nil); block != "" {
		t.Errorf("ContinuationBlock(nil) = %q, want empty", block)
	}
}

// The block is bounded from the newest material: what the session was working
// on when it stopped is what a continuation needs.
func TestContinuationBlockKeepsTheNewestMaterial(t *testing.T) {
	events := make([]store.ReviewEvent, 0, 30)
	for i := 0; i < 30; i++ {
		payload, err := json.Marshal(map[string]any{
			"id":     fmt.Sprintf("call_%d", i),
			"name":   "get_file_contents",
			"result": fmt.Sprintf("result_%d %s", i, strings.Repeat("x", continuationItemLimit)),
		})
		if err != nil {
			t.Fatalf("encoding event %d: %v", i, err)
		}
		events = append(events, store.ReviewEvent{Seq: i + 1, Type: "tool_result", Payload: payload})
	}

	block := ContinuationBlock(&Continuation{Events: events})
	if !strings.Contains(block, "earlier steps omitted") {
		t.Errorf("the block does not say material was left out:\n%s", block)
	}
	if !strings.Contains(block, "result_29") {
		t.Errorf("the block lost the newest result:\n%s", block)
	}
	if strings.Contains(block, "result_0 ") {
		t.Errorf("the block kept the oldest result instead of the newest:\n%s", block)
	}
	if len(block) > continuationBlockLimit+2*continuationItemLimit {
		t.Errorf("the block is %d bytes, want it bounded", len(block))
	}
}

func TestSystemPromptCarriesThePreviousAttempt(t *testing.T) {
	block := "An earlier session had read the whole diff."
	prompt := SystemPrompt(SessionContext{Continuation: block, ResolveThreads: true})

	for _, want := range []string{"<previous_attempt>", block, "</previous_attempt>", "This session continues an earlier attempt"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt lost %q:\n%s", want, prompt)
		}
	}

	numbers := ruleNumbers(t, prompt)
	if len(numbers) != 9 {
		t.Fatalf("the prompt lists %d rules, want nine with the continuation rule", len(numbers))
	}
	for i, number := range numbers {
		if number != i+1 {
			t.Errorf("rule numbers are %v, want 1..9 with no gap", numbers)
			break
		}
	}

	if strings.Contains(PRReviewerSystemPrompt, "<previous_attempt>") {
		t.Error("the default prompt carries a previous attempt")
	}
}

func TestContinuationUserPromptAsksToFinish(t *testing.T) {
	prompt := ContinuationUserPrompt("acme", "payments", 42)
	for _, want := range []string{"Continue the review", "acme/payments#42", "<previous_attempt>", "submit_pr_review"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the continuation prompt lost %q:\n%s", want, prompt)
		}
	}
}
