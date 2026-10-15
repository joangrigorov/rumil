package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joangrigorov/rumil/internal/testutil"
)

// runRoot executes the CLI with the given arguments, the way a shell would.
func runRoot(t *testing.T, args ...string) (string, error) {
	t.Helper()

	root := newRootCmd()
	out := &strings.Builder{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs(args)

	err := root.Execute()
	return out.String(), err
}

// reviewFakes wires the hermetic GitHub and model endpoints a review needs, and
// returns a configuration file that points at them.
func reviewFakes(t *testing.T) string {
	t.Helper()

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

	return writeConfig(t, llm.URL())
}

// The point of discovery: no --config, and the file in the working directory is
// the one that is read, which is what the CLI did before the search existed.
func TestReviewFindsTheConfigurationInTheWorkingDirectory(t *testing.T) {
	configPath := reviewFakes(t)
	t.Chdir(filepath.Dir(configPath))

	out, err := runRoot(t, "review", "--repo", "acme/payments", "--pr", "42")
	if err != nil {
		t.Fatalf("review without --config returned error: %v (output: %s)", err, out)
	}
	if !strings.Contains(out, "review submitted to acme/payments#42 as COMMENT") {
		t.Errorf("output does not carry the review result:\n%s", out)
	}
}

// RUMIL_CONFIG is for the runs a flag is awkward in: a container, a unit file.
func TestReviewFindsTheConfigurationNamedByTheEnvironment(t *testing.T) {
	configPath := reviewFakes(t)
	t.Setenv("RUMIL_CONFIG", configPath)
	t.Chdir(t.TempDir())

	out, err := runRoot(t, "review", "--repo", "acme/payments", "--pr", "42")
	if err != nil {
		t.Fatalf("review with %s set returned error: %v (output: %s)", "RUMIL_CONFIG", err, out)
	}
	if !strings.Contains(out, "review submitted to acme/payments#42 as COMMENT") {
		t.Errorf("output does not carry the review result:\n%s", out)
	}
}

// A named file beats discovery, so a working directory cannot shadow the file
// the operator pointed at.
func TestTheConfigFlagBeatsTheWorkingDirectory(t *testing.T) {
	configPath := reviewFakes(t)

	// An unreadable file of the same name in the working directory: if
	// discovery won, the command would fail on it.
	decoy := filepath.Join(t.TempDir(), "rumil.yaml")
	if err := os.WriteFile(decoy, []byte("app:\n  log_level: not-a-level\n"), 0o600); err != nil {
		t.Fatalf("writing the decoy config: %v", err)
	}
	t.Chdir(filepath.Dir(decoy))

	out, err := runRoot(t, "review", "--config", configPath, "--repo", "acme/payments", "--pr", "42")
	if err != nil {
		t.Fatalf("review with --config returned error: %v (output: %s)", err, out)
	}
	if !strings.Contains(out, "review submitted to acme/payments#42 as COMMENT") {
		t.Errorf("output does not carry the review result:\n%s", out)
	}
}
