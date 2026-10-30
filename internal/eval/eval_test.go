package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joangrigorov/rumil/internal/config"
	"github.com/joangrigorov/rumil/internal/testutil"
)

const fixtureDiff = `diff --git a/main.go b/main.go
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
`

func writeFixture(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settlement.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}
	return path
}

// The fixtures shipped in the repository are what the command runs by default,
// so they have to load.
func TestShippedFixturesLoad(t *testing.T) {
	fixtures, err := LoadDir(filepath.Join("..", "..", "fixtures"))
	if err != nil {
		t.Fatalf("LoadDir returned error: %v", err)
	}
	if len(fixtures) < 3 {
		t.Fatalf("the shipped set holds %d fixtures, want at least three", len(fixtures))
	}
	names := make(map[string]bool, len(fixtures))
	for _, fixture := range fixtures {
		names[fixture.Name] = true
	}
	for _, want := range []string{"unbounded-retry", "always-nil-error", "token-in-log"} {
		if !names[want] {
			t.Errorf("the shipped set lost %q: %v", want, names)
		}
	}
}

func TestLoadFileAppliesDefaults(t *testing.T) {
	path := writeFixture(t, `
description: "the settlement worker retries without a bound"
diff: |
  diff --git a/main.go b/main.go
  --- a/main.go
  +++ b/main.go
files:
  main.go: "package main\n"
expected:
  - path: main.go
    line: 13
`)

	fixture, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile returned error: %v", err)
	}
	if fixture.Name != "settlement" {
		t.Errorf("name = %q, want the file name", fixture.Name)
	}
	if fixture.Owner != DefaultOwner || fixture.Repo != DefaultRepo || fixture.Number != DefaultNumber {
		t.Errorf("coordinates = %s/%s#%d, want the defaults", fixture.Owner, fixture.Repo, fixture.Number)
	}
	if fixture.BaseSHA != DefaultBaseSHA || fixture.HeadSHA != DefaultHeadSHA {
		t.Errorf("shas = %s/%s, want the defaults", fixture.BaseSHA, fixture.HeadSHA)
	}
}

func TestLoadFileRefusesAnUnusableFixture(t *testing.T) {
	path := writeFixture(t, `
files:
  main.go: "package main\n"
expected:
  - path: ""
    line: 0
`)
	_, err := LoadFile(path)
	if err == nil {
		t.Fatal("LoadFile accepted a fixture with no diff")
	}
	for _, want := range []string{"diff is empty", "path is required", "line must be positive"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to name %q", err, want)
		}
	}
}

// A finding accounts for one expectation, lines are matched with the same
// tolerance a person reading the diff would allow, and the body match ignores
// case.
func TestScoreCorrelatesFindings(t *testing.T) {
	fixture := Fixture{
		Name: "retry",
		Expected: []Expectation{
			{Path: "main.go", Line: 13, Match: "TIMEOUT"},
			{Path: "main.go", Line: 40, Match: "unbounded"},
		},
		FalsePositives: []Expectation{
			{Path: "main.go", Line: 12, Match: "naming"},
		},
	}
	findings := []Finding{
		{Path: "main.go", Line: 12, Body: "This call has no timeout."},
		{Path: "main.go", Line: 12, Body: "The naming here is odd."},
		{Path: "main.go", Line: 50, Body: "Something else."},
		{Path: "main.go", Line: 12, Body: "This call has no timeout."},
	}

	score := Score(fixture, findings)
	if score.Expected != 2 || score.Matched != 1 {
		t.Errorf("matched %d of %d, want one", score.Matched, score.Expected)
	}
	if len(score.Missed) != 1 || score.Missed[0].Line != 40 {
		t.Errorf("missed = %+v, want the unbounded finding", score.Missed)
	}
	if len(score.KnownFalsePositives) != 1 {
		t.Errorf("known false positives = %+v, want one", score.KnownFalsePositives)
	}
	if len(score.Unexpected) != 2 {
		t.Errorf("unexpected = %+v, want the stray finding and the repeated one", score.Unexpected)
	}
}

