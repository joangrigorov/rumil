// Package reviewer runs one pull request review through an embedded kit agent
// session whose only tools are Rumil's own tool set.
package reviewer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/joangrigorov/rumil/internal/config"
	"github.com/joangrigorov/rumil/internal/ghapp"
	"github.com/joangrigorov/rumil/internal/mcpservers"
	"github.com/joangrigorov/rumil/internal/store"
)

// providerWireOpenAICompat selects chat completions on the configured
// OpenAI-compatible endpoint.
const providerWireOpenAICompat = "openai-compat"

// Request identifies the pull request to review.
type Request struct {
	Owner  string
	Repo   string
	Number int
	// Observer, when set, receives the session's progress for the observation
	// deck. It travels with the request rather than the engine because workers
	// share one engine and run sessions concurrently.
	Observer Observer
}

// Result reports what the review produced.
type Result struct {
	// ReviewSubmitted is true when submit_pr_review reached GitHub.
	ReviewSubmitted bool
	// Event is the review event GitHub accepted.
	Event string
	// Fallback is true when a stronger event was resubmitted as COMMENT.
	Fallback bool
	// MovedToSummary counts comments that targeted lines outside the diff.
	MovedToSummary int
	// SkippedDuplicates counts comments that already existed on the pull request.
	SkippedDuplicates int
	// ToolCalls counts the tool calls the session executed.
	ToolCalls int
	// Steps counts the model steps the session took.
	Steps int
	// TurnLimitReached is true when the session used its whole turn budget
	// without submitting a review.
	TurnLimitReached bool
	// Summary is the closing text from the session.
	Summary string
}

// Engine reviews one pull request at a time.
type Engine struct {
	cfg    *config.Config
	logger *slog.Logger
	auth   *ghapp.AppAuth
	// memory is nil when the operator configured no embedding provider, which
	// is what makes memory opt-in (project_plan.md §3 rule 6).
	memory *memory
}

// NewEngine validates the GitHub App credentials and prepares the engine. db is
// the store pre-retrieval reads recorded rules from; it may be nil, and an
// engine built without it reviews on the prompt alone. baseURL overrides the
// GitHub API endpoint and is empty outside tests.
func NewEngine(cfg *config.Config, logger *slog.Logger, baseURL string, db *store.DB) (*Engine, error) {
	keyPEM, err := cfg.GitHub.ResolvePrivateKey()
	if err != nil {
		return nil, err
	}
	auth, err := ghapp.NewAppAuth(cfg.GitHub, keyPEM, baseURL)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	memory, err := newMemory(cfg, db, logger)
	if err != nil {
		return nil, err
	}

	return &Engine{cfg: cfg, logger: logger, auth: auth, memory: memory}, nil
}

// ErrHandleMismatch reports that app.bot_handle is not the App's own slug.
var ErrHandleMismatch = errors.New("app.bot_handle is not this App's slug")

// ErrNoReviewSubmitted reports that a session ended without reaching GitHub.
// A review that produced no review is a failure, not a successful run with a
// warning: nothing was said about the pull request.
var ErrNoReviewSubmitted = errors.New("review session ended without submitting a review")

// CheckApp holds the configuration against the App's own record and returns
// what the record says about the rest of it.
//
// The handle is what tells a mention in a comment apart from other text, what
// marks a review as Rumil's own among the reviews of a pull request, and what
// recognizes a review request aimed at Rumil. A handle that is not the slug
// therefore leaves the comment commands, the review requests and thread
// resolution inert while every other part of a run keeps working, which is why
// it stops a run rather than being trusted. The error wraps ErrHandleMismatch
// for a handle that disagrees, and anything else is a read that failed.
//
// The warnings are the parts of the configuration that GitHub will refuse
// later, in the middle of a round rather than at its start.
func (e *Engine) CheckApp(ctx context.Context) ([]string, error) {
	record, err := e.auth.Record(ctx)
	if err != nil {
		return nil, err
	}
	if !ghapp.SameLogin(record.Slug, e.cfg.App.BotHandle) {
		return nil, fmt.Errorf("%w: the configuration says %q and the App's slug is %q, so set app.bot_handle to %q",
			ErrHandleMismatch, e.cfg.App.BotHandle, record.Slug, record.Slug)
	}

	var warnings []string
	// Closing a review thread is refused for an installation whose Contents
	// permission is read, whatever its pull_requests permission says, and the
	// refusal arrives at the end of a round that has already been paid for. The
	// installation is what answers this, not the App: a permission raised on the
	// App does not reach the tokens until the installation's owner accepts it.
	if level := record.Permissions.GetContents(); level != "write" {
		warnings = append(warnings, fmt.Sprintf(
			"closing review threads will be refused: GitHub answers resolveReviewThread only when the installation's Contents permission is read and write, and installation %d's is %q. If the App's own settings already say write, the installation has not accepted the change yet, and its owner accepts it in the installation's settings",
			e.cfg.GitHub.InstallationID, level))
	}
	return warnings, nil
}

