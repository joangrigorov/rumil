// Package reviewer runs one pull request review through an embedded kit agent
// session whose only tools are Rumil's own tool set.
package reviewer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/joangrigorov/rumil/internal/config"
	"github.com/joangrigorov/rumil/internal/ghapp"
	"github.com/joangrigorov/rumil/internal/mcpservers"
	"github.com/joangrigorov/rumil/internal/repoconfig"
	"github.com/joangrigorov/rumil/internal/snapshot"
	"github.com/joangrigorov/rumil/internal/store"
)

// providerWireOpenAICompat selects chat completions on the configured
// OpenAI-compatible endpoint.
const providerWireOpenAICompat = "openai-compat"

// chatProvider is the instance provider factory. Kit's built-in
// OpenAI-compatible route has no place to set the HTTP client or a reasoning
// effort, so Rumil registers its own: the client bounds the wait for the
// response headers, and the effort rides the chat-completions wire.
const chatProvider = "rumil-openai-compat"

// stopReasonLength is the finish reason of a response cut off at the model's
// output token cap, which fantasy spells "length".
const stopReasonLength = "length"

// outputLimitNudge is the message a truncated session gets next. The cut-off
// turn kept no usable text, so the model has to be told what is missing and
// that the analysis it already did is enough for the tool call.
const outputLimitNudge = "Your last response hit the model output token limit before you called submit_pr_review, so no review was submitted. Do not repeat the analysis. Call submit_pr_review now with the findings you already have, and keep the call short."

// continueNudge is the message a session gets when a turn ends with text
// instead of the submit call. A slow provider can drop a turn back with no
// tool calls, and the review would otherwise fail with everything it learned.
const continueNudge = "Your last turn ended without calling submit_pr_review, so no review was posted. Continue from the work you have done: verify anything still open, then call submit_pr_review with your summary and inline comments. Do not end the turn before that call."