func TestReportShapeIsStable(t *testing.T) {
	report := Report{Fixtures: []FixtureScore{
		{Name: "a", Expected: 2, Matched: 1, Missed: []Expectation{{Path: "a.go", Line: 4, Match: "nil"}}},
		{Name: "b", Expected: 1, Matched: 1, Unexpected: []Finding{{Path: "b.go", Line: 9, Body: "noise"}}},
	}}

	var table bytes.Buffer
	if err := WriteTable(&table, report); err != nil {
		t.Fatalf("WriteTable returned error: %v", err)
	}
	for _, want := range []string{"FIXTURE", "a", "b", "2 fixtures", "recall 0.67", "missed a.go:4 nil", "unexpected b.go:9 noise"} {
		if !strings.Contains(table.String(), want) {
			t.Errorf("the table lost %q:\n%s", want, table.String())
		}
	}

	var encoded bytes.Buffer
	if err := WriteJSON(&encoded, report); err != nil {
		t.Fatalf("WriteJSON returned error: %v", err)
	}
	var document struct {
		Fixtures []FixtureScore `json:"fixtures"`
		Totals   Totals         `json:"totals"`
	}
	if err := json.Unmarshal(encoded.Bytes(), &document); err != nil {
		t.Fatalf("the JSON report does not parse: %v\n%s", err, encoded.String())
	}
	if len(document.Fixtures) != 2 || document.Totals.Matched != 2 || document.Totals.Missed != 1 {
		t.Errorf("report = %+v, want two fixtures with the totals", document)
	}
}

// The runner puts the fixture in front of the real engine, with the fake GitHub
// standing in for the repository, and scores what the provider submits.
func TestRunnerScoresAReview(t *testing.T) {
	llm := testutil.NewFakeLLM(t)
	llm.QueueToolCall("submit_pr_review", map[string]any{
		"event":        "COMMENT",
		"summary_body": "The retry is unbounded.",
		"comments": []map[string]any{
			{"path": "main.go", "line": 13, "side": "RIGHT", "body": "This call has no timeout."},
		},
	})
	llm.QueueText("Review submitted.")

	keyPath := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(keyPath, testutil.TestPrivateKeyPEM(t), 0o600); err != nil {
		t.Fatalf("writing the test key: %v", err)
	}
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
app:
  bot_handle: "@rumil"
github:
  app_id: 1
  installation_id: 2
  private_key_path: %q
models:
  providers:
    - id: "chat"
      provider: "openai-compatible"
      base_url: %q
      model_name: "chat"
      api_key: "key"
`, keyPath, llm.URL()+"/v1")), "eval_test.yaml")
	if err != nil {
		t.Fatalf("parsing the test configuration: %v", err)
	}

	fixture := Fixture{
		Name:    "settlement",
		Owner:   "acme",
		Repo:    "payments",
		Number:  42,
		Title:   "Retry the settlement call",
		Body:    "Adds a retry.",
		BaseSHA: DefaultBaseSHA,
		HeadSHA: DefaultHeadSHA,
		Diff:    fixtureDiff,
		Files: map[string]string{
			"main.go": "package main\n\nfunc main() {\n\tsettle(nil)\n}\n",
		},
		Expected: []Expectation{{Path: "main.go", Line: 13, Match: "timeout"}},
	}

	report := Runner{Config: cfg, Timeout: time.Minute}.Run(context.Background(), []Fixture{fixture})
	if len(report.Fixtures) != 1 {
		t.Fatalf("report has %d fixtures, want one", len(report.Fixtures))
	}
	score := report.Fixtures[0]
	if score.Error != "" {
		t.Fatalf("the fixture failed: %s", score.Error)
	}
	if score.Matched != 1 || len(score.Missed) != 0 {
		t.Errorf("score = %+v, want the expected finding matched", score)
	}
	if score.ReviewEvent != "COMMENT" {
		t.Errorf("review event = %q, want COMMENT", score.ReviewEvent)
	}
	if len(score.Findings) != 1 || score.Findings[0].Line != 13 {
		t.Errorf("findings = %+v, want the submitted comment", score.Findings)
	}
}