// ModelName is the chat model a review runs on, for the session record.
func (e *Engine) ModelName() string {
	provider, err := e.cfg.Models.ChatProvider()
	if err != nil {
		return ""
	}
	return provider.ModelName
}

// Review runs the review. The installation is verified before the session is
// created, so a missing GitHub App installation fails fast and cheap.
func (e *Engine) Review(ctx context.Context, req Request) (*Result, error) {
	observer := req.Observer
	if err := e.auth.Verify(ctx); err != nil {
		return nil, err
	}
	if e.cfg.GitHub.InstallationID <= 0 {
		return nil, errors.New("github.installation_id is required to review a pull request")
	}

	provider, err := e.cfg.Models.ChatProvider()
	if err != nil {
		return nil, fmt.Errorf("selecting the chat provider: %w", err)
	}

	githubClient := ghapp.NewClient(e.auth.Client(), req.Owner, req.Repo, req.Number)

	// Repository guidelines are read before the first turn, from the base SHA
	// unless the operator opted into trusting the branch under review
	// (project_plan.md §6 Phase 2).
	meta, err := githubClient.Metadata(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the pull request metadata: %w", err)
	}
	if observer != nil {
		observer.Observe(ctx, sessionMetadataEvent(meta.Title, meta.HeadSHA, meta.BaseSHA, len(meta.ChangedFiles)))
	}
	guidelinesRef := meta.BaseSHA
	if e.cfg.Review.GuidelinesRef == config.GuidelinesRefHead {
		guidelinesRef = meta.HeadSHA
	}
	guidelinesPath, guidelines, err := githubClient.ReviewGuidelines(ctx, guidelinesRef)
	if err != nil {
		return nil, fmt.Errorf("reading the repository review guidelines: %w", err)
	}
	if guidelinesPath != "" {
		e.logger.Info("loaded repository review guidelines",
			"path", guidelinesPath,
			"ref", guidelinesRef,
			"bytes", len(guidelines),
		)
	}
	if observer != nil {
		eventType := "guidelines_missing"
		if guidelinesPath != "" {
			eventType = "guidelines_loaded"
		}
		observer.Observe(ctx, Event{Type: eventType, At: time.Now(), Payload: map[string]any{
			"path": guidelinesPath, "ref": guidelinesRef, "bytes": len(guidelines),
		}})
	}

	var (
		mu     sync.Mutex
		result Result
	)

	// The history is read once, before the session can submit anything: the
	// block in the prompt and the baseline the session measures its own
	// reviews against have to be the same read, or a review could count
	// itself and close the threads it just opened (project_plan.md §7.1
	// rule 4, T-020-07).
	priorHistory, priorReviews := readPriorHistory(ctx, githubClient, e.cfg.App.BotHandle, e.logger)

	githubTools := mcpservers.NewGithubTools(githubClient, e.cfg.App.MaxDiffBytes, e.cfg.App.BotHandle, e.logger, priorReviews)
	githubTools.OnReviewSubmitted = func(outcome ghapp.ReviewOutcome) {
		mu.Lock()
		result.ReviewSubmitted = true
		result.Event = outcome.Event
		result.Fallback = outcome.Fallback
		result.MovedToSummary = len(outcome.MovedToSummary)
		result.SkippedDuplicates = len(outcome.SkippedDuplicates)
		mu.Unlock()

		if observer != nil {
			observer.Observe(ctx, Event{Type: "review_submitted", At: time.Now(), Payload: map[string]any{
				"event":              outcome.Event,
				"fallback":           outcome.Fallback,
				"moved_to_summary":   len(outcome.MovedToSummary),
				"skipped_duplicates": len(outcome.SkippedDuplicates),
			}})
		}
	}

	specs := githubTools.Specs()
	memoryBlock := ""
	memoryOn := e.memory != nil
	if e.memory != nil {
		// Pre-retrieval is best effort: an embedding provider that is down costs
		// this review its recorded rules, which is the state an operator who
		// never turned memory on is in, and is not worth failing a review over.
		// The tools stay registered either way: memory is configured, so the
		// session can still search, and a session that can search is told what
		// a recorded rule is.
		specs = append(specs, e.memory.tools(req)...)
		block, matches, retrieveErr := e.memory.retrieve(ctx, req, meta, guidelines != "")
		if retrieveErr != nil {
			e.logger.Warn("pre-retrieval of recorded rules failed, the review runs without them", "error", retrieveErr)
			if observer != nil {
				observer.Observe(ctx, Event{Type: "memory_failed", At: time.Now(), Payload: map[string]any{
					"error": retrieveErr.Error(),
				}})
			}
		} else {
			memoryBlock = block
			if observer != nil {
				observer.Observe(ctx, Event{Type: "memory_retrieved", At: time.Now(), Payload: map[string]any{
					"rules": len(matches), "bytes": len(block), "ids": memoryIDs(matches),
				}})
			}
		}
	}

	// review.resolve_threads off takes the tool away, and the prompt rule that
	// names it goes with it.
	resolveThreads := e.cfg.Review.ThreadsResolvable()
	if !resolveThreads {
		specs = slices.DeleteFunc(specs, func(spec mcpservers.Spec) bool { return spec.Name == "resolve_review_thread" })
	}

	sessionTools, err := kitTools(specs, func(string) {
		mu.Lock()
		defer mu.Unlock()
		result.ToolCalls++
	})
	if err != nil {
		return nil, fmt.Errorf("building the review tools: %w", err)
	}

	session, err := kit.New(ctx, e.sessionOptions(provider, SystemPrompt(SessionContext{
		Guidelines:     guidelines,
		Memories:       memoryBlock,
		MemoryOn:       memoryOn,
		PriorHistory:   priorHistory,
		ResolveThreads: resolveThreads,
	}), sessionTools, e.cfg.App.MaxTurns(), observer != nil))
	if err != nil {
		return nil, fmt.Errorf("starting the review session: %w", err)
	}
	stopSteps := session.OnPrepareStep(kit.HookPriorityNormal, func(step kit.PrepareStepHook) *kit.PrepareStepResult {
		mu.Lock()
		if step.StepNumber+1 > result.Steps {
			result.Steps = step.StepNumber + 1
		}
		mu.Unlock()
		return nil
	})
	defer stopSteps()
	stopCompaction := e.watchContext(ctx, session)
	defer stopCompaction()
	defer func() {
		if closeErr := session.Close(); closeErr != nil {
			e.logger.Warn("closing the review session failed", "error", closeErr)
		}
	}()
	if observer != nil {
		// The subscription goes before Prompt, which is what emits the
		// lifecycle: subscribing after it would only see a finished session.
		unsubscribe := session.Subscribe(func(event kit.Event) {
			e.observeKitEvent(ctx, observer, event)
		})
		defer unsubscribe()
	}

	text, promptErr := session.Prompt(ctx, ReviewUserPrompt(req.Owner, req.Repo, req.Number))

	mu.Lock()
	result.Summary = text
	mu.Unlock()

	if promptErr != nil {
		if observer != nil {
			observer.Observe(ctx, Event{Type: "session_error", At: time.Now(), Payload: map[string]any{
				"error": promptErr.Error(),
			}})
		}
		return &result, fmt.Errorf("running the review session for %s/%s#%d: %w", req.Owner, req.Repo, req.Number, promptErr)
	}

	if !result.ReviewSubmitted {
		// kit stops the tool loop at Options.MaxSteps and returns whatever text
		// the model produced. Nothing was said about the pull request, so this
		// is an error: the worker marks the job FAILED and the session FAILED
		// instead of reporting a successful review with no review in it.
		maxTurns := e.cfg.App.MaxTurns()
		result.TurnLimitReached = maxTurns > 0 && result.Steps >= maxTurns
		e.logger.Warn("review session ended without submitting a review",
			"steps", result.Steps,
			"tool_calls", result.ToolCalls,
			"max_agent_turns", maxTurns,
			"turn_limit_reached", result.TurnLimitReached,
		)
		limit := ""
		if result.TurnLimitReached {
			limit = fmt.Sprintf(" (turn limit %d reached)", maxTurns)
		}
		return &result, fmt.Errorf("%w for %s/%s#%d after %d steps and %d tool calls%s",
			ErrNoReviewSubmitted, req.Owner, req.Repo, req.Number, result.Steps, result.ToolCalls, limit)
	}
	return &result, nil
}

