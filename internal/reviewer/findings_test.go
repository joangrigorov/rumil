package reviewer

import (
	"testing"

	"github.com/joangrigorov/rumil/internal/testutil"
)

// A submitted review reports the inline comments GitHub accepted, with the
// comment ids later feedback arrives on, so the worker can store them.
func TestReviewReturnsTheSubmittedFindings(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	llm.QueueToolCall("submit_pr_review", map[string]any{
		"event":        "COMMENT",
		"summary_body": "One finding.",
		"comments": []map[string]any{
			{"path": "main.go", "line": 13, "side": "RIGHT", "body": "This line wraps the call without a timeout."},
		},
	})
	llm.QueueText("Review submitted.")

	h := newHarness(t, gh, llm, harnessOpts{})
	result, err := h.review(t)
	if err != nil {
		t.Fatalf("Review returned error: %v\nlogs:\n%s", err, h.logs.String())
	}

	if result.ReviewID == 0 {
		t.Error("the result carries no review id")
	}
	if len(result.Findings) != 1 {
		t.Fatalf("findings = %+v, want the submitted comment", result.Findings)
	}
	finding := result.Findings[0]
	if finding.CommentID == 0 {
		t.Error("the finding carries no comment id")
	}
	if finding.Path != "main.go" || finding.Line != 13 {
		t.Errorf("finding = %+v, want main.go:13", finding)
	}
}
