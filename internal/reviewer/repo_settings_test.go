package reviewer

import (
	"context"
	"strings"
	"testing"

	"github.com/joangrigorov/rumil/internal/repoconfig"
	"github.com/joangrigorov/rumil/internal/testutil"
)

// A repository's .rumil.yaml chooses a configured provider, names a default
// focus, and reaches the prompt as its own block.
func TestRepositorySettingsChooseTheProviderAndFocus(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	gh.Contents[repoconfig.FileName] = `
models:
  default_chat: "chat-b"
review:
  default_focus: ["migrations", "deployment"]
  ignore_paths: ["vendor/**"]
  max_inline_comments: 1
`
	llm := testutil.NewFakeLLM(t)
	llm.QueueToolCall("submit_pr_review", map[string]any{
		"event":        "COMMENT",
		"summary_body": "One finding.",
		"comments": []map[string]any{
			{"path": "service.go", "line": 44, "body": "a finding"},
		},
	})
	llm.QueueText("Review submitted.")

	h := newHarness(t, gh, llm, harnessOpts{})
	res, err := h.review(t)
	if err != nil {
		t.Fatalf("Review returned error: %v\nlogs:\n%s", err, h.logs.String())
	}
	if !res.ReviewSubmitted {
		t.Fatal("ReviewSubmitted = false, want a review")
	}

	if models := h.modelsUsed(t); len(models) == 0 || models[0] != "test-model-b" {
		t.Errorf("models used = %v, want the repository's chat-b provider", models)
	}
	prompt := llm.Requests()[0]
	for _, want := range []string{
		"<repository_review_settings>",
		"Do not report findings on these paths",
		"migrations; deployment",
		"at most 1 inline comments",
	} {
		if !strings.Contains(prompt.SystemPrompt, want) {
			t.Errorf("the system prompt lost %q:\n%s", want, prompt.SystemPrompt)
		}
	}
	if !strings.Contains(prompt.UserPrompt, "<focus>\nmigrations; deployment\n</focus>") {
		t.Errorf("the user prompt does not carry the repository's default focus:\n%s", prompt.UserPrompt)
	}
}

// The settings file is read from the same ref as RUMIL.md: the base branch by
// default, so the pull request cannot change the settings that review it.
func TestRepositorySettingsAreReadFromTheBaseBranch(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ref       string
		wantModel string
	}{
		{name: "base by default", ref: "", wantModel: "test-model-b"},
		{name: "head when the operator opted in", ref: "head", wantModel: "test-model-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh := testutil.NewFakeGitHub(t)
			gh.ContentsByRef = map[string]map[string]string{
				gh.BaseSHA: {repoconfig.FileName: "models:\n  default_chat: \"chat-b\"\n"},
				gh.HeadSHA: {repoconfig.FileName: "models:\n  default_chat: \"chat-a\"\n"},
			}
			llm := testutil.NewFakeLLM(t)
			llm.QueueToolCall("submit_pr_review", map[string]any{
				"event": "COMMENT", "summary_body": "No findings.",
			})
			llm.QueueText("Review submitted.")

			h := newHarness(t, gh, llm, harnessOpts{guidelinesRef: tc.ref})
			if _, err := h.review(t); err != nil {
				t.Fatalf("Review returned error: %v\nlogs:\n%s", err, h.logs.String())
			}
			if models := h.modelsUsed(t); len(models) == 0 || models[0] != tc.wantModel {
				t.Errorf("models used = %v, want %q", models, tc.wantModel)
			}
		})
	}
}

// An invalid settings file fails the review before the first model call,
// because falling back would review against rules the repository replaced.
func TestInvalidRepositorySettingsFailTheReview(t *testing.T) {
	for _, tc := range []struct{ name, doc, want string }{
		{"an unknown key", "review:\n  auto_approve: true\n", "auto_approve"},
		{"an unknown provider", "models:\n  default_chat: \"chat-z\"\n", "chat-z"},
		{"a credential", "models:\n  api_key: \"sk-test\"\n", "not allowed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh := testutil.NewFakeGitHub(t)
			gh.Contents[repoconfig.FileName] = tc.doc
			llm := testutil.NewFakeLLM(t)

			h := newHarness(t, gh, llm, harnessOpts{})
			if _, err := h.review(t); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Review error = %v, want it to contain %q", err, tc.want)
			}
			if calls := llm.Count(); calls != 0 {
				t.Errorf("model requests = %d, want none for an invalid settings file", calls)
			}
		})
	}
}