// watchContext compacts the session when its estimated context reaches the
// configured trigger. Kit's own AutoCompact reads the model's context window
// from its catalog; the OpenAI-compatible models Rumil is pointed at are
// usually absent there, so an unknown model would never trigger. Returning
// the unregister function keeps the hook scoped to this session.
func (e *Engine) watchContext(ctx context.Context, session *kit.Kit) func() {
	if !e.cfg.Compaction.CompactionEnabled() {
		return func() {}
	}
	return session.OnPrepareStep(kit.HookPriorityNormal, func(kit.PrepareStepHook) *kit.PrepareStepResult {
		e.compactIfNeeded(ctx, session)
		return nil
	})
}

// compactIfNeeded summarizes the older context once the session reaches the
// configured trigger. The context window handed to kit is the trigger plus the
// response reserve, which is the budget the summary has to fit back into.
func (e *Engine) compactIfNeeded(ctx context.Context, session *kit.Kit) {
	compaction := e.cfg.Compaction
	tokens := session.EstimateContextTokens()
	if tokens < compaction.TriggerTokens {
		return
	}

	result, err := session.Compact(ctx, &kit.CompactionOptions{
		ContextWindow:    compaction.TriggerTokens + compaction.ReserveTokens,
		ReserveTokens:    compaction.ReserveTokens,
		KeepRecentTokens: compaction.KeepRecentTokens,
	}, "")
	if err != nil {
		e.logger.Warn("compacting the session context failed", "error", err, "estimated_tokens", tokens)
		return
	}
	if result == nil {
		return
	}
	e.logger.Info("compacted the session context",
		"estimated_tokens", tokens,
		"original_tokens", result.OriginalTokens,
		"compacted_tokens", result.CompactedTokens,
		"messages_removed", result.MessagesRemoved,
	)
}

