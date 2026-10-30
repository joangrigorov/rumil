package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/joangrigorov/rumil/internal/metrics"
	"github.com/joangrigorov/rumil/internal/reviewer"
	"github.com/joangrigorov/rumil/internal/store"
)

// Bounds on the session transcript writer. The queue absorbs a burst of
// reasoning deltas; the flush interval is what a viewer sees as live latency.
const (
	eventQueueSize     = 2048
	eventFlushInterval = 50 * time.Millisecond
	eventFlushBatch    = 64
	eventWriteTimeout  = 10 * time.Second
)

// broker fans a live session's events out to SSE connections. Subscribers are
// per session, and a slow browser is allowed to miss events: it reconnects
// with Last-Event-ID and replays them from the table, which is the record that
// matters.
type broker struct {
	mu          sync.Mutex
	subscribers map[string]map[chan store.ReviewEvent]struct{}
}

func newBroker() *broker {
	return &broker{subscribers: make(map[string]map[chan store.ReviewEvent]struct{})}
}

// subscribe returns the channel and the function that removes it.
func (b *broker) subscribe(sessionID string) (<-chan store.ReviewEvent, func()) {
	channel := make(chan store.ReviewEvent, 256)

	b.mu.Lock()
	if b.subscribers[sessionID] == nil {
		b.subscribers[sessionID] = make(map[chan store.ReviewEvent]struct{})
	}
	b.subscribers[sessionID][channel] = struct{}{}
	b.mu.Unlock()

	return channel, func() {
		b.mu.Lock()
		defer b.mu.Unlock()

		if subscribers := b.subscribers[sessionID]; subscribers != nil {
			delete(subscribers, channel)
			if len(subscribers) == 0 {
				delete(b.subscribers, sessionID)
			}
		}
		close(channel)
	}
}

// publish sends to every subscriber without blocking. The event is already in
// the table by the time this runs, so a drop costs a live frame and never the
// record.
func (b *broker) publish(sessionID string, event store.ReviewEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for channel := range b.subscribers[sessionID] {
		select {
		case channel <- event:
		default:
		}
	}
}

// SessionRecorder turns a review into a stored session with a live stream. One
// recorder is shared by the worker pool; every call gets its own observer.
type SessionRecorder struct {
	db     *store.DB
	broker *broker
	logger *slog.Logger
	meter  *metrics.Metrics
}

// NewSessionRecorder builds the recorder over the same database the deck reads.
// meter may be nil, and observation then stays a trace with no metrics.
func NewSessionRecorder(db *store.DB, broker *broker, logger *slog.Logger, meter *metrics.Metrics) *SessionRecorder {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &SessionRecorder{db: db, broker: broker, logger: logger, meter: meter}
}

// Start records the session row and returns the observer to attach to the
// review request. A failure to record leaves the review runnable: observation
// is a trace, and a database that cannot take one does not fail the review.
func (r *SessionRecorder) Start(ctx context.Context, request store.NewReviewSession) (*sessionObserver, error) {
	session, err := r.db.CreateReviewSession(ctx, request)
	if err != nil {
		return nil, err
	}

	observer := &sessionObserver{
		db:        r.db,
		broker:    r.broker,
		logger:    r.logger,
		meter:     r.meter,
		sessionID: session.ID,
		events:    make(chan store.ReviewEvent, eventQueueSize),
		done:      make(chan struct{}),
	}
	go observer.write()
	return observer, nil
}

// sessionObserver implements reviewer.Observer. It assigns the session's seq,
// queues the event, and lets one writer flush batches so a burst of deltas
// never becomes one database round trip per token.
type sessionObserver struct {
	db        *store.DB
	broker    *broker
	logger    *slog.Logger
	meter     *metrics.Metrics
	sessionID string

	seq   atomic.Int64
	drops atomic.Int64
	// mu and closed make the queue's one close safe against a straggler event
	// from a tool goroutine.
	mu     sync.Mutex
	closed bool
	events chan store.ReviewEvent
	done   chan struct{}
}

// Observe records one event.
func (o *sessionObserver) Observe(_ context.Context, event reviewer.Event) {
	o.recordMetrics(event)

	payload, err := json.Marshal(event.Payload)
	if err != nil {
		o.logger.Warn("encoding a session event failed", "session_id", o.sessionID, "type", event.Type, "error", err)
		return
	}

	at := event.At
	if at.IsZero() {
		at = time.Now()
	}
	record := store.ReviewEvent{
		Seq:     int(o.seq.Add(1)),
		At:      at,
		Type:    event.Type,
		Payload: payload,
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return
	}

	select {
	case o.events <- record:
	default:
		// A delta is worth dropping under pressure; the next full message or
		// the terminal event carries the same content. A tool call, a review
		// outcome or an error is not, so it waits for the writer.
		if isDeltaEvent(event.Type) {
			o.drops.Add(1)
			return
		}
		o.events <- record
	}
}

// recordMetrics turns the events that describe a failure into counters. The
// payload carries the class, so no error string becomes a label.
func (o *sessionObserver) recordMetrics(event reviewer.Event) {
	if o.meter == nil {
		return
	}
	switch event.Type {
	case "step_finish":
		o.meter.ProviderRequest("ok")
	case "error":
		class, _ := event.Payload["class"].(string)
		if class == "" {
			class = "other"
		}
		o.meter.ProviderRequest(class)
		if timeout, _ := event.Payload["header_timeout"].(bool); timeout {
			o.meter.ProviderHeaderTimeout()
		}
	case "retry":
		o.meter.ProviderRetry()
	case "snapshot_failed":
		o.meter.SnapshotFallback()
	case "memory_failed":
		o.meter.MemoryFailure()
	}
}

