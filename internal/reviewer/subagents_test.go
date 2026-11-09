package reviewer

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/joangrigorov/rumil/internal/config"
	"github.com/joangrigorov/rumil/internal/mcpservers"
	"github.com/joangrigorov/rumil/internal/testutil"
)

func ptr[T any](value T) *T { return &value }

// The fleet's whole safety story is that the child tool set is the allowlist.
// If a write or the fleet tool itself reached a child, a subagent could submit
// a review or spawn another generation.
func TestSubagentReadSpecsExcludesWritesAndFleetTools(t *testing.T) {
	specs := []mcpservers.Spec{
		{Name: "get_pr_diff"},
		{Name: "get_file_contents"},
		{Name: "get_pr_metadata"},
		{Name: "list_pr_conversation"},
		{Name: "submit_pr_review"},
		{Name: "resolve_review_thread"},
		{Name: "spawn_reviewer"},
	}
	memory := []mcpservers.Spec{{Name: "search_memory"}, {Name: "remember"}}

	got := subagentReadSpecs(specs, memory)
	names := make(map[string]bool, len(got))
	for _, spec := range got {
		names[spec.Name] = true
	}
	for _, want := range []string{"get_pr_diff", "get_file_contents", "search_memory"} {
		if !names[want] {
			t.Errorf("subagentReadSpecs omitted %s", want)
		}
	}
	for _, banned := range []string{"get_pr_metadata", "list_pr_conversation", "submit_pr_review",
		"resolve_review_thread", "spawn_reviewer", "remember"} {
		if names[banned] {
			t.Errorf("subagentReadSpecs exposed %s", banned)
		}
	}
}

// A review with the fleet on must run the child, make the verification call,
// and only then submit. The child's advertised tools are the check that no
// write reached it.
func TestFleetSpawnsAReadOnlyChildAndVerifiesBeforeSubmitting(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)

	report := `{"findings":[{"path":"main.go","line":13,"side":"RIGHT","severity":"high","title":"settle ignores the error","body":"the call is unchecked","evidence":"main.go line 13"}],"files_reviewed":["main.go","service.go"],"notes":""}`
	verdict := `{"verdicts":[{"index":0,"verdict":"keep","reason":"the line is unchecked"}],"additional_findings":[],"coverage_notes":"","notes":""}`

	llm.QueueToolCall("spawn_reviewer", map[string]any{
		"tasks": []any{map[string]any{
			"label": "main",
			"task":  "review the settlement change",
			"paths": []any{"main.go", "service.go"},
		}},
	})
	llm.QueueText(report)
	llm.QueueToolCall("verify_review", map[string]any{
		"summary": "one finding",
		"findings": []any{map[string]any{
			"path": "main.go", "line": 13, "side": "RIGHT", "body": "the call is unchecked",
		}},
	})
	llm.QueueText(verdict)
	llm.QueueToolCall("submit_pr_review", map[string]any{
		"event": "COMMENT", "summary_body": "The settlement call is unchecked.",
	})

	h := newHarness(t, gh, llm, harnessOpts{reviewLimits: `  subagents:
    enabled: true
    min_changed_files: 1
    max_concurrency: 1
    max_runs: 4
    max_turns: 4
    verify: true
    require_coverage: true
`})

	res, err := h.review(t)
	if err != nil {
		t.Fatalf("Review returned error: %v\nlogs:\n%s", err, h.logs.String())
	}
	if !res.ReviewSubmitted {
		t.Fatal("ReviewSubmitted = false, want the review to reach GitHub")
	}

	requests := llm.Requests()
	if len(requests) < 5 {
		t.Fatalf("model requests = %d, want at least 5 (leader, child, verifier, submit)", len(requests))
	}

	// The child is the second request: the leader asks for spawn_reviewer and
	// the tool blocks until the child answers.
	child := requests[1]
	if !strings.Contains(child.SystemPrompt, "read-only subagent reviewer") {
		t.Errorf("child system prompt does not identify the read-only subagent:\n%s", child.SystemPrompt)
	}
	if strings.Contains(child.SystemPrompt, "You are Rumil") {
		t.Error("child received the leader prompt")
	}
	tools := make(map[string]bool, len(child.Tools))
	for _, name := range child.Tools {
		tools[name] = true
	}
	for _, banned := range []string{"submit_pr_review", "resolve_review_thread", "spawn_reviewer", "verify_review"} {
		if tools[banned] {
			t.Errorf("child was advertised %s", banned)
		}
	}
	if !tools["get_pr_diff"] || !tools["get_file_contents"] {
		t.Errorf("child tools = %v, want the read-only GitHub tools", child.Tools)
	}
}