// Request identifies the pull request to review.
type Request struct {
	Owner  string
	Repo   string
	Number int
	// Focus is the guidance the person who asked for the review wrote after
	// the command, for example "focus on migration consistency". It narrows
	// where the effort goes; it does not replace the review.
	Focus string
	// SessionID is the recorded session, when one exists. It is what the check
	// run links to, so a reviewer can open the transcript from the pull
	// request.
	SessionID string
	// Observer, when set, receives the session's progress for the observation
	// deck. It travels with the request rather than the engine because workers
	// share one engine and run sessions concurrently.
	Observer Observer
	// Continuation, when set, resumes an earlier attempt that stopped at its
	// turn limit. It carries what the earlier attempt learned and this
	// attempt's own step budget.
	Continuation *Continuation
	// Cancelled reports that an operator asked the session to stop. The engine
	// checks it between steps and before a tool call; nil means nothing will
	// cancel this session.
	Cancelled func() bool
	// ProviderID, when set, names the chat provider this attempt runs on. It is
	// how a fallback attempt selects the next provider instead of the one the
	// operator or the repository chose.
	ProviderID string
	// InstallationID selects the GitHub App installation. Zero uses the
	// primary installation, or, when several are configured, the first one
	// that can read the repository.
	InstallationID int64
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
	// OutputLimitReached is true when a turn was cut off at the model's output
	// token cap without submitting a review, so the session asked again for
	// the tool call with a fresh budget.
	OutputLimitReached bool
	// Summary is the closing text from the session.
	Summary string
	// ReviewID is the review GitHub created, which a dismissal event names.
	ReviewID int64
	// Findings are the inline comments GitHub accepted, with their comment ids.
	Findings []ghapp.SubmittedFinding
	// ResolvedFindings are the root comment ids of threads this session closed.
	ResolvedFindings []int64
	// Usage is the tokens the session used, nil when the provider reported no
	// usage.
	Usage *Usage
	// ProviderID is the chat provider this attempt ran on.
	ProviderID string

	// headSHA is the commit this attempt reviewed, which a fallback carries so
	// the next attempt judges the same code.
	headSHA string
	// transcript is the compact tool history a fallback carries into its
	// continuation block.
	transcript []store.ReviewEvent
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

// ErrReviewCancelled reports that an operator stopped a session. A review that
// already reached GitHub is not cancelled: once it is posted, there is nothing
// left to stop.
var ErrReviewCancelled = errors.New("the review session was cancelled")

// ErrHeadMoved reports that a continuation's pull request is no longer at the
// commit the earlier attempt reviewed. Continuing would judge code whose diff
// the stored transcript does not describe.
var ErrHeadMoved = errors.New("the pull request moved since the earlier attempt")

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
	var warnings []string
	for _, installationID := range e.cfg.GitHub.InstallationIDs() {
		// Minting the token is what proves this installation belongs to the
		// App and is installed; the record read then says what it may do.
		if err := e.auth.VerifyInstallation(ctx, installationID); err != nil {
			return nil, err
		}
		record, err := e.auth.RecordFor(ctx, installationID)
		if err != nil {
			return nil, err
		}
		if !ghapp.SameLogin(record.Slug, e.cfg.App.BotHandle) {
			return nil, fmt.Errorf("%w: installation %d's App's slug is %q and the configuration says %q, so set app.bot_handle to %q",
				ErrHandleMismatch, installationID, record.Slug, e.cfg.App.BotHandle, record.Slug)
		}

		// Closing a review thread is refused for an installation whose Contents
		// permission is read, whatever its pull_requests permission says, and the
		// refusal arrives at the end of a round that has already been paid for. The
		// installation is what answers this, not the App: a permission raised on the
		// App does not reach the tokens until the installation's owner accepts it.
		if level := record.Permissions.GetContents(); level != "write" {
			warnings = append(warnings, fmt.Sprintf(
				"closing review threads will be refused: GitHub answers resolveReviewThread only when the installation's Contents permission is read and write, and installation %d's is %q. If the App's own settings already say write, the installation has not accepted the change yet, and its owner accepts it in the installation's settings",
				installationID, level))
		}
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

// Review runs the review, falling back to the next configured provider when
// the selected one fails before a review is submitted. The attempts share the
// same pull request, the same commit and the tool transcript the earlier
// attempt gathered, so a fallback finishes the review instead of starting it
// again. A review that already reached GitHub is never followed by another
// attempt.
func (e *Engine) Review(ctx context.Context, req Request) (*Result, error) {
	installationID, err := e.resolveInstallation(ctx, req.InstallationID, req.Owner, req.Repo)
	if err != nil {
		return nil, err
	}
	req.InstallationID = installationID

	used := map[string]bool{}
	for {
		result, err := e.reviewAttempt(ctx, req)
		if err == nil || result == nil {
			return result, err
		}
		used[result.ProviderID] = true
		if result.ReviewSubmitted || !isProviderFailure(err) {
			return result, err
		}
		nextID, nextModel := e.nextFallbackProvider(used)
		if nextID == "" {
			return result, err
		}

		e.logger.Warn("falling back to the next chat provider",
			"from", result.ProviderID,
			"to", nextID,
			"error", err,
		)
		if req.Observer != nil {
			req.Observer.Observe(ctx, Event{Type: "provider_fallback", At: time.Now(), Payload: map[string]any{
				"from":  result.ProviderID,
				"to":    nextID,
				"model": nextModel,
				"error": err.Error(),
			}})
		}

		req.ProviderID = nextID
		req.Continuation = &Continuation{
			ParentSessionID: req.SessionID,
			HeadSHA:         result.headSHA,
			Summary:         result.Summary,
			Events:          result.transcript,
			Reason:          ContinuationProvider,
		}
	}
}

// resolveInstallation picks the installation a request runs as. An explicit id
// is held against the configured set. Zero uses the only configured
// installation, and with several asks GitHub which one can read the
// repository: the CLI reaches one pull request at a time and has no delivery to
// say which account it belongs to.
func (e *Engine) resolveInstallation(ctx context.Context, requested int64, owner, repo string) (int64, error) {
	if requested > 0 {
		if !e.cfg.GitHub.HasInstallation(requested) {
			return 0, fmt.Errorf("%w: %d", ghapp.ErrInstallationNotAllowed, requested)
		}
		return requested, nil
	}

	ids := e.cfg.GitHub.InstallationIDs()
	if len(ids) == 0 {
		return 0, errors.New("github.installation_id or github.installations is required to review a pull request")
	}
	if len(ids) == 1 {
		return ids[0], nil
	}

	for _, id := range ids {
		client, err := e.auth.ClientFor(id)
		if err != nil {
			continue
		}
		if _, _, err := client.Repositories.Get(ctx, owner, repo); err == nil {
			return id, nil
		}
	}
	return 0, fmt.Errorf("no configured installation can read %s/%s", owner, repo)
}

// chatProviderFor resolves the chat provider for one installation. An
// installation may name its own provider; otherwise the operator default is
// used, and a request-level id still wins.
func (e *Engine) chatProviderFor(installationID int64, requested string) (*config.Provider, error) {
	if requested == "" {
		if installation := e.cfg.GitHub.InstallationByID(installationID); installation != nil {
			requested = installation.ChatProvider
		}
	}
	return e.cfg.Models.ChatProviderByID(requested)
}

// nextFallbackProvider returns the first configured fallback this review has
// not run on, in order.
func (e *Engine) nextFallbackProvider(used map[string]bool) (id, model string) {
	for _, candidate := range e.cfg.Models.Fallback {
		if used[candidate] {
			continue
		}
		provider, err := e.cfg.Models.ChatProviderByID(candidate)
		if err != nil {
			// Configuration validation refuses this at load time; a provider
			// removed since is skipped rather than stopping the review.
			continue
		}
		return candidate, provider.ModelName
	}
	return "", ""
}

// isProviderFailure reports whether an attempt failed in a way another provider
// may survive. Auth and invalid-request failures are not: every provider would
// answer the same request the same way.
func isProviderFailure(err error) bool {
	classified := kit.ClassifyProviderError(err)
	return errors.Is(classified, kit.ErrProviderUnavailable) || errors.Is(classified, kit.ErrRateLimit)
}

// touchesPath reports whether the pull request changes path. The metadata call
// already fetched the changed-file list, so detecting a RUMIL.md proposal costs
// no extra request.
func touchesPath(files []ghapp.ChangedFile, path string) bool {
	for _, file := range files {
		if file.Path == path {
			return true
		}
	}
	return false
}

// reviewAttempt runs the review on one provider. Review wraps it with the
// fallback chain.
func (e *Engine) reviewAttempt(ctx context.Context, req Request) (final *Result, err error) {
	observer := req.Observer
	if err := e.auth.VerifyInstallation(ctx, req.InstallationID); err != nil {
		return nil, err
	}
	authClient, err := e.auth.ClientFor(req.InstallationID)
	if err != nil {
		return nil, err
	}

	githubClient := ghapp.NewClient(authClient, req.Owner, req.Repo, req.Number)

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

	// The check run is the progress a reviewer sees without reading a comment
	// per turn. It is best effort: a missing Checks permission leaves the
	// review itself unchanged.
	checkID := e.startReviewCheck(ctx, req, meta.HeadSHA)
	defer func() {
		e.finishReviewCheck(ctx, req, checkID, final, err)
	}()

	// A continuation gets its own budget, which is what makes it a second
	// attempt rather than the same run resumed at zero.
	maxTurns := e.cfg.App.MaxTurns()
	if req.Continuation != nil {
		if req.Continuation.Turns > 0 {
			maxTurns = req.Continuation.Turns
		}
		if req.Continuation.HeadSHA != "" && meta.HeadSHA != req.Continuation.HeadSHA {
			return nil, fmt.Errorf("%w: the earlier attempt reviewed %s and the pull request is now at %s, so start a fresh review",
				ErrHeadMoved, shortCommit(req.Continuation.HeadSHA), shortCommit(meta.HeadSHA))
		}
		if observer != nil {
			observer.Observe(ctx, Event{Type: "continuation_start", At: time.Now(), Payload: map[string]any{
				"parent_session_id": req.Continuation.ParentSessionID,
				"max_steps":         maxTurns,
				"transcript_events": len(req.Continuation.Events),
			}})
		}
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

	// A pull request that adds or changes RUMIL.md does not get to rewrite the
	// policy judging it while the governing copy comes from the base SHA. The
	// reviewer still has to judge the proposed change, so the branch copy is
	// read separately and rendered as a proposal, never as the policy block.
	proposedGuidelines := ""
	if e.cfg.Review.GuidelinesRef != config.GuidelinesRefHead && touchesPath(meta.ChangedFiles, ghapp.GuidelinesPath) {
		_, proposed, proposedErr := githubClient.ReviewGuidelines(ctx, meta.HeadSHA)
		if proposedErr != nil {
			e.logger.Warn("reading the pull request's RUMIL.md failed; the review continues without the proposal",
				"error", proposedErr)
		} else if proposed != "" {
			proposedGuidelines = proposed
			e.logger.Info("loaded the pull request's proposed review guidelines",
				"path", ghapp.GuidelinesPath,
				"ref", meta.HeadSHA,
				"bytes", len(proposed),
			)
			if observer != nil {
				observer.Observe(ctx, Event{Type: "proposed_guidelines_loaded", At: time.Now(), Payload: map[string]any{
					"path": ghapp.GuidelinesPath, "ref": meta.HeadSHA, "bytes": len(proposed),
				}})
			}
		}
	}

	// The repository's own settings are read from the same ref as RUMIL.md,
	// which is the base branch unless the operator opted into the head. They
	// narrow this review only; an invalid file fails the review with its
	// problem rather than silently falling back to the operator's settings.
	repoSettings, repoFileFound, err := readRepositorySettings(ctx, githubClient, guidelinesRef, e.cfg.Models)
	if err != nil {
		return nil, err
	}
	// An installation may override the operator-wide budget. The repository's
	// settings narrow the review further but do not touch the budget.
	reviewConfig := e.cfg.Review
	installation := e.cfg.GitHub.InstallationByID(req.InstallationID)
	if installation != nil {
		if installation.MaxTotalTokens > 0 {
			reviewConfig.MaxTotalTokens = installation.MaxTotalTokens
		}
		if installation.MaxCostUSD > 0 {
			reviewConfig.MaxCostUSD = installation.MaxCostUSD
		}
	}
	effective := repoSettings.Apply(reviewConfig)

	// The provider is selected after the repository's settings are known: a
	// repository may name a cheaper model, and only the operator's configured
	// providers are candidates. An installation's own provider applies when the
	// repository is silent, and a fallback attempt names its provider
	// explicitly, which is how the chain advances.
	providerID := effective.ChatProviderID
	if providerID == "" && installation != nil {
		providerID = installation.ChatProvider
	}
	if req.ProviderID != "" {
		providerID = req.ProviderID
	}
	provider, err := e.cfg.Models.ChatProviderByID(providerID)
	if err != nil {
		return nil, fmt.Errorf("selecting the chat provider: %w", err)
	}

	if observer != nil {
		observer.Observe(ctx, Event{Type: "repository_settings", At: time.Now(), Payload: map[string]any{
			"file":                repoconfig.FileName,
			"found":               repoFileFound,
			"ref":                 guidelinesRef,
			"resolve_threads":     effective.ResolveThreads,
			"request_changes":     effective.RequestChanges,
			"ignore_paths":        effective.IgnorePaths,
			"default_focus":       effective.DefaultFocus,
			"max_inline_comments": effective.MaxInlineComments,
			"chat_provider":       provider.ID,
			"model":               provider.ModelName,
		}})
	}

	var (
		mu     sync.Mutex
		result Result
	)
	result.ProviderID = provider.ID
	result.headSHA = meta.HeadSHA
	// The transcript is what a fallback attempt inherits. It is captured even
	// when no fallback is configured, because the provider may fail on the
	// first attempt and the list is small.
	capture := newTranscriptCapture()
	defer func() { result.transcript = capture.events() }()

	// The history is read once, before the session can submit anything: the
	// block in the prompt and the baseline the session measures its own
	// reviews against have to be the same read, or a review could count
	// itself and close the threads it just opened (project_plan.md §7.1
	// rule 4, T-020-07).
	priorHistory, priorReviews := readPriorHistory(ctx, githubClient, e.cfg.App.BotHandle, e.logger)

	// The working tree is what makes listing, searching and reading local.
	// A snapshot that cannot be fetched is not fatal: the GitHub API tools
	// stay registered and the session reads the same commit through them.
	snap := e.openSnapshot(ctx, githubClient, meta.HeadSHA, observer)
	if snap != nil {
		defer func() {
			if closeErr := snap.Close(); closeErr != nil {
				e.logger.Warn("removing the repository snapshot failed", "error", closeErr)
			}
		}()
	}

	githubTools := mcpservers.NewGithubTools(githubClient, mcpservers.GithubToolsOptions{
		MaxDiffBytes: e.cfg.App.MaxDiffBytes,
		BotHandle:    e.cfg.App.BotHandle,
		Logger:       e.logger,
		PriorReviews: priorReviews,
		Snapshot:     snap,
		HeadSHA:      meta.HeadSHA,

		IgnorePath:             effective.Ignores,
		MaxInlineComments:      effective.MaxInlineComments,
		DisallowRequestChanges: !effective.RequestChanges,
	})
	githubTools.OnReviewSubmitted = func(outcome ghapp.ReviewOutcome) {
		mu.Lock()
		result.ReviewSubmitted = true
		result.Event = outcome.Event
		result.Fallback = outcome.Fallback
		result.MovedToSummary = len(outcome.MovedToSummary)
		result.SkippedDuplicates = len(outcome.SkippedDuplicates)
		result.ReviewID = outcome.ReviewID
		result.Findings = outcome.Findings
		mu.Unlock()

		if outcome.FindingsError != "" {
			e.logger.Warn("reading the submitted review's comments failed, finding outcomes will be incomplete",
				"error", outcome.FindingsError)
		}

		if observer != nil {
			observer.Observe(ctx, Event{Type: "review_submitted", At: time.Now(), Payload: map[string]any{
				"event":              outcome.Event,
				"fallback":           outcome.Fallback,
				"moved_to_summary":   len(outcome.MovedToSummary),
				"skipped_duplicates": len(outcome.SkippedDuplicates),
			}})
		}
	}
	githubTools.OnThreadResolved = func(commentID int64) {
		mu.Lock()
		result.ResolvedFindings = append(result.ResolvedFindings, commentID)
		mu.Unlock()
	}

	specs := githubTools.Specs()
	mcpSpecs, closeMCP := e.mcpTools(ctx, req, meta.HeadSHA, snap)
	defer closeMCP()
	specs = append(specs, mcpSpecs...)
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
	// names it goes with it. The repository's settings may narrow the operator's
	// choice further.
	resolveThreads := effective.ResolveThreads
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
		Guidelines:         guidelines,
		ProposedGuidelines: proposedGuidelines,
		Memories:           memoryBlock,
		MemoryOn:           memoryOn,
		PriorHistory:       priorHistory,
		ResolveThreads:     resolveThreads,
		Continuation:       ContinuationBlock(req.Continuation),
		LocalFiles:         snap != nil,
		MCPTools:           len(mcpSpecs) > 0,
		RepositorySettings: RepositorySettingsBlock(effective, repoFileFound),
	}), sessionTools, maxTurns, observer != nil))
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

	// The tracker sums what the provider reports per step. The budget reads it
	// between steps, so a session over its limit stops before paying for
	// another request.
	tracker := &usageTracker{}
	budget := e.newUsageBudget(provider, reviewConfig)
	if budget.enabled() {
		stopBudget := session.OnPrepareStep(kit.HookPriorityHigh, func(kit.PrepareStepHook) *kit.PrepareStepResult {
			mu.Lock()
			submitted := result.ReviewSubmitted
			mu.Unlock()
			if submitted {
				// A review that reached GitHub is finished; the budget has
				// nothing left to prevent.
				return nil
			}
			usage, reported := tracker.snapshot()
			if !reported {
				return nil
			}
			if err := budget.check(usage); err != nil {
				return &kit.PrepareStepResult{Error: err}
			}
			return nil
		})
		defer stopBudget()
	}
	defer func() {
		usage, reported := tracker.snapshot()
		if !reported {
			return
		}
		usage.CostUSD = e.estimateCost(usage, provider)
		mu.Lock()
		result.Usage = &usage
		mu.Unlock()
	}()

	// A cancellation stops the session between steps and keeps its tools from
	// running in the meantime. A review that already reached GitHub is not
	// cancelled: the operator asked to stop work that is still going, and
	// taking back a posted review is not this path.
	cancelled := func() bool {
		if req.Cancelled == nil || !req.Cancelled() {
			return false
		}
		mu.Lock()
		submitted := result.ReviewSubmitted
		mu.Unlock()
		return !submitted
	}
	if req.Cancelled != nil {
		stopCancelStep := session.OnPrepareStep(kit.HookPriorityNormal, func(kit.PrepareStepHook) *kit.PrepareStepResult {
			if !cancelled() {
				return nil
			}
			return &kit.PrepareStepResult{Error: ErrReviewCancelled}
		})
		defer stopCancelStep()
		stopCancelTool := session.OnBeforeToolCall(kit.HookPriorityHigh, func(kit.BeforeToolCallHook) *kit.BeforeToolCallResult {
			if !cancelled() {
				return nil
			}
			return &kit.BeforeToolCallResult{Block: true, Reason: "the review was cancelled"}
		})
		defer stopCancelTool()
	}

	stopCompaction := e.watchContext(ctx, session)
	defer stopCompaction()
	defer func() {
		if closeErr := session.Close(); closeErr != nil {
			e.logger.Warn("closing the review session failed", "error", closeErr)
		}
	}()
	// The subscription goes before Prompt, which is what emits the lifecycle:
	// subscribing after it would only see a finished session. It runs with or
	// without an observer, because the provider failures it collects are what
	// a no-review error names.
	issues := &providerIssues{}
	unsubscribe := session.Subscribe(func(event kit.Event) {
		issues.note(event)
		tracker.noteEvent(event)
		capture.note(event)
		if observer != nil {
			e.observeKitEvent(ctx, observer, event)
		}
	})
	defer unsubscribe()

	focus := req.Focus
	if focus == "" {
		// A repository may name the focus its reviews should take when the
		// person asking for one did not.
		focus = effective.DefaultFocus
	}
	userPrompt := ReviewUserPrompt(req.Owner, req.Repo, req.Number, focus)
	if req.Continuation != nil {
		userPrompt = ContinuationUserPrompt(req.Owner, req.Repo, req.Number)
	}
	recordTurn := func(turn *kit.TurnResult) {
		if turn == nil {
			return
		}
		mu.Lock()
		result.Summary = turn.Response
		mu.Unlock()
	}
	runPrompt := func(prompt string) (*kit.TurnResult, error) {
		turn, err := session.PromptResult(ctx, prompt)
		recordTurn(turn)
		if err != nil {
			if observer != nil {
				observer.Observe(ctx, Event{Type: "session_error", At: time.Now(), Payload: map[string]any{
					"error": err.Error(),
				}})
			}
			return turn, fmt.Errorf("running the review session for %s/%s#%d: %w", req.Owner, req.Repo, req.Number, err)
		}
		return turn, nil
	}
	turn, promptErr := runPrompt(userPrompt)
	if promptErr != nil {
		return &result, promptErr
	}

	// A turn that ends on the output cap was cut off mid-answer: the model
	// never reached the tool call. The conversation keeps no usable text from
	// that turn, so a second one says what to do with a fresh output budget.
	// A spent step budget has no room for it, and the turn-limit error below
	// names that cause instead.
	truncated := turn != nil && turn.StopReason == stopReasonLength
	result.OutputLimitReached = truncated
	hasBudget := maxTurns == 0 || result.Steps < maxTurns
	if !result.ReviewSubmitted && truncated && hasBudget {
		e.logger.Warn("the model reached its output limit without submitting a review; asking for the tool call",
			"steps", result.Steps,
			"tool_calls", result.ToolCalls,
		)
		if _, promptErr = runPrompt(outputLimitNudge); promptErr != nil {
			return &result, promptErr
		}
	}

	// A provider that stalls can hand the turn back with text and no tool
	// call. One more prompt costs one model request and turns that dead end
	// into a review; a second one would only spend the budget again, so the
	// no-review error below stays the final answer.
	if !result.ReviewSubmitted && !truncated && hasBudget {
		e.logger.Warn("the model ended its turn without submitting a review; asking it to continue",
			"steps", result.Steps,
			"tool_calls", result.ToolCalls,
		)
		if _, promptErr = runPrompt(continueNudge); promptErr != nil {
			return &result, promptErr
		}
	}

	if !result.ReviewSubmitted {
		// kit stops the tool loop at Options.MaxSteps and returns whatever text
		// the model produced. Nothing was said about the pull request, so this
		// is an error: the worker marks the job FAILED and the session FAILED
		// instead of reporting a successful review with no review in it.
		result.TurnLimitReached = maxTurns > 0 && result.Steps >= maxTurns
		e.logger.Warn("review session ended without submitting a review",
			"steps", result.Steps,
			"tool_calls", result.ToolCalls,
			"max_agent_turns", maxTurns,
			"turn_limit_reached", result.TurnLimitReached,
			"output_limit_reached", result.OutputLimitReached,
		)
		cause := ""
		switch {
		case result.TurnLimitReached:
			cause = fmt.Sprintf(" (turn limit %d reached)", maxTurns)
		case result.OutputLimitReached:
			cause = " (model output limit reached)"
		}
		if provider := issues.summary(); provider != "" {
			cause += fmt.Sprintf(" (the model provider reported: %s)", provider)
		}
		return &result, fmt.Errorf("%w for %s/%s#%d after %d steps and %d tool calls%s",
			ErrNoReviewSubmitted, req.Owner, req.Repo, req.Number, result.Steps, result.ToolCalls, cause)
	}
	return &result, nil
}

