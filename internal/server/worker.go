package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/joangrigorov/rumil/internal/config"
	"github.com/joangrigorov/rumil/internal/continuation"
	"github.com/joangrigorov/rumil/internal/ghapp"
	"github.com/joangrigorov/rumil/internal/reviewer"
	"github.com/joangrigorov/rumil/internal/store"
)

// pollInterval is how long an idle worker waits before looking for work.
const pollInterval = 250 * time.Millisecond

// Pool drains the webhook job queue with a fixed number of workers.
type Pool struct {
	jobs     *store.DB
	engine   *reviewer.Engine
	botLogin string
	workers  int
	interval time.Duration
	timeout  time.Duration
	recorder *SessionRecorder
	logger   *slog.Logger

	wg sync.WaitGroup
}

// SetSessionRecorder attaches the observation deck's worker half. It is set
// once before Start; without one a review runs exactly as it did before, and
// nothing is stored.
func (p *Pool) SetSessionRecorder(recorder *SessionRecorder) {
	p.recorder = recorder
}

// PoolOption tunes a worker pool at construction.
type PoolOption func(*Pool)

// WithJobTimeout bounds one review or command job. A non-positive value keeps
// the default.
func WithJobTimeout(timeout time.Duration) PoolOption {
	return func(p *Pool) {
		if timeout > 0 {
			p.timeout = timeout
		}
	}
}

// NewPool builds a pool of concurrency workers. botHandle is the configured
// handle, which is what tells a command apart from a mention.
func NewPool(jobs *store.DB, engine *reviewer.Engine, botHandle string, concurrency int, logger *slog.Logger, opts ...PoolOption) *Pool {
	if concurrency < 1 {
		concurrency = 1
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	pool := &Pool{
		jobs:     jobs,
		engine:   engine,
		botLogin: ghapp.NormalizeLogin(botHandle),
		workers:  concurrency,
		interval: pollInterval,
		timeout:  config.DefaultWorkerTimeout,
		logger:   logger,
	}
	for _, opt := range opts {
		opt(pool)
	}
	return pool
}

// Start launches the workers. Cancelling ctx stops them claiming new jobs;
// Wait then blocks until the running reviews have stopped.
func (p *Pool) Start(ctx context.Context) {
	for i := 0; i < p.workers; i++ {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.work(ctx)
		}()
	}
}

// Wait blocks until every worker has returned.
func (p *Pool) Wait() {
	p.wg.Wait()
}

// work claims and processes jobs until ctx is cancelled.
func (p *Pool) work(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}

		job, err := p.jobs.ClaimJob(ctx)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return
			}
			p.logger.Error("claiming a job failed", "error", err)
			if !p.pause(ctx) {
				return
			}
		case job == nil:
			if !p.pause(ctx) {
				return
			}
		default:
			p.process(ctx, job)
		}
	}
}