// The coverage gate refuses the first submit, then lets the review through with
// the remaining paths named in the summary. Verification is off here so the
// test isolates one gate.
func TestFleetCoverageGateRefusesOnceThenNamesTheGap(t *testing.T) {
	f := &fleet{
		cfg: config.Subagents{
			MaxConcurrency:  1,
			MaxRuns:         2,
			MaxTurns:        2,
			Verify:          ptr(false),
			RequireCoverage: ptr(true),
		},
		assigned:  map[string]string{"main.go": "main"},
		skipped:   map[string]string{},
		knownPath: map[string]struct{}{"main.go": {}, "service.go": {}},
		ignored:   func(string) bool { return false },
	}

	if _, err := f.beforeSubmit(); err == nil {
		t.Fatal("beforeSubmit accepted an incomplete coverage set")
	}
	note, err := f.beforeSubmit()
	if err != nil {
		t.Fatalf("second beforeSubmit returned error: %v", err)
	}
	if !strings.Contains(note, "service.go") {
		t.Errorf("note = %q, want the uncovered path named", note)
	}

	// A skipped path is covered for the gate.
	f.skipped["service.go"] = "generated"
	note, err = f.beforeSubmit()
	if err != nil {
		t.Fatalf("beforeSubmit after skipping returned error: %v", err)
	}
	if note != "" {
		t.Errorf("note = %q, want none after every path is covered", note)
	}
}

// The run cap is a hard stop: a batch that would exceed it is refused before
// any child starts.
func TestFleetRefusesABatchBeyondTheRunCap(t *testing.T) {
	f := &fleet{
		cfg:       config.Subagents{MaxConcurrency: 2, MaxRuns: 2, MaxTurns: 2},
		knownPath: map[string]struct{}{"a.go": {}, "b.go": {}, "c.go": {}},
		ignored:   func(string) bool { return false },
		assigned:  map[string]string{},
		skipped:   map[string]string{},
	}
	f.runs = 1

	_, err := f.spawn(context.Background(), map[string]any{
		"tasks": []any{
			map[string]any{"label": "a", "task": "t", "paths": []any{"a.go"}},
			map[string]any{"label": "b", "task": "t", "paths": []any{"b.go"}},
		},
	})
	if err == nil {
		t.Fatal("spawn accepted a batch past max_runs")
	}
	if !strings.Contains(err.Error(), "run budget") {
		t.Errorf("error = %v, want the run budget named", err)
	}
}

// A malformed child report is still a successful child; the leader reads the
// text and the deck records no finding count.
func TestCountReportFindingsToleratesProse(t *testing.T) {
	if got := countReportFindings("I looked and found nothing."); got != 0 {
		t.Errorf("findings = %d, want 0 for prose", got)
	}
	if got := countReportFindings(`{"findings":[{"path":"a"},{"path":"b"}]}`); got != 2 {
		t.Errorf("findings = %d, want 2", got)
	}
}

// Two children run at once. The fake script gives both the same text, so the
// test does not depend on which one wins the queue; the race detector is the
// point.
func TestFleetRunsChildrenConcurrently(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)

	report := `{"findings":[],"files_reviewed":[],"notes":"clean"}`
	llm.QueueToolCall("spawn_reviewer", map[string]any{
		"tasks": []any{
			map[string]any{"label": "main", "task": "review main", "paths": []any{"main.go"}},
			map[string]any{"label": "service", "task": "review service", "paths": []any{"service.go"}},
		},
	})
	llm.QueueText(report)
	llm.QueueText(report)
	llm.QueueToolCall("submit_pr_review", map[string]any{
		"event": "COMMENT", "summary_body": "Clean.",
	})

	h := newHarness(t, gh, llm, harnessOpts{reviewLimits: `  subagents:
    enabled: true
    min_changed_files: 1
    max_concurrency: 2
    max_runs: 4
    max_turns: 4
    verify: false
    require_coverage: true
`})

	res, err := h.review(t)
	if err != nil {
		t.Fatalf("Review returned error: %v\nlogs:\n%s", err, h.logs.String())
	}
	if !res.ReviewSubmitted {
		t.Fatal("ReviewSubmitted = false, want the review to reach GitHub")
	}

	children := 0
	for _, request := range llm.Requests() {
		if strings.Contains(request.SystemPrompt, "read-only subagent reviewer") {
			children++
		}
	}
	if children != 2 {
		t.Errorf("child requests = %d, want 2", children)
	}
}