// write drains the queue in order and publishes a batch only after it is
// stored, so a live frame and a replay carry the same seq.
func (o *sessionObserver) write() {
	defer close(o.done)

	ticker := time.NewTicker(eventFlushInterval)
	defer ticker.Stop()

	batch := make([]store.ReviewEvent, 0, eventFlushBatch)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), eventWriteTimeout)
		defer cancel()

		if err := o.db.AppendReviewEvents(ctx, o.sessionID, newEvents(batch)); err != nil {
			o.logger.Warn("recording review session events failed", "session_id", o.sessionID, "error", err)
		} else {
			for _, event := range batch {
				switch event.Type {
				case "session_metadata":
					o.updateMetadata(ctx, event)
				case "repository_settings", "provider_fallback":
					o.updateSessionModel(ctx, event)
				}
				o.broker.publish(o.sessionID, event)
			}
		}
		batch = batch[:0]
	}

	for {
		select {
		case event, ok := <-o.events:
			if !ok {
				flush()
				return
			}
			batch = append(batch, event)
			if len(batch) >= eventFlushBatch || !isDeltaEvent(event.Type) {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// updateMetadata fills the title and shas the engine learns after the session
// row exists, so the list can name the pull request without waiting for the
// review to finish.
func (o *sessionObserver) updateMetadata(ctx context.Context, event store.ReviewEvent) {
	var metadata struct {
		Title   string `json:"title"`
		HeadSHA string `json:"head_sha"`
		BaseSHA string `json:"base_sha"`
	}
	if err := json.Unmarshal(event.Payload, &metadata); err != nil {
		o.logger.Warn("decoding session metadata failed", "session_id", o.sessionID, "error", err)
		return
	}
	if err := o.db.UpdateReviewSessionMetadata(ctx, o.sessionID, metadata.Title, metadata.HeadSHA, metadata.BaseSHA); err != nil {
		o.logger.Warn("recording session metadata failed", "session_id", o.sessionID, "error", err)
	}
}

// updateSessionModel records the model the review actually runs on. A
// repository's settings may choose a provider of its own, and a provider
// failure may move the review to a fallback, both after the session row was
// written.
func (o *sessionObserver) updateSessionModel(ctx context.Context, event store.ReviewEvent) {
	var payload struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		o.logger.Warn("decoding the session model failed", "session_id", o.sessionID, "error", err)
		return
	}
	if err := o.db.UpdateReviewSessionModel(ctx, o.sessionID, payload.Model); err != nil {
		o.logger.Warn("recording the session model failed", "session_id", o.sessionID, "error", err)
	}
}

// Finish flushes the transcript, records the terminal event, moves the session
// to its outcome, and only then publishes session_end. A viewer that sees the
// end can therefore trust the summary it carries.
func (o *sessionObserver) Finish(ctx context.Context, outcome store.ReviewOutcome) {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return
	}
	o.closed = true
	close(o.events)
	o.mu.Unlock()

	<-o.done

	if dropped := o.drops.Load(); dropped > 0 {
		o.appendTerminal(ctx, "events_dropped", map[string]any{"count": dropped})
	}
	payload := map[string]any{
		"status":       outcome.Status,
		"summary":      outcome.Summary,
		"review_event": outcome.ReviewEvent,
		"tool_calls":   outcome.ToolCalls,
		"error":        outcome.Error,
		"finished_at":  time.Now().UTC(),
	}
	record := o.appendTerminal(ctx, "session_end", payload)

	if err := o.db.FinishReviewSession(ctx, o.sessionID, outcome); err != nil {
		o.logger.Warn("recording the review session outcome failed", "session_id", o.sessionID, "error", err)
	}
	o.broker.publish(o.sessionID, record)
}

// appendTerminal stores one final event and returns it for the broker.
func (o *sessionObserver) appendTerminal(ctx context.Context, eventType string, payload map[string]any) store.ReviewEvent {
	encoded, err := json.Marshal(payload)
	if err != nil {
		encoded = []byte(`{}`)
	}
	record := store.ReviewEvent{
		Seq:     int(o.seq.Add(1)),
		At:      time.Now(),
		Type:    eventType,
		Payload: encoded,
	}
	if err := o.db.AppendReviewEvents(ctx, o.sessionID, newEvents([]store.ReviewEvent{record})); err != nil {
		o.logger.Warn("recording the session end failed", "session_id", o.sessionID, "error", err)
	}
	return record
}

// isDeltaEvent reports the events that arrive once per token. They are the
// ones a busy queue may drop.
func isDeltaEvent(eventType string) bool {
	switch eventType {
	case "reasoning_delta", "message_delta", "tool_output", "tool_args_delta":
		return true
	default:
		return false
	}
}

// newEvents is the store's write shape for a flush.
func newEvents(events []store.ReviewEvent) []store.NewReviewEvent {
	out := make([]store.NewReviewEvent, len(events))
	for i, event := range events {
		out[i] = store.NewReviewEvent(event)
	}
	return out
}
