// Package reviewer runs one pull request review through an embedded kit agent
// session whose only tools are Rumil's own tool set.
package reviewer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/joangrigorov/rumil/internal/config"
	"github.com/joangrigorov/rumil/internal/ghapp"
	"github.com/joangrigorov/rumil/internal/mcpservers"
)

// providerWireOpenAICompat selects chat completions on the configured
// OpenAI-compatible endpoint.
const providerWireOpenAICompat = "openai-compat"

// Request identifies the pull request to review.
type Request struct {
	Owner  string
	Repo   string
	Number int
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
}

// NewEngine validates the GitHub App credentials and prepares the engine.
// baseURL overrides the GitHub API endpoint and is empty outside tests.
func NewEngine(cfg *config.Config, logger *slog.Logger, baseURL string) (*Engine, error) {
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

	return &Engine{cfg: cfg, logger: logger, auth: auth}, nil
}

// Review runs the review. The installation is verified before the session is
// created, so a missing GitHub App installation fails fast and cheap.
func (e *Engine) Review(ctx context.Context, req Request) (*Result, error) {
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

	var (
		mu     sync.Mutex
		result Result
	)
	githubTools := mcpservers.NewGithubTools(githubClient, e.cfg.App.MaxDiffBytes, e.logger)
	githubTools.OnReviewSubmitted = func(outcome ghapp.ReviewOutcome) {
		mu.Lock()
		defer mu.Unlock()
		result.ReviewSubmitted = true
		result.Event = outcome.Event
		result.Fallback = outcome.Fallback
		result.MovedToSummary = len(outcome.MovedToSummary)
		result.SkippedDuplicates = len(outcome.SkippedDuplicates)
	}

	sessionTools, err := kitTools(githubTools.Specs(), func(string) {
		mu.Lock()
		defer mu.Unlock()
		result.ToolCalls++
	})
	if err != nil {
		return nil, fmt.Errorf("building the review tools: %w", err)
	}

	session, err := kit.New(ctx, e.sessionOptions(provider, sessionTools))
	if err != nil {
		return nil, fmt.Errorf("starting the review session: %w", err)
	}
	defer func() {
		if closeErr := session.Close(); closeErr != nil {
			e.logger.Warn("closing the review session failed", "error", closeErr)
		}
	}()

	text, promptErr := session.Prompt(ctx, ReviewUserPrompt(req.Owner, req.Repo, req.Number))

	mu.Lock()
	result.Summary = text
	mu.Unlock()

	if promptErr != nil {
		return &result, fmt.Errorf("running the review session for %s/%s#%d: %w", req.Owner, req.Repo, req.Number, promptErr)
	}

	if !result.ReviewSubmitted {
		// kit stops the tool loop at Options.MaxSteps and returns whatever text
		// the model produced, so the engine detects the exhausted budget here
		// and reports it instead of failing the container.
		result.TurnLimitReached = result.ToolCalls >= e.cfg.App.MaxAgentTurns
		e.logger.Warn("review session ended without submitting a review",
			"tool_calls", result.ToolCalls,
			"max_agent_turns", e.cfg.App.MaxAgentTurns,
			"turn_limit_reached", result.TurnLimitReached,
		)
	}
	return &result, nil
}

// sessionOptions builds the kit session configuration: isolated from host
// state, no core tools (no shell, no file writes), and only the GitHub tool set
// registered.
func (e *Engine) sessionOptions(provider *config.Provider, tools []kit.Tool) *kit.Options {
	options := &kit.Options{}
	kit.Isolated()(options)
	kit.WithModel(modelString(provider))(options)
	kit.WithSystemPrompt(PRReviewerSystemPrompt)(options)
	kit.WithStreaming(false)(options)
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

	options.MaxSteps = e.cfg.App.MaxAgentTurns
	options.Quiet = true
	options.ExtraTools = tools
	return options
}

func modelString(provider *config.Provider) string {
	return "openai/" + provider.ModelName
}
