package store

import (
	"context"
	"fmt"
	"time"
)

// QueueState is what /metrics reports about the webhook queue. It is read at
// scrape time, so a restarted process reports the database rather than the
// counters it remembers.
type QueueState struct {
	Queued  int
	Running int
	// OldestQueued is the age of the oldest job still waiting, and zero when
	// the queue is empty.
	OldestQueued time.Duration
}

// SessionState is what /metrics reports about review sessions.
type SessionState struct {
	Running   int
	Succeeded int
	Failed    int
}

// QueueState reads the queue depth and the oldest waiting job in one query.
func (d *DB) QueueState(ctx context.Context) (QueueState, error) {
	var (
		state     QueueState
		oldestAge float64
	)
	err := d.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status = 'QUEUED'),
		       count(*) FILTER (WHERE status = 'RUNNING'),
		       coalesce(extract(epoch FROM now() - min(created_at) FILTER (WHERE status = 'QUEUED'))::double precision, 0)
		FROM webhook_jobs`).Scan(&state.Queued, &state.Running, &oldestAge)
	if err != nil {
		return QueueState{}, fmt.Errorf("reading the queue state: %w", err)
	}
	state.OldestQueued = time.Duration(oldestAge * float64(time.Second))
	return state, nil
}

// SessionState reads the session counts in one query.
func (d *DB) SessionState(ctx context.Context) (SessionState, error) {
	var state SessionState
	err := d.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status = 'RUNNING'),
		       count(*) FILTER (WHERE status = 'SUCCEEDED'),
		       count(*) FILTER (WHERE status = 'FAILED')
		FROM review_sessions`).Scan(&state.Running, &state.Succeeded, &state.Failed)
	if err != nil {
		return SessionState{}, fmt.Errorf("reading the session state: %w", err)
	}
	return state, nil
}
