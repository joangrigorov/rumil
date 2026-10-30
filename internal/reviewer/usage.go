package reviewer

import (
	"errors"
	"fmt"
	"sync"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/joangrigorov/rumil/internal/config"
)

// ErrBudgetExceeded reports a session that used more than its configured
// budget. It ends the session before the next model request: the review was
// either submitted by then or it was not going to be.
var ErrBudgetExceeded = errors.New("the review budget was exceeded")

// Usage is what one review session was billed for, summed over its steps.
// Cache tokens stay separate so an operator can see how much of the input came
// from a cache.
type Usage struct {
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	// CostUSD is the estimated cost, nil when the provider has no prices.
	CostUSD *float64
}

// TotalTokens is the sum every category, which is the context-fill figure kit
// documents and the one a token budget counts.
func (u Usage) TotalTokens() int64 {
	return u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheCreationTokens
}

// usageTracker accumulates the usage kit reports for each step. Kit emits a
// StepFinishEvent per model request, so the sum is the session's bill even
// across the extra prompts a stalled or truncated turn gets.
type usageTracker struct {
	mu       sync.Mutex
	reported bool
	usage    Usage
}

// note records one step. A provider that reports nothing leaves every field
// zero, which is indistinguishable from a provider that says zero; both are
// recorded as "usage not reported".
func (t *usageTracker) note(usage kit.LLMUsage) {
	if usage.InputTokens == 0 && usage.OutputTokens == 0 &&
		usage.CacheReadTokens == 0 && usage.CacheCreationTokens == 0 &&
		usage.TotalTokens == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.reported = true
	t.usage.InputTokens += usage.InputTokens
	t.usage.OutputTokens += usage.OutputTokens
	t.usage.CacheReadTokens += usage.CacheReadTokens
	t.usage.CacheCreationTokens += usage.CacheCreationTokens
}

// noteEvent records the usage of a step-finish event.
func (t *usageTracker) noteEvent(event kit.Event) {
	if finish, ok := event.(kit.StepFinishEvent); ok {
		t.note(finish.Usage)
	}
}

// snapshot returns the total and whether any provider reported usage.
func (t *usageTracker) snapshot() (Usage, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.usage, t.reported
}

// usageBudget is the configured per-session limit, with prices resolved once.
type usageBudget struct {
	maxTokens int64
	maxCost   float64
	price     config.TokenPrice
	priced    bool
}

// newUsageBudget reads the limits from the effective review settings, which may
// carry an installation's own budget. provider is the chat provider this
// session runs on, which is what prices the cost limit when a repository chose
// a provider of its own.
func (e *Engine) newUsageBudget(provider *config.Provider, review config.Review) usageBudget {
	budget := usageBudget{
		maxTokens: review.MaxTotalTokens,
		maxCost:   review.MaxCostUSD,
	}
	if provider != nil {
		budget.price = provider.TokenPrice()
		budget.priced = provider.HasPrice()
	}
	return budget
}

// enabled reports whether any limit is configured.
func (b usageBudget) enabled() bool {
	return b.maxTokens > 0 || b.maxCost > 0
}

// cost estimates what the usage costs at the configured prices. Cache creation
// is priced at the input rate, which is what a provider that charges for it
// bills; a provider that does not charge for it is overestimated by that much.
func (b usageBudget) cost(usage Usage) float64 {
	return float64(usage.InputTokens)*b.price.Input/1e6 +
		float64(usage.OutputTokens)*b.price.Output/1e6 +
		float64(usage.CacheReadTokens)*b.price.CacheRead/1e6 +
		float64(usage.CacheCreationTokens)*b.price.Input/1e6
}

// check reports the limit the usage has passed, if any. The token budget comes
// first because it is the one the provider reported.
func (b usageBudget) check(usage Usage) error {
	if b.maxTokens > 0 && usage.TotalTokens() > b.maxTokens {
		return fmt.Errorf("%w: the session used %d tokens, over the review.max_total_tokens limit of %d",
			ErrBudgetExceeded, usage.TotalTokens(), b.maxTokens)
	}
	if b.maxCost > 0 && b.priced && b.cost(usage) > b.maxCost {
		return fmt.Errorf("%w: the session's estimated cost is $%.4f, over the review.max_cost_usd limit of $%.2f",
			ErrBudgetExceeded, b.cost(usage), b.maxCost)
	}
	return nil
}

// estimateCost is what the deck records for a finished session, nil when the
// operator configured no prices.
func (e *Engine) estimateCost(usage Usage, provider *config.Provider) *float64 {
	if provider == nil || !provider.HasPrice() {
		return nil
	}
	budget := usageBudget{price: provider.TokenPrice(), priced: true}
	cost := budget.cost(usage)
	return &cost
}