// The repository's ignore paths and request-changes switch are enforced by the
// tool, so a model that submits anyway still produces the review that was asked
// for.
func TestRepositorySettingsAreEnforcedAtSubmit(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	gh.Contents[repoconfig.FileName] = `
review:
  ignore_paths: ["main.go"]
  request_changes: false
`
	llm := testutil.NewFakeLLM(t)
	llm.QueueToolCall("submit_pr_review", map[string]any{
		"event":        "REQUEST_CHANGES",
		"summary_body": "Two findings.",
		"comments": []map[string]any{
			{"path": "main.go", "line": 13, "body": "ignored"},
			{"path": "service.go", "line": 44, "body": "kept"},
		},
	})
	llm.QueueText("Review submitted.")

	h := newHarness(t, gh, llm, harnessOpts{})
	if _, err := h.review(t); err != nil {
		t.Fatalf("Review returned error: %v\nlogs:\n%s", err, h.logs.String())
	}

	reviews := gh.SubmittedReviews()
	if len(reviews) != 1 {
		t.Fatalf("submitted %d reviews, want 1", len(reviews))
	}
	if reviews[0].Event != "COMMENT" {
		t.Errorf("event = %q, want COMMENT under request_changes: false", reviews[0].Event)
	}
	if len(reviews[0].Comments) != 1 || reviews[0].Comments[0].Path != "service.go" {
		t.Errorf("submitted comments = %+v, want only the service.go finding", reviews[0].Comments)
	}
}

// resolve_threads: false takes the tool away, which is the same shape the
// operator's switch has.
func TestRepositorySettingsCanTurnOffThreadResolution(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	gh.Contents[repoconfig.FileName] = "review:\n  resolve_threads: false\n"
	llm := testutil.NewFakeLLM(t)
	llm.QueueToolCall("submit_pr_review", map[string]any{
		"event": "COMMENT", "summary_body": "No findings.",
	})
	llm.QueueText("Review submitted.")

	h := newHarness(t, gh, llm, harnessOpts{})
	if _, err := h.review(t); err != nil {
		t.Fatalf("Review returned error: %v\nlogs:\n%s", err, h.logs.String())
	}

	for _, name := range llm.OfferedToolNames() {
		if name == "resolve_review_thread" {
			t.Error("the session was offered resolve_review_thread, want the repository's setting respected")
		}
	}
	if prompt := llm.Requests()[0].SystemPrompt; strings.Contains(prompt, "Resolve a thread only when") {
		t.Errorf("the prompt still carries the resolution rule:\n%s", prompt)
	}
}

// The effective settings are emitted for the session page, including when no
// file exists.
func TestRepositorySettingsAreObserved(t *testing.T) {
	for _, tc := range []struct {
		name  string
		doc   string
		found bool
	}{
		{"a file", "review:\n  default_focus: [\"migrations\"]\n", true},
		{"no file", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh := testutil.NewFakeGitHub(t)
			if tc.found {
				gh.Contents[repoconfig.FileName] = tc.doc
			}
			llm := testutil.NewFakeLLM(t)
			llm.QueueToolCall("submit_pr_review", map[string]any{
				"event": "COMMENT", "summary_body": "No findings.",
			})
			llm.QueueText("Review submitted.")

			h := newHarness(t, gh, llm, harnessOpts{})
			observer := &captureObserver{}
			if _, err := h.engine.Review(context.Background(), Request{
				Owner: gh.Owner, Repo: gh.Repo, Number: gh.Number, Observer: observer,
			}); err != nil {
				t.Fatalf("Review returned error: %v\nlogs:\n%s", err, h.logs.String())
			}

			var payload map[string]any
			for _, event := range observer.events {
				if event.Type == "repository_settings" {
					payload = event.Payload
					break
				}
			}
			if payload == nil {
				t.Fatal("no repository_settings event was emitted")
			}
			if payload["found"] != tc.found {
				t.Errorf("found = %v, want %v", payload["found"], tc.found)
			}
			if tc.found && payload["default_focus"] != "migrations" {
				t.Errorf("default_focus = %v, want migrations", payload["default_focus"])
			}
		})
	}
}
