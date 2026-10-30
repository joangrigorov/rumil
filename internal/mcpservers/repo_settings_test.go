package mcpservers_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/joangrigorov/rumil/internal/ghapp"
	"github.com/joangrigorov/rumil/internal/mcpservers"
	"github.com/joangrigorov/rumil/internal/testutil"
)

// A repository's settings are enforced at submit time: an ignored path never
// reaches GitHub, the inline cap is a cap, and a REQUEST_CHANGES the repository
// does not allow is submitted as COMMENT.
func TestRepositorySettingsNarrowTheSubmittedReview(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	rumil := ghapp.NewClient(fakeClient(t, gh), gh.Owner, gh.Repo, gh.Number)
	tools := mcpservers.NewGithubTools(rumil, mcpservers.GithubToolsOptions{
		MaxDiffBytes:           4096,
		BotHandle:              "rumil",
		Logger:                 memoryLogger(),
		IgnorePath:             func(path string) bool { return path == "main.go" },
		MaxInlineComments:      1,
		DisallowRequestChanges: true,
	})

	out := mustCall(t, tools.Specs(), "submit_pr_review", map[string]any{
		"event":        "REQUEST_CHANGES",
		"summary_body": "Findings.",
		"comments": []any{
			map[string]any{"path": "main.go", "line": 13, "body": "ignored by repository settings"},
			map[string]any{"path": "service.go", "line": 44, "body": "the one kept finding"},
			map[string]any{"path": "service.go", "line": 43, "body": "over the inline cap"},
		},
	})

	reviews := gh.SubmittedReviews()
	if len(reviews) != 1 {
		t.Fatalf("submitted %d reviews, want 1", len(reviews))
	}
	review := reviews[0]
	if review.Event != ghapp.EventComment {
		t.Errorf("event = %q, want COMMENT when the repository disallows requested changes", review.Event)
	}
	if len(review.Comments) != 1 {
		t.Fatalf("submitted %d inline comments, want the ignored path and the cap to leave 1: %+v", len(review.Comments), review.Comments)
	}
	if review.Comments[0].Path != "service.go" || review.Comments[0].Body != "the one kept finding" {
		t.Errorf("kept comment = %+v, want the first service.go finding", review.Comments[0])
	}
	if !strings.Contains(review.Body, "1 further finding(s) were omitted") {
		t.Errorf("summary does not account for the capped finding:\n%s", review.Body)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("decoding the tool result: %v", err)
	}
	if result["ignored_paths"] != float64(1) {
		t.Errorf("ignored_paths = %v, want 1", result["ignored_paths"])
	}
	if result["omitted_comments"] != float64(1) {
		t.Errorf("omitted_comments = %v, want 1", result["omitted_comments"])
	}
	if result["request_changes_downgraded"] != true {
		t.Errorf("request_changes_downgraded = %v, want true", result["request_changes_downgraded"])
	}
}

// Without repository settings the submit path keeps the event the model chose.
func TestSubmitWithoutRepositorySettingsKeepsTheEvent(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	rumil := ghapp.NewClient(fakeClient(t, gh), gh.Owner, gh.Repo, gh.Number)
	tools := mcpservers.NewGithubTools(rumil, mcpservers.GithubToolsOptions{
		MaxDiffBytes: 4096, BotHandle: "rumil", Logger: memoryLogger(),
	})

	mustCall(t, tools.Specs(), "submit_pr_review", map[string]any{
		"event":        "REQUEST_CHANGES",
		"summary_body": "Findings.",
		"comments": []any{
			map[string]any{"path": "service.go", "line": 44, "body": "a finding"},
		},
	})

	reviews := gh.SubmittedReviews()
	if len(reviews) != 1 || reviews[0].Event != ghapp.EventRequestChanges {
		t.Fatalf("submitted %+v, want one REQUEST_CHANGES review", reviews)
	}
	if len(reviews[0].Comments) != 1 {
		t.Errorf("submitted %d comments, want 1", len(reviews[0].Comments))
	}
}
