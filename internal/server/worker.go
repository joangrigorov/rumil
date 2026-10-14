package server

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/joangrigorov/rumil/internal/reviewer"
	"github.com/joangrigorov/rumil/internal/store"
)

// pollInterval is how long an idle worker waits before looking for work.
const pollInterval = 250 * time.Millisecond

// reviewTimeout bounds one review. A hung model call would otherwise hold a
// worker for the life of the process.
const reviewTimeout = 15 * time.Minute

// Pool drains the webhook job queue with a fixed number of workers.
type Pool struct {
	jobs     *store.DB
	engine   *reviewer.Engine
	workers  int
	interval time.Duration
	timeout  time.Duration
	logger   *slog.Logger

	wg sync.WaitGroup
}

// NewPool builds a pool of concurrency workers.
func NewPool(jobs *store.DB, engine *reviewer.Engine, concurrency int, logger *slog.Logger) *Pool {
	if concurrency < 1 {
		concurrency = 1
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Pool{
		jobs:     jobs,
		engine:   engine,
		workers:  concurrency,
		interval: pollInterval,
		timeout:  reviewTimeout,
		logger:   logger,
	}
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
		"repo", job.OrgName+"/"+job.RepoName,
		"pr", job.PRNumber,
		"event_type", job.EventType,
	)

	if job.EventType != eventPullRequest {
		// A comment job carries no review: the @rumil command parser that
		// consumes it arrives in v0.2.0 (Phase 4).
		logger.Info("job needs no review in this phase")
		p.finish(ctx, logger, job, true, nil)
		return
	}

	// The review runs on its own bounded context so a shutdown stops new work
	// without cutting off the review already in flight.
	reviewCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.timeout)
	defer cancel()

	result, err := p.engine.Review(reviewCtx, reviewer.Request{
		Owner:  job.OrgName,
		Repo:   job.RepoName,
		Number: job.PRNumber,
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			// Leave the job RUNNING: the next start returns it to the queue.
			logger.Warn("review interrupted, the job returns to the queue on the next start", "error", err)
			return
		}
		logger.Error("review failed", "error", err)
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
