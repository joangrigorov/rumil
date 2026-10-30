package reviewer

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/joangrigorov/rumil/internal/config"
	"github.com/joangrigorov/rumil/internal/testutil"
)

// scriptedUsageReview scripts three steps: a read, the submit call, and the
// closing text. Each step reports its own usage, so the totals are the sum.
func scriptedUsageReview(t *testing.T) *harness {
	t.Helper()

	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	llm.QueueTurn(testutil.ScriptedTurn{
		ToolCalls:    []testutil.ToolCall{{Name: "get_pr_metadata", Args: map[string]any{}}},
		PromptTokens: 100, CachedTokens: 40,
	})
	llm.QueueTurn(testutil.ScriptedTurn{
		ToolCalls: []testutil.ToolCall{{Name: "submit_pr_review", Args: map[string]any{
			"event": "COMMENT", "summary_body": "Looks fine.",
		}}},
		PromptTokens: 200, CachedTokens: 50,
	})
	llm.QueueTurn(testutil.ScriptedTurn{Text: "Review submitted.", PromptTokens: 300})

	return newHarness(t, gh, llm, harnessOpts{})
}

// A session's totals are the sum of its steps, with cache tokens kept apart
// from the input the provider charged at the full rate.
func TestReviewRecordsUsageAcrossSteps(t *testing.T) {
	h := scriptedUsageReview(t)

	result, err := h.review(t)
	if err != nil {
		t.Fatalf("Review returned error: %v\nlogs:\n%s", err, h.logs.String())
	}
	if result.Usage == nil {
		t.Fatal("the session recorded no usage")
	}
	usage := *result.Usage
	if usage.InputTokens != 510 {
		t.Errorf("input tokens = %d, want 510 (60+150+300)", usage.InputTokens)
	}
	if usage.OutputTokens != 15 {
		t.Errorf("output tokens = %d, want 15", usage.OutputTokens)
	}
	if usage.CacheReadTokens != 90 {
		t.Errorf("cache read tokens = %d, want 90 (40+50)", usage.CacheReadTokens)
	}
	if usage.CacheCreationTokens != 0 {
		t.Errorf("cache creation tokens = %d, want 0", usage.CacheCreationTokens)
	}
	if usage.TotalTokens() != 615 {
		t.Errorf("total tokens = %d, want 615", usage.TotalTokens())
	}
	// No prices in this configuration, so the cost is unknown rather than zero.
	if usage.CostUSD != nil {
		t.Errorf("cost = %v, want nil without prices", *usage.CostUSD)
	}
}

// Configured prices produce an estimate, and the input rate prices both plain
// input and cache reads the operator priced separately.
func TestReviewEstimatesCostWhenPricesAreConfigured(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	llm.QueueTurn(testutil.ScriptedTurn{
		ToolCalls:    []testutil.ToolCall{{Name: "get_pr_metadata", Args: map[string]any{}}},
		PromptTokens: 100, CachedTokens: 40,
	})
	llm.QueueTurn(testutil.ScriptedTurn{
		ToolCalls: []testutil.ToolCall{{Name: "submit_pr_review", Args: map[string]any{
			"event": "COMMENT", "summary_body": "Looks fine.",
		}}},
		PromptTokens: 200, CachedTokens: 50,
	})
	llm.QueueTurn(testutil.ScriptedTurn{Text: "Review submitted.", PromptTokens: 300})

	h := newHarness(t, gh, llm, harnessOpts{chatPrices: `
      price_input_per_mtok: 1
      price_output_per_mtok: 2
      price_cache_read_per_mtok: 0.5
`})

	result, err := h.review(t)
	if err != nil {
		t.Fatalf("Review returned error: %v\nlogs:\n%s", err, h.logs.String())
	}
	if result.Usage == nil || result.Usage.CostUSD == nil {
		t.Fatal("the session recorded no estimated cost")
	}
	// 510 input at $1/M, 15 output at $2/M, 90 cache reads at $0.50/M.
	want := 510.0/1e6 + 15.0*2/1e6 + 90.0*0.5/1e6
	if math.Abs(*result.Usage.CostUSD-want) > 1e-9 {
		t.Errorf("cost = %v, want %v", *result.Usage.CostUSD, want)
	}
}

// A provider that reports no usage leaves the total absent, which the deck
// shows as "usage not reported" rather than as a free review.
func TestReviewWithoutUsageRecordsNone(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	llm.QueueTurn(testutil.ScriptedTurn{
		ToolCalls:    []testutil.ToolCall{{Name: "get_pr_metadata", Args: map[string]any{}}},
		UsageOmitted: true,
	})
	llm.QueueTurn(testutil.ScriptedTurn{
		ToolCalls: []testutil.ToolCall{{Name: "submit_pr_review", Args: map[string]any{
			"event": "COMMENT", "summary_body": "Looks fine.",
		}}},
		UsageOmitted: true,
	})
	llm.QueueTurn(testutil.ScriptedTurn{Text: "Review submitted.", UsageOmitted: true})

	h := newHarness(t, gh, llm, harnessOpts{})
	result, err := h.review(t)
	if err != nil {
		t.Fatalf("Review returned error: %v\nlogs:\n%s", err, h.logs.String())
	}
	if result.Usage != nil {
		t.Errorf("usage = %+v, want nil when the provider reports nothing", *result.Usage)
	}
}