func TestFleetVerificationGateRefusesOnceThenNamesTheFailure(t *testing.T) {
	f := &fleet{
		cfg: config.Subagents{
			MaxConcurrency:  1,
			MaxRuns:         2,
			MaxTurns:        2,
			Verify:          ptr(true),
			RequireCoverage: ptr(false),
		},
		knownPath: map[string]struct{}{"main.go": {}},
		assigned:  map[string]string{"main.go": "main"},
		skipped:   map[string]string{},
		ignored:   func(string) bool { return false },
		verifyErr: "the child timed out",
	}

	if _, err := f.beforeSubmit(); err == nil || !strings.Contains(err.Error(), "verify_review") {
		t.Fatalf("first beforeSubmit = %v, want the verification instruction", err)
	}
	note, err := f.beforeSubmit()
	if err != nil {
		t.Fatalf("second beforeSubmit returned error: %v", err)
	}
	if !strings.Contains(note, "Verification: unavailable") || !strings.Contains(note, "timed out") {
		t.Errorf("note = %q, want the verification failure named", note)
	}

	f.verified = true
	note, err = f.beforeSubmit()
	if err != nil {
		t.Fatalf("verified beforeSubmit returned error: %v", err)
	}
	if note != "" {
		t.Errorf("note = %q, want none after verification", note)
	}
}

func TestFleetIsOfferedOnlyWhenEnabledAndLargeEnough(t *testing.T) {
	cases := []struct {
		name   string
		limits string
		want   bool
	}{
		{
			name: "enabled above the threshold",
			limits: `  subagents:
    enabled: true
    min_changed_files: 2
    max_concurrency: 1
    max_runs: 4
    max_turns: 4
    verify: false
    require_coverage: false
`,
			want: true,
		},
		{
			name: "enabled below the threshold",
			limits: `  subagents:
    enabled: true
    min_changed_files: 100
    max_concurrency: 1
    max_runs: 4
    max_turns: 4
    verify: false
    require_coverage: false
`,
			want: false,
		},
		{
			name:   "disabled",
			limits: "",
			want:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gh := testutil.NewFakeGitHub(t)
			llm := testutil.NewFakeLLM(t)
			llm.QueueToolCall("submit_pr_review", map[string]any{"event": "COMMENT", "summary_body": "Clean."})
			h := newHarness(t, gh, llm, harnessOpts{reviewLimits: tc.limits})

			if _, err := h.review(t); err != nil {
				t.Fatalf("Review returned error: %v\nlogs:\n%s", err, h.logs.String())
			}
			requests := llm.Requests()
			if len(requests) == 0 {
				t.Fatal("the review made no model request")
			}
			offered := slices.Contains(requests[0].Tools, "spawn_reviewer")
			if offered != tc.want {
				t.Errorf("spawn_reviewer offered = %t, want %t (tools: %v)", offered, tc.want, requests[0].Tools)
			}
		})
	}
}

func TestFleetRefusesAChildAfterTheBudgetIsSpent(t *testing.T) {
	f := &fleet{
		tracker: &usageTracker{},
		budget:  usageBudget{maxTokens: 100},
	}
	f.tracker.add(Usage{InputTokens: 150})
	if err := f.budgetExhausted(); err == nil {
		t.Fatal("budgetExhausted accepted a spent budget")
	}

	within := &fleet{tracker: &usageTracker{}, budget: usageBudget{maxTokens: 100}}
	within.tracker.add(Usage{InputTokens: 50})
	if err := within.budgetExhausted(); err != nil {
		t.Errorf("budgetExhausted = %v, want nil under the limit", err)
	}
}
