package server

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/joangrigorov/rumil/internal/store"
)

// cancelPollInterval is how often a running job reads its cancel flag. The
// engine asks an atomic bool on every step and tool call, so the read is cheap
// and the delay to notice a click is at most this interval.
const cancelPollInterval = 500 * time.Millisecond

// cancelWatcher follows one job's cancel_requested flag. A database read per
// model step would be a round trip in the hot path, and a per-process map would
// miss a cancellation from another server; one poll per interval is the middle
// ground.
type cancelWatcher struct {
	cancelled atomic.Bool
	stop      chan struct{}
	done      chan struct{}
}

// newCancelWatcher starts polling the job until Close. A poll that fails is
// logged once and retried: a database hiccup must not cancel a review.
func newCancelWatcher(ctx context.Context, jobs *store.DB, jobID string, logger *slog.Logger) *cancelWatcher {
	watcher := &cancelWatcher{stop: make(chan struct{}), done: make(chan struct{})}
	if jobs == nil || jobID == "" {
		close(watcher.done)
		return watcher
	}

	go func() {
		defer close(watcher.done)
		ticker := time.NewTicker(cancelPollInterval)
		defer ticker.Stop()

		lastErr := ""
		for {
			select {
			case <-ctx.Done():
				return
			case <-watcher.stop:
				return
			case <-ticker.C:
				requested, err := jobs.JobCancelRequested(ctx, jobID)
				if err != nil {
					if err.Error() != lastErr {
						logger.Warn("reading the cancel flag failed", "job_id", jobID, "error", err)
						lastErr = err.Error()
					}
					continue
				}
				if requested {
					watcher.cancelled.Store(true)
					return
				}
			}
		}
	}()
	return watcher
}

// Stopped reports whether the operator asked the job to stop.
func (w *cancelWatcher) Stopped() bool {
	return w.cancelled.Load()
}

// Close stops the poller and waits for it to return.
func (w *cancelWatcher) Close() {
	select {
	case <-w.done:
		return
	default:
	}
	close(w.stop)
	<-w.done
}