// A token budget stops the session before the next model request and names the
// limit, so the review fails with a reason an operator can act on.
func TestReviewStopsAtItsTokenBudget(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	llm.QueueTurn(testutil.ScriptedTurn{
		ToolCalls:    []testutil.ToolCall{{Name: "get_pr_metadata", Args: map[string]any{}}},
		PromptTokens: 100, CachedTokens: 40,
	})
	// A second turn is queued but must never be requested: the budget is
	// checked between steps.
	llm.QueueText("Review submitted.")

	h := newHarness(t, gh, llm, harnessOpts{reviewLimits: "  max_total_tokens: 100\n"})

	result, err := h.review(t)
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("Review error = %v, want ErrBudgetExceeded\nlogs:\n%s", err, h.logs.String())
	}
	if !strings.Contains(err.Error(), "review.max_total_tokens") {
		t.Errorf("error = %v, want it to name the limit", err)
	}
	if result == nil || result.Usage == nil {
		t.Fatal("the session lost the usage it had already spent")
	}
	if result.Usage.TotalTokens() != 105 {
		t.Errorf("usage = %d tokens, want the 105 of the first step", result.Usage.TotalTokens())
	}
	if llm.Count() != 1 {
		t.Errorf("model requests = %d, want one: the budget stops before the next step", llm.Count())
	}
}

// A cost budget uses the configured prices and names itself in the error.
func TestReviewStopsAtItsCostBudget(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	llm.QueueTurn(testutil.ScriptedTurn{
		ToolCalls:    []testutil.ToolCall{{Name: "get_pr_metadata", Args: map[string]any{}}},
		PromptTokens: 100, CachedTokens: 40,
	})
	llm.QueueText("Review submitted.")

	h := newHarness(t, gh, llm, harnessOpts{
		chatPrices: `
      price_input_per_mtok: 1
      price_output_per_mtok: 2
      price_cache_read_per_mtok: 0.5
`,
		reviewLimits: "  max_cost_usd: 0.0000001\n",
	})

	_, err := h.review(t)
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("Review error = %v, want ErrBudgetExceeded\nlogs:\n%s", err, h.logs.String())
	}
	if !strings.Contains(err.Error(), "review.max_cost_usd") {
		t.Errorf("error = %v, want it to name the limit", err)
	}
}

// A budget is a ceiling: spending exactly the limit is allowed, and the next
// token is over it.
func TestBudgetBoundaryIsInclusive(t *testing.T) {
	budget := usageBudget{maxTokens: 100, maxCost: 0.01, priced: true, price: config.TokenPrice{Input: 1}}
	if err := budget.check(Usage{InputTokens: 100}); err != nil {
		t.Errorf("check at the limit = %v, want nil", err)
	}
	if err := budget.check(Usage{InputTokens: 101}); !errors.Is(err, ErrBudgetExceeded) {
		t.Errorf("check over the limit = %v, want ErrBudgetExceeded", err)
	}

	cost := usageBudget{maxCost: 1, priced: true, price: config.TokenPrice{Input: 1e6}}
	if err := cost.check(Usage{InputTokens: 1}); err != nil {
		t.Errorf("cost check at the limit = %v, want nil", err)
	}
	if err := cost.check(Usage{InputTokens: 2}); !errors.Is(err, ErrBudgetExceeded) {
		t.Errorf("cost check over the limit = %v, want ErrBudgetExceeded", err)
	}
}

// The worker runs with an observer, which puts the provider on the streaming
// wire. Usage has to survive that path too, because it is the one production
// uses.
func TestUsageIsRecordedFromAStreamingSession(t *testing.T) {
	h := scriptedUsageReview(t)
	observer := &captureObserver{}

	result, err := h.engine.Review(context.Background(), Request{
		Owner: h.gh.Owner, Repo: h.gh.Repo, Number: h.gh.Number, Observer: observer,
	})
	if err != nil {
		t.Fatalf("Review returned error: %v\nlogs:\n%s", err, h.logs.String())
	}
	if result.Usage == nil {
		t.Fatal("the streaming session recorded no usage")
	}
	if result.Usage.InputTokens != 510 || result.Usage.OutputTokens != 15 || result.Usage.CacheReadTokens != 90 {
		t.Errorf("usage = %+v, want 510 in, 15 out, 90 cached", *result.Usage)
	}
}

// A repository may choose a provider other than the configured default, and the
// budget and the recorded cost have to price the session at that provider.
func TestUsageBudgetUsesTheSessionsProvider(t *testing.T) {
	input, output := 1.0, 2.0
	provider := &config.Provider{ID: "cheap", PriceInputPerMTok: &input, PriceOutputPerMTok: &output}
	engine := &Engine{cfg: &config.Config{Review: config.Review{MaxCostUSD: 10}}}

	budget := engine.newUsageBudget(provider, engine.cfg.Review)
	usage := Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000}
	if got := budget.cost(usage); math.Abs(got-3) > 1e-9 {
		t.Errorf("budget cost = %v, want 3 at the session's provider", got)
	}
	if cost := engine.estimateCost(usage, provider); cost == nil || math.Abs(*cost-3) > 1e-9 {
		t.Errorf("estimated cost = %v, want 3 at the session's provider", cost)
	}
	if cost := engine.estimateCost(usage, nil); cost != nil {
		t.Errorf("estimated cost = %v, want nil without a provider", cost)
	}
}