// pause waits for the poll interval, reporting false when ctx is cancelled.
func (p *Pool) pause(ctx context.Context) bool {
	timer := time.NewTimer(p.interval)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// process runs one job and records its outcome.
func (p *Pool) process(ctx context.Context, job *store.Job) {
	logger := p.logger.With(
		"delivery_id", job.DeliveryID,
		"job_id", job.ID,
		"installation_id", job.InstallationID,
		"repo", job.OrgName+"/"+job.RepoName,
		"pr", job.PRNumber,
		"event_type", job.EventType,
	)

	switch job.EventType {
	case eventPullRequest:
		// The reaction goes on the pull request, because a review request
		// carries no comment to answer.
		p.acknowledgeReview(ctx, logger, reviewer.Request{
			Owner: job.OrgName, Repo: job.RepoName, Number: job.PRNumber,
		})
		p.review(ctx, logger, job, "")
	case eventIssueComment, eventReviewComment:
		p.command(ctx, logger, job)
	case continuation.EventType:
		p.continuation(ctx, logger, job)
	default:
		logger.Info("the event needs no work")
		p.finish(ctx, logger, job, true, nil)
	}
}

// command runs what a comment asked for: a review of the pull request, or a
// rule to record.
func (p *Pool) command(ctx context.Context, logger *slog.Logger, job *store.Job) {
	delivery, command, err := p.commandOf(job)
	if err != nil {
		logger.Error("reading the command out of the delivery failed", "error", err)
		p.finish(ctx, logger, job, false, err)
		return
	}

	// The reaction goes on the comment, and it goes first: it tells the person
	// the command was taken, which is what they are waiting for.
	p.acknowledgeCommand(ctx, logger, reviewer.RememberRequest{
		Owner: job.OrgName, Repo: job.RepoName, Number: job.PRNumber,
		CommentID: delivery.Comment.ID, Conversation: job.EventType == eventIssueComment,
	})

	switch command.Verb {
	case commandReview:
		// The words after the verb are the focus the person asked for.
		p.review(ctx, logger, job, command.Instruction)
	case commandRemember:
		p.remember(ctx, logger, job, delivery, command)
	default:
		// The webhook refuses a verb it does not know, so this is a job that
		// outlived the version that queued it.
		logger.Info("the command needs no work", "verb", command.Verb)
		p.finish(ctx, logger, job, true, nil)
	}
}

// acknowledgeReview reacts to a review request, best-effort: a reaction that
// could not be posted is not a review that cannot run.
func (p *Pool) acknowledgeReview(ctx context.Context, logger *slog.Logger, req reviewer.Request) {
	if err := p.engine.Acknowledge(ctx, req); err != nil {
		logger.Warn("acknowledging the review request failed", "error", err)
	}
}

// acknowledgeCommand reacts to the comment that carried a command, which is
// where the person who typed it is looking.
func (p *Pool) acknowledgeCommand(ctx context.Context, logger *slog.Logger, req reviewer.RememberRequest) {
	if err := p.engine.AcknowledgeCommand(ctx, req); err != nil {
		logger.Warn("acknowledging the command failed", "error", err)
	}
}

// review runs one review request. focus is the guidance a review command
// carried, and empty for a review requested as a reviewer.
func (p *Pool) review(ctx context.Context, logger *slog.Logger, job *store.Job, focus string) {
	// The review runs on its own bounded context so a shutdown stops new work
	// without cutting off the review already in flight.
	reviewCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.timeout)
	defer cancel()

	// The session is the trace of this run. A database that cannot take one
	// does not stop the review: observation is best effort.
	var recorded *sessionObserver
	var observer reviewer.Observer
	if p.recorder != nil {
		session, err := p.recorder.Start(reviewCtx, newReviewSession(job, p.engine.ModelName()))
		if err != nil {
			logger.Warn("recording the review session failed", "error", err)
		} else {
			recorded = session
			observer = session
		}
	}

	sessionID := ""
	if recorded != nil {
		sessionID = recorded.sessionID
	}

	result, err := p.engine.Review(reviewCtx, reviewer.Request{
		Owner:     job.OrgName,
		Repo:      job.RepoName,
		Number:    job.PRNumber,
		Focus:     focus,
		SessionID: sessionID,
		Observer:  observer,
	})
	if recorded != nil {
		finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		recorded.Finish(finishCtx, sessionOutcome(result, err))
		p.recordFindings(finishCtx, logger, recorded.sessionID, job.InstallationID,
			job.OrgName, job.RepoName, job.PRNumber, result)
		finishCancel()
	}
	if err != nil {
		if classifyFailure(err) == jobRetry {
			logger.Warn("review interrupted, the job returns to the queue on the next start", "error", err)
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			logger.Error("review exceeded its budget and will not be retried", "error", err, "timeout", p.timeout)
		} else {
			logger.Error("review failed", "error", err)
		}
		p.finish(ctx, logger, job, false, err)
		return
	}

	logger.Info("review finished",
		"review_submitted", result.ReviewSubmitted,
		"event", result.Event,
		"tool_calls", result.ToolCalls,
	)
	p.finish(ctx, logger, job, true, nil)
}