// openSnapshot materializes the reviewed commit for the session's read tools.
// A failure is a warning rather than an error: the session keeps the GitHub
// API tools and still reviews the same commit through them.
func (e *Engine) openSnapshot(ctx context.Context, client *ghapp.Client, head string, observer Observer) *snapshot.Snapshot {
	if !e.cfg.Snapshot.SnapshotEnabled() {
		return nil
	}
	snapCtx, cancel := context.WithTimeout(ctx, e.cfg.Snapshot.TimeoutOrDefault())
	defer cancel()

	archive, err := client.Archive(snapCtx, head)
	if err != nil {
		e.snapshotFailed(ctx, observer, err)
		return nil
	}
	defer func() { _ = archive.Close() }()

	snap, err := snapshot.Create(snapCtx, archive, snapshot.Options{
		MaxBytes: e.cfg.Snapshot.MaxBytesOrDefault(),
		Dir:      e.cfg.Snapshot.Dir,
	})
	if err != nil {
		e.snapshotFailed(ctx, observer, err)
		return nil
	}

	e.logger.Info("repository snapshot ready", "ref", shortCommit(head), "files", snap.Count())
	if observer != nil {
		observer.Observe(ctx, Event{Type: "snapshot_ready", At: time.Now(), Payload: map[string]any{
			"ref":   head,
			"files": snap.Count(),
		}})
	}
	return snap
}

