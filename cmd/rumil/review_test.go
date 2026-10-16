package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joangrigorov/rumil/internal/ghapp"
	"github.com/joangrigorov/rumil/internal/reviewer"
	"github.com/joangrigorov/rumil/internal/testutil"
)

// writeConfig renders a rumil.yaml pointing at the given endpoints and returns
// its path.
func writeConfig(t *testing.T, llmURL string) string {
	t.Helper()
	return writeConfigFor(t, llmURL, "")
}

// writeConfigFor renders a configuration with app.bot_handle set. An empty
// handle leaves the key out, which is the default the runtime applies.
func writeConfigFor(t *testing.T, llmURL, handle string) string {
	t.Helper()

	keyPath := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(keyPath, testutil.TestPrivateKeyPEM(t), 0o600); err != nil {
		t.Fatalf("writing test private key: %v", err)
	}

	botHandle := ""
	if handle != "" {
		botHandle = fmt.Sprintf("  bot_handle: %q\n", handle)
	}

	doc := fmt.Sprintf(`
app:
%s  max_agent_turns: 5
github:
  app_id: 123456
  installation_id: 98765432
  private_key_path: %q
models:
  providers:
    - id: "chat"
      provider: "openai-compatible"
      base_url: %q
      model_name: "test-model"
      api_key: "test-key"
`, botHandle, keyPath, llmURL+"/v1")

	configPath := filepath.Join(t.TempDir(), "rumil.yaml")
	if err := os.WriteFile(configPath, []byte(doc), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	return configPath
}

func runReviewCmd(t *testing.T, configPath string) (string, error) {
	t.Helper()

	root := newRootCmd()
	out := &strings.Builder{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"review", "--config", configPath, "--repo", "acme/payments", "--pr", "42"})

	err := root.Execute()
	return out.String(), err
}

// T-001-01 (CLI half): a successful review exits zero.
func TestT001_01_ReviewCommandExitsZeroOnSuccess(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	llm.QueueToolCall("get_pr_metadata", nil)
	llm.QueueToolCall("get_pr_diff", nil)
	llm.QueueToolCall("submit_pr_review", map[string]any{
		"event":        "COMMENT",
		"summary_body": "Looks fine.",
	})
	llm.QueueText("Review submitted.")

	githubAPIBaseURL = gh.URL()
	t.Cleanup(func() { githubAPIBaseURL = "" })

	out, err := runReviewCmd(t, writeConfig(t, llm.URL()))
	if err != nil {
		t.Fatalf("review command returned error: %v (output: %s)", err, out)
	}
	if !strings.Contains(out, "review submitted to acme/payments#42 as COMMENT") {
		t.Errorf("output = %q, want the confirmation line", out)
	}
	if len(gh.SubmittedReviews()) != 1 {
		t.Errorf("submitted %d reviews, want 1", len(gh.SubmittedReviews()))
	}
}

// T-001-06 (CLI half): a missing GitHub App installation exits non-zero.
func TestT001_06_ReviewCommandExitsNonZeroWhenNotInstalled(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	gh.MintStatus = 404
	llm := testutil.NewFakeLLM(t)

	githubAPIBaseURL = gh.URL()
	t.Cleanup(func() { githubAPIBaseURL = "" })

	out, err := runReviewCmd(t, writeConfig(t, llm.URL()))
	if err == nil {
		t.Fatalf("review command succeeded, want a non-zero exit (output: %s)", out)
	}
	if !errors.Is(err, ghapp.ErrAppNotInstalled) {
		t.Errorf("error = %v, want it to wrap ghapp.ErrAppNotInstalled", err)
	}
	if llm.Count() != 0 {
		t.Errorf("model requests = %d, want none before the installation is verified", llm.Count())
	}
}

// T-001-07 (CLI half): a session that never submits exits zero with a warning,
// so a stuck model cannot fail the container.
func TestT001_07_ReviewCommandExitsZeroAtTurnLimit(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	for i := 0; i < 20; i++ {
		llm.QueueToolCall("get_pr_metadata", nil)
	}

	githubAPIBaseURL = gh.URL()
	t.Cleanup(func() { githubAPIBaseURL = "" })

	out, err := runReviewCmd(t, writeConfig(t, llm.URL()))
	if err != nil {
		t.Fatalf("review command returned error: %v", err)
	}
	if !strings.Contains(out, "no review submitted for acme/payments#42") {
		t.Errorf("output = %q, want the clean-stop message", out)
	}
}

// A handle that is not the App's slug costs a run its own review history and its
// ability to close its own threads, so the run stops before it spends a model
// call on a review it cannot finish.
func TestReviewCommandStopsWhenTheHandleIsNotTheAppsSlug(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	gh.AppSlug = "rumil-testing"
	llm := testutil.NewFakeLLM(t)

	githubAPIBaseURL = gh.URL()
	t.Cleanup(func() { githubAPIBaseURL = "" })

	out, err := runReviewCmd(t, writeConfigFor(t, llm.URL(), "testing-rumil"))
	if err == nil {
		t.Fatalf("review command succeeded, want a non-zero exit (output: %s)", out)
	}
	if !errors.Is(err, reviewer.ErrHandleMismatch) {
		t.Errorf("error = %v, want it to wrap reviewer.ErrHandleMismatch", err)
	}
	if !strings.Contains(err.Error(), `"rumil-testing"`) {
		t.Errorf("error = %v, want it to name the App's slug", err)
	}
	if llm.Count() != 0 {
		t.Errorf("model requests = %d, want none before the handle is checked", llm.Count())
	}
}

// A record that could not be read is a GitHub failure rather than a
// configuration one, so the run continues and the warning carries the detail.
func TestReviewCommandContinuesWhenTheAppRecordCannotBeRead(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	gh.AppStatus = 502
	llm := testutil.NewFakeLLM(t)
	llm.QueueToolCall("get_pr_metadata", nil)
	llm.QueueToolCall("get_pr_diff", nil)
	llm.QueueToolCall("submit_pr_review", map[string]any{
		"event":        "COMMENT",
		"summary_body": "Looks fine.",
	})
	llm.QueueText("Review submitted.")

	githubAPIBaseURL = gh.URL()
	t.Cleanup(func() { githubAPIBaseURL = "" })

	out, err := runReviewCmd(t, writeConfig(t, llm.URL()))
	if err != nil {
		t.Fatalf("review command returned error: %v (output: %s)", err, out)
	}
	if len(gh.SubmittedReviews()) != 1 {
		t.Errorf("submitted %d reviews, want the review to run anyway", len(gh.SubmittedReviews()))
	}
}

func TestReviewCommandRejectsBadFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "repo without owner", args: []string{"review", "--repo", "payments", "--pr", "1"}, want: "owner/name"},
		{name: "zero pr number", args: []string{"review", "--repo", "acme/payments", "--pr", "0"}, want: "--pr"},
		{name: "missing repo", args: []string{"review", "--pr", "1"}, want: "repo"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := newRootCmd()
			root.SetOut(&strings.Builder{})
			root.SetErr(&strings.Builder{})
			root.SetArgs(tt.args)

			err := root.Execute()
			if err == nil {
				t.Fatalf("command succeeded, want an error mentioning %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}