// continuation runs one admin-requested attempt at a session that stopped at
// its turn limit. The attempt is a session row of its own, pointing at the
// parent, so both records show it as a separate run.
func (p *Pool) continuation(ctx context.Context, logger *slog.Logger, job *store.Job) {
	request, err := continuation.Parse(job.Payload)
	if err != nil {
		logger.Error("reading the continuation request failed", "error", err)
		p.finish(ctx, logger, job, false, err)
		return
	}

	parent, err := p.jobs.ReviewSessionByID(ctx, request.SessionID)
	if errors.Is(err, store.ErrReviewSessionNotFound) {
		err = fmt.Errorf("review session %s does not exist", request.SessionID)
	}
	if err != nil {
		logger.Error("reading the session to continue failed", "session_id", request.SessionID, "error", err)
		p.finish(ctx, logger, job, false, err)
		return
	}
	// The check is repeated here because the session may have submitted a
	// review between the admin asking and a worker starting.
	if err := parent.Continuable(); err != nil {
		logger.Warn("refused a continuation", "session_id", parent.ID, "error", err)
		p.finish(ctx, logger, job, false, err)
		return
	}

	events, err := p.jobs.ReviewTranscriptEvents(ctx, parent.ID, store.ReviewTranscriptMax)
	if err != nil {
		logger.Error("reading the session transcript failed", "session_id", parent.ID, "error", err)
		p.finish(ctx, logger, job, false, err)
		return
	}

	continuationCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.timeout)
	defer cancel()

	var recorded *sessionObserver
	var observer reviewer.Observer
	if p.recorder != nil {
		session, startErr := p.recorder.Start(continuationCtx, store.NewReviewSession{
			JobID:           job.ID,
			InstallationID:  parent.InstallationID,
			OrgName:         parent.OrgName,
			RepoName:        parent.RepoName,
			PRNumber:        parent.PRNumber,
			Model:           p.engine.ModelName(),
			ParentSessionID: parent.ID,
			RequestedBy:     request.RequestedBy,
			RequestedAt:     &request.RequestedAt,
		})
		if startErr != nil {
			logger.Warn("recording the continuation session failed", "error", startErr)
		} else {
			recorded = session
			observer = session
		}
	}

	sessionID := ""
	if recorded != nil {
		sessionID = recorded.sessionID
	}

	result, reviewErr := p.engine.Review(continuationCtx, reviewer.Request{
		Owner:     parent.OrgName,
		Repo:      parent.RepoName,
		Number:    parent.PRNumber,
		SessionID: sessionID,
		Observer:  observer,
		Continuation: &reviewer.Continuation{
			ParentSessionID: parent.ID,
			HeadSHA:         parent.HeadSHA,
			Summary:         parent.Summary,
			Events:          events,
			Turns:           request.Turns,
		},
	})
	if recorded != nil {
		finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		recorded.Finish(finishCtx, sessionOutcome(result, reviewErr))
		p.recordFindings(finishCtx, logger, recorded.sessionID, parent.InstallationID,
			parent.OrgName, parent.RepoName, parent.PRNumber, result)
		finishCancel()
	}
	if reviewErr != nil {
		if classifyFailure(reviewErr) == jobRetry {
			logger.Warn("continuation interrupted, the job returns to the queue on the next start", "error", reviewErr)
			return
		}
		if errors.Is(reviewErr, context.DeadlineExceeded) {
			logger.Error("continuation exceeded its budget and will not be retried", "error", reviewErr, "timeout", p.timeout)
		} else {
			logger.Error("continuation failed", "session_id", parent.ID, "error", reviewErr)
		}
		p.finish(ctx, logger, job, false, reviewErr)
		return
	}

	logger.Info("continuation finished",
		"parent_session_id", parent.ID,
		"requested_by", request.RequestedBy,
		"review_submitted", result.ReviewSubmitted,
		"event", result.Event,
		"tool_calls", result.ToolCalls,
	)
	p.finish(ctx, logger, job, true, nil)
}

