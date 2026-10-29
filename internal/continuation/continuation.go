// Package continuation queues another attempt at a review session that stopped
// at its turn limit. The attempt is a job of its own, so the queue and the
// session records show it separately from the run it continues.
package continuation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/joangrigorov/rumil/internal/store"
)

// EventType is the job type a continuation is queued as. It is not a GitHub
// event: the admin surfaces queue it directly.
const EventType = "review_continuation"

// ErrAlreadyQueued reports that this session already has a continuation. The
// delivery id is derived from the session, so a second request cannot queue the
// same work twice.
var ErrAlreadyQueued = errors.New("the review session already has a continuation")

// Request is the payload of a continuation job.
type Request struct {
	SessionID string `json:"session_id"`
	// Turns is the new attempt's own step budget. Zero uses
	// app.max_agent_turns, including an unlimited configuration.
	Turns       int       `json:"turns"`
	RequestedBy string    `json:"requested_by"`
	RequestedAt time.Time `json:"requested_at"`
}

// NewRequest checks that the session can be continued and builds the request
// the job carries. turns is zero for the configured budget.
func NewRequest(parent store.ReviewSession, turns int, requestedBy string) (Request, error) {
	if err := parent.Continuable(); err != nil {
		return Request{}, err
	}
	if turns < 0 {
		return Request{}, fmt.Errorf("turns must be zero for the configured budget or positive, got %d", turns)
	}
	requestedBy = strings.TrimSpace(requestedBy)
	if requestedBy == "" {
		return Request{}, errors.New("requested_by is required to record who asked for the continuation")
	}
	return Request{
		SessionID:   parent.ID,
		Turns:       turns,
		RequestedBy: requestedBy,
		RequestedAt: time.Now().UTC(),
	}, nil
}

// DeliveryID is the idempotency key of a session's continuation. It is derived
// from the session so the queue refuses a second request: a session is
// continued once, and a continuation that fails is itself the session to
// continue next.
func DeliveryID(sessionID string) string {
	return "continuation:" + sessionID
}

// NewJob builds the queued job for a request. The coordinates are the parent
// session's, so the worker does not have to read the parent to know what it is
// reviewing.
func NewJob(parent store.ReviewSession, request Request) (store.Job, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return store.Job{}, fmt.Errorf("encoding the continuation request: %w", err)
	}
	return store.Job{
		DeliveryID:     DeliveryID(parent.ID),
		InstallationID: parent.InstallationID,
		EventType:      EventType,
		OrgName:        parent.OrgName,
		RepoName:       parent.RepoName,
		PRNumber:       parent.PRNumber,
		Payload:        payload,
	}, nil
}

// Queue validates the session, builds the job, and enqueues it.
func Queue(ctx context.Context, db *store.DB, parent store.ReviewSession, turns int, requestedBy string) (Request, error) {
	request, err := NewRequest(parent, turns, requestedBy)
	if err != nil {
		return Request{}, err
	}
	// A child session is the durable record of a continuation, so it is
	// checked before the job's delivery id: a job row can be pruned, and the
	// child is what says the attempt exists.
	if child, err := db.ReviewSessionContinuation(ctx, parent.ID); err != nil {
		return Request{}, err
	} else if child != nil {
		return Request{}, ErrAlreadyQueued
	}
	job, err := NewJob(parent, request)
	if err != nil {
		return Request{}, err
	}
	if err := db.Enqueue(ctx, job); err != nil {
		if errors.Is(err, store.ErrDuplicateDelivery) {
			return Request{}, ErrAlreadyQueued
		}
		return Request{}, err
	}
	return request, nil
}

// Parse reads a continuation job's payload.
func Parse(payload []byte) (Request, error) {
	var request Request
	if err := json.Unmarshal(payload, &request); err != nil {
		return Request{}, fmt.Errorf("decoding the continuation request: %w", err)
	}
	if request.SessionID == "" {
		return Request{}, errors.New("the continuation request names no session")
	}
	if request.Turns < 0 {
		return Request{}, fmt.Errorf("the continuation request has negative turns %d", request.Turns)
	}
	if strings.TrimSpace(request.RequestedBy) == "" {
		request.RequestedBy = "unknown"
	}
	if request.RequestedAt.IsZero() {
		request.RequestedAt = time.Now().UTC()
	}
	return request, nil
}