// sessionOptions builds the kit session configuration: isolated from host
// state, no core tools (no shell, no file writes), and only the tool set the
// caller passes. maxSteps bounds the tool loop.
func (e *Engine) sessionOptions(provider *config.Provider, systemPrompt string, tools []kit.Tool, maxSteps int, streaming bool) *kit.Options {
	options := &kit.Options{}
	kit.Isolated()(options)
	kit.WithModel(modelString(provider))(options)
	kit.WithSystemPrompt(systemPrompt)(options)
	// Streaming is what turns reasoning and text into events as they happen.
	// It is off without an observer, which keeps the one-off CLI path and the
	// existing tests on the non-streaming wire they were written against.
	kit.WithStreaming(streaming)(options)
	kit.WithProviderURL(provider.BaseURL)(options)
	kit.WithProviderAPIKey(provider.APIKey)(options)
	kit.WithProviderWire(providerWireOpenAICompat)(options)
	if provider.MaxTokens > 0 {
		kit.WithMaxTokens(provider.MaxTokens)(options)
	}
	if provider.Temperature != nil {
		temperature := float32(*provider.Temperature)
		options.Temperature = &temperature
	}

	options.MaxSteps = maxSteps
	options.Quiet = true
	options.ExtraTools = tools
	return options
}

func modelString(provider *config.Provider) string {
	return "openai/" + provider.ModelName
}