// newReviewSession is what a review job knows when its session starts.
func newReviewSession(job *store.Job, model string) store.NewReviewSession {
	return store.NewReviewSession{
		JobID:          job.ID,
		InstallationID: job.InstallationID,
		OrgName:        job.OrgName,
		RepoName:       job.RepoName,
		PRNumber:       job.PRNumber,
		Model:          model,
	}
}

// recordFindings stores the inline comments a submitted review posted, which is
// what later feedback is attached to. It is best effort: a row that cannot be
// written leaves the review unchanged and only the report incomplete.
func (p *Pool) recordFindings(ctx context.Context, logger *slog.Logger, sessionID string, installationID int64,
	orgName, repoName string, number int, result *reviewer.Result) {
	if sessionID == "" || result == nil {
		return
	}
	if len(result.Findings) > 0 {
		findings := make([]store.Finding, 0, len(result.Findings))
		for _, finding := range result.Findings {
			findings = append(findings, store.Finding{
				SessionID:      sessionID,
				InstallationID: installationID,
				OrgName:        orgName,
				RepoName:       repoName,
				PRNumber:       number,
				ReviewID:       result.ReviewID,
				CommentID:      finding.CommentID,
				Path:           finding.Path,
				Line:           finding.Line,
				Body:           finding.Body,
			})
		}
		if err := p.jobs.RecordFindings(ctx, findings); err != nil {
			logger.Warn("recording the review's findings failed, outcome tracking will be incomplete", "error", err)
		}
	}
	if len(result.ResolvedFindings) > 0 {
		if err := p.jobs.MarkFindingsResolved(ctx, result.ResolvedFindings); err != nil {
			logger.Warn("recording the resolved findings failed, outcome tracking will be incomplete", "error", err)
		}
	}
}

// sessionOutcome is what the deck records for a finished review. A review the
// engine interrupted is FAILED with its cause, so a session row cannot stay
// RUNNING after the worker has stopped working on it.
func sessionOutcome(result *reviewer.Result, err error) store.ReviewOutcome {
	outcome := store.ReviewOutcome{Status: store.ReviewSucceeded}
	if result != nil {
		// A failed session keeps its partial text and tool count so the deck
		// shows what the run did before it stopped, even though the status is
		// FAILED. The turn-limit marker is what the surfaces read to offer a
		// continuation.
		outcome.Summary = result.Summary
		outcome.ReviewEvent = result.Event
		outcome.ToolCalls = result.ToolCalls
		outcome.TurnLimitReached = result.TurnLimitReached
	}
	if err != nil {
		outcome.Status = store.ReviewFailed
		outcome.Error = err.Error()
	}
	return outcome
}

// remember distills the command a comment carried into a rule and answers the
// comment.
func (p *Pool) remember(ctx context.Context, logger *slog.Logger, job *store.Job, delivery payload, command Command) {
	if !p.engine.MemoryConfigured() {
		// Memory is opt-in, so a command that arrives while it is off is a
		// configuration state rather than a failure: the job is completed with a
		// warning instead of being marked FAILED.
		logger.Warn("ignoring a command because memory is not configured")
		p.finish(ctx, logger, job, true, nil)
		return
	}

	req, err := p.rememberRequest(job, delivery, command)
	if err != nil {
		logger.Error("reading the command out of the delivery failed", "error", err)
		p.finish(ctx, logger, job, false, err)
		return
	}

	commandCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.timeout)
	defer cancel()

	result, err := p.engine.Remember(commandCtx, req)
	if err != nil {
		if classifyFailure(err) == jobRetry {
			// Leave the job RUNNING: the next start returns it to the queue, and
			// the short id in the answer is what stops it answering twice.
			logger.Warn("command interrupted, the job returns to the queue on the next start", "error", err)
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			logger.Error("the command exceeded its budget and will not be retried", "error", err, "timeout", p.timeout)
		} else {
			logger.Error("recording the rule failed", "error", err)
		}
		p.finish(ctx, logger, job, false, err)
		return
	}

	logger.Info("command handled",
		"proposed", result.Proposed,
		"inserted", result.Inserted,
		"memory", result.Memory.ID,
		"status", result.Memory.Status,
		"replied", result.Replied,
		"requested_by", req.RequestedBy,
	)
	p.finish(ctx, logger, job, true, nil)
}

