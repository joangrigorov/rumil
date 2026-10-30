package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joangrigorov/rumil/internal/testutil"
)

const evalFixtureDoc = `
name: settlement
diff: |
  diff --git a/main.go b/main.go
  index 1111111..2222222 100644
  --- a/main.go
  +++ b/main.go
  @@ -10,4 +10,6 @@ func main() {
   	if err != nil {
   		log.Fatal(err)
   	}
  +	// retry the settlement call
  +	settle(ctx)
   	defer conn.Close()
files:
  main.go: |
    package main

    func main() {
    	settle(nil)
    }
expected:
  - path: main.go
    line: 13
    match: timeout
`

// writeFixtures writes one fixture and returns the directory.
func writeFixtures(t *testing.T, doc string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "settlement.yaml"), []byte(doc), 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}
	return dir
}

// The eval command runs the fixture against the configured provider and prints
// the score, without touching a real repository.
func TestEvalCommandScoresAFixture(t *testing.T) {
	llm := testutil.NewFakeLLM(t)
	llm.QueueToolCall("submit_pr_review", map[string]any{
		"event":        "COMMENT",
		"summary_body": "One finding.",
		"comments": []map[string]any{
			{"path": "main.go", "line": 13, "side": "RIGHT", "body": "This call has no timeout."},
		},
	})
	llm.QueueText("Review submitted.")

	root := newRootCmd()
	out := &strings.Builder{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{
		"eval", "--config", writeConfig(t, llm.URL()),
		"--fixtures", writeFixtures(t, evalFixtureDoc),
	})

	if err := root.Execute(); err != nil {
		t.Fatalf("eval command returned error: %v (output: %s)", err, out.String())
	}
	text := out.String()
	for _, want := range []string{"FIXTURE", "settlement", "1 expected, 1 matched", "recall 1.00"} {
		if !strings.Contains(text, want) {
			t.Errorf("output lost %q:\n%s", want, text)
		}
	}
}

// The JSON shape is what a comparison script parses.
func TestEvalCommandPrintsJSON(t *testing.T) {
	llm := testutil.NewFakeLLM(t)
	llm.QueueToolCall("submit_pr_review", map[string]any{
		"event":        "COMMENT",
		"summary_body": "Nothing to report.",
	})
	llm.QueueText("Review submitted.")

	root := newRootCmd()
	out := &strings.Builder{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{
		"eval", "--config", writeConfig(t, llm.URL()),
		"--fixtures", writeFixtures(t, evalFixtureDoc), "--json",
	})

	if err := root.Execute(); err != nil {
		t.Fatalf("eval command returned error: %v (output: %s)", err, out.String())
	}
	var document struct {
		Fixtures []struct {
			Name     string `json:"name"`
			Expected int    `json:"expected"`
			Matched  int    `json:"matched"`
			Error    string `json:"error"`
		} `json:"fixtures"`
		Totals struct {
			Fixtures int     `json:"fixtures"`
			Missed   int     `json:"missed"`
			Recall   float64 `json:"recall"`
		} `json:"totals"`
	}
	if err := json.Unmarshal([]byte(out.String()), &document); err != nil {
		t.Fatalf("the output is not JSON: %v\n%s", err, out.String())
	}
	if document.Totals.Fixtures != 1 || document.Totals.Missed != 1 || document.Totals.Recall != 0 {
		t.Errorf("totals = %+v, want one fixture with a miss", document.Totals)
	}
}