func (e *Engine) snapshotFailed(ctx context.Context, observer Observer, err error) {
	e.logger.Warn("the repository snapshot could not be built; the review uses the GitHub API tools", "error", err)
	if observer != nil {
		observer.Observe(ctx, Event{Type: "snapshot_failed", At: time.Now(), Payload: map[string]any{
			"error": err.Error(),
		}})
	}
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
	// The custom factory is what bounds the wait for a provider's response
	// headers; without it a stalled request holds a worker for the whole job
	// timeout.
	options.Providers = map[string]kit.ProviderFactory{
		chatProvider: chatProviderFactory(provider),
	}
	kit.WithModel(chatProvider + "/" + provider.ModelName)(options)
	if provider.ThinkingLevel != "" {
		// The custom provider is what actually sends the effort on the
		// chat-completions wire; the option keeps kit's own view consistent.
		kit.WithThinkingLevel(provider.ThinkingLevel)(options)
	}
	// The default policy allows four attempts within two minutes, which a
	// two-minute header timeout would spend on one attempt. A few minutes of
	// headroom lets a provider that is briefly overloaded recover.
	kit.WithRetryPolicy(kit.RetryPolicy{
		MaxAttempts:  4,
		InitialDelay: time.Second,
		MaxDelay:     30 * time.Second,
		MaxElapsed:   5 * time.Minute,
	})(options)
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

// chatProviderFactory builds the OpenAI-compatible provider kit would build,
// with two additions: a client that gives up waiting for the response headers
// after the provider's request timeout, and the reasoning effort when one is
// configured.
func chatProviderFactory(provider *config.Provider) kit.ProviderFactory {
	return func(ctx context.Context, cfg *kit.ProviderConfig, modelName string) (*kit.ProviderResult, error) {
		built, err := openaicompat.New(
			openaicompat.WithBaseURL(cfg.ProviderURL),
			openaicompat.WithAPIKey(cfg.ProviderAPIKey),
			openaicompat.WithName(chatProvider),
			openaicompat.WithHTTPClient(boundedHTTPClient(provider.RequestTimeoutOrDefault())),
		)
		if err != nil {
			return nil, fmt.Errorf("building the chat provider: %w", err)
		}
		model, err := built.LanguageModel(ctx, modelName)
		if err != nil {
			return nil, fmt.Errorf("selecting model %q on the chat provider: %w", modelName, err)
		}
		result := &kit.ProviderResult{Model: model}
		if provider.ThinkingLevel != "" {
			result.ProviderOptions = fantasy.ProviderOptions{
				chatProvider: &openaicompat.ProviderOptions{
					ReasoningEffort: reasoningEffort(provider.ThinkingLevel),
				},
			}
		}
		return result, nil
	}
}

// boundedHTTPClient bounds the wait for a provider's response headers. The
// streamed body is not cut: a reasoning answer may take minutes to finish once
// it starts, and only a provider that never starts is the problem.
func boundedHTTPClient(timeout time.Duration) *http.Client {
	transport, ok := http.DefaultTransport.(*http.Transport)
	var bounded *http.Transport
	if ok {
		bounded = transport.Clone()
	} else {
		bounded = &http.Transport{}
	}
	bounded.ResponseHeaderTimeout = timeout
	return &http.Client{Transport: bounded}
}

// reasoningEffort maps a configured level onto the chat-completions effort.
// "off" returns nil, which leaves the provider's own default in place.
func reasoningEffort(level string) *openai.ReasoningEffort {
	var effort openai.ReasoningEffort
	switch level {
	case config.ThinkingNone:
		effort = openai.ReasoningEffortNone
	case config.ThinkingMinimal:
		effort = openai.ReasoningEffortMinimal
	case config.ThinkingLow:
		effort = openai.ReasoningEffortLow
	case config.ThinkingMedium:
		effort = openai.ReasoningEffortMedium
	case config.ThinkingHigh:
		effort = openai.ReasoningEffortHigh
	case config.ThinkingMax:
		effort = openai.ReasoningEffortMax
	default:
		return nil
	}
	return &effort
}