// commandOf reads the comment and the command back out of the delivery the
// webhook stored. The payload is the signed body GitHub sent, so the fields the
// webhook classified on are the ones read here.
func (p *Pool) commandOf(job *store.Job) (payload, Command, error) {
	var delivery payload
	if err := json.Unmarshal(job.Payload, &delivery); err != nil {
		return payload{}, Command{}, fmt.Errorf("decoding delivery %s: %w", job.DeliveryID, err)
	}
	if delivery.Comment == nil {
		return payload{}, Command{}, fmt.Errorf("delivery %s carries no comment", job.DeliveryID)
	}

	command, ok := parseCommand(p.botLogin, delivery.Comment.Body)
	if !ok {
		return payload{}, Command{}, fmt.Errorf("delivery %s carries no @%s command", job.DeliveryID, p.botLogin)
	}
	return delivery, command, nil
}

// rememberRequest reads the context a remembered rule is recorded with.
func (p *Pool) rememberRequest(job *store.Job, delivery payload, command Command) (reviewer.RememberRequest, error) {
	if command.Verb != commandRemember || command.Instruction == "" {
		return reviewer.RememberRequest{}, fmt.Errorf("delivery %s no longer parses as a %s command", job.DeliveryID, commandRemember)
	}

	source := delivery.Comment.HTMLURL
	if source == "" {
		source = fmt.Sprintf("%s/%s#%d comment %d", job.OrgName, job.RepoName, job.PRNumber, delivery.Comment.ID)
	}

	return reviewer.RememberRequest{
		Owner:             job.OrgName,
		Repo:              job.RepoName,
		Number:            job.PRNumber,
		Command:           command.Instruction,
		OrgScoped:         command.Org,
		CommentID:         delivery.Comment.ID,
		InReplyToID:       delivery.Comment.InReplyToID,
		Conversation:      job.EventType == eventIssueComment,
		Path:              delivery.Comment.Path,
		DiffHunk:          delivery.Comment.DiffHunk,
		RequestedBy:       delivery.Comment.User.Login,
		AuthorAssociation: delivery.Comment.AuthorAssociation,
		SourceComment:     source,
	}, nil
}

// jobFailure is what a failed job means for the queue.
type jobFailure int

const (
	// jobRetry means the job was stopped rather than broken: the process is
	// shutting down, and the next start returns the job to the queue.
	jobRetry jobFailure = iota
	// jobFailed means the job is recorded FAILED with its cause and is not run
	// again.
	jobFailed
)

// classifyFailure tells those two apart. The job's work runs on
// context.WithoutCancel, so a cancellation is the process shutting down, while
// the job's own deadline is the work spending its budget: a job that spent it
// cannot finish on a retry, and leaving it RUNNING would requeue it on every
// restart, burning a review's tokens each time and never recording why.
func classifyFailure(err error) jobFailure {
	if errors.Is(err, context.DeadlineExceeded) {
		return jobFailed
	}
	if errors.Is(err, context.Canceled) {
		return jobRetry
	}
	return jobFailed
}

// finish records the job outcome on a context that survives a shutdown, so a
// completed review is never left looking unfinished.
func (p *Pool) finish(ctx context.Context, logger *slog.Logger, job *store.Job, ok bool, cause error) {
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	var err error
	if ok {
		err = p.jobs.CompleteJob(finishCtx, job.ID)
	} else {
		err = p.jobs.FailJob(finishCtx, job.ID, cause)
	}
	if err != nil {
		logger.Error("recording the job outcome failed", "error", err)
	}
}
