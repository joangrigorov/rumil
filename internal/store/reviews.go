package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// Review status values of the review_status enum.
const (
	ReviewRunning   = "RUNNING"
	ReviewSucceeded = "SUCCEEDED"
	ReviewFailed    = "FAILED"
)

// Bounds on the observation deck's reads. A session list is a page, and a
// replay is a batch: both are bounded so a long session cannot pull the whole
// table into one response.
const (
	ReviewListDefault   = 50
	ReviewListMax       = 200
	ReviewEventsDefault = 500
	ReviewEventsMax     = 2000
	// ReviewTranscriptMax bounds the tool calls and results a continuation
	// carries into its prompt. The reads are newest first, so a session that
	// emitted more than this keeps the part it was working on when it stopped.
	ReviewTranscriptMax = 200
)

// ErrReviewSessionNotFound reports a session id that is not in the table.
var ErrReviewSessionNotFound = errors.New("review session not found")

// Reasons a session cannot be given a continuation attempt.
var (
	// ErrReviewAlreadySubmitted reports a session that said something about the
	// pull request. Continuing it would submit a second review over the first.
	ErrReviewAlreadySubmitted = errors.New("the review session already submitted a review")
	// ErrReviewNotFailed reports a session that is still running.
	ErrReviewNotFailed = errors.New("only a failed review session can be continued")
)

// TokenUsage is what one review session was billed for. Cache tokens are kept
// separate so the deck can show how much of the input came from a cache.
type TokenUsage struct {
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	// CostUSD is the estimated cost, nil when the provider has no prices.
	CostUSD *float64
}

// TotalTokens sums every category.
func (u TokenUsage) TotalTokens() int64 {
	return u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheCreationTokens
}

// UsageTotals aggregates the sessions a list shows.
type UsageTotals struct {
	// Sessions is how many sessions the filter matches, and Reported how many
	// of them carried usage. The two differ when a provider reports nothing.
	Sessions            int
	Reported            int
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	// CostUSD is the sum of the recorded estimates, and CostReported how many
	// sessions carried one.
	CostUSD      float64
	CostReported int
}

// TotalTokens sums every category.
func (t UsageTotals) TotalTokens() int64 {
	return t.InputTokens + t.OutputTokens + t.CacheReadTokens + t.CacheCreationTokens
}

// ReviewSession is one run of the reviewer.
type ReviewSession struct {
	ID             string
	JobID          string
	InstallationID int64
	OrgName        string
	RepoName       string
	PRNumber       int
	PRTitle        string
	HeadSHA        string
	BaseSHA        string
	Model          string
	Status         string
	Summary        string
	ReviewEvent    string
	ToolCalls      int
	Error          string
	// ParentSessionID is the attempt this session continues, empty for a first
	// attempt at a pull request.
	ParentSessionID string
	// TurnLimitReached records that the session spent its whole step budget
	// without submitting a review, which is the failure a continuation fixes.
	TurnLimitReached bool
	// RequestedBy and RequestedAt are who asked for a continuation and when.
	// They are empty on a first attempt.
	RequestedBy string
	RequestedAt *time.Time
	// Usage is what the provider reported, nil when it reported nothing.
	Usage      *TokenUsage
	StartedAt  time.Time
	FinishedAt *time.Time
}

// NewReviewSession is what a worker knows when a review starts.
type NewReviewSession struct {
	JobID          string
	InstallationID int64
	OrgName        string
	RepoName       string
	PRNumber       int
	Model          string
	// ParentSessionID, RequestedBy and RequestedAt are set on a continuation.
	ParentSessionID string
	RequestedBy     string
	RequestedAt     *time.Time
}

// ReviewOutcome is how a review ended.
type ReviewOutcome struct {
	Status      string
	Summary     string
	ReviewEvent string
	ToolCalls   int
	Error       string
	// TurnLimitReached says the failure was the step budget rather than a
	// provider or tool error.
	TurnLimitReached bool
	// Usage is what the provider reported for the session, nil when it
	// reported nothing.
	Usage *TokenUsage
}

// Continuable reports whether the session failed without submitting a review
// and can be given another attempt. A session that already submitted a review
// is refused whatever its status: a continuation would submit a second review
// over it.
func (s ReviewSession) Continuable() error {
	if s.Status == ReviewSucceeded || s.ReviewEvent != "" {
		return ErrReviewAlreadySubmitted
	}
	if s.Status != ReviewFailed {
		return ErrReviewNotFailed
	}
	return nil
}

// ReviewSessionFilter selects a page of sessions.
type ReviewSessionFilter struct {
	OrgName  string
	RepoName string
	Status   string
	Limit    int
}

// ReviewEvent is one recorded step of a session.
type ReviewEvent struct {
	Seq     int
	At      time.Time
	Type    string
	Payload json.RawMessage
}

// NewReviewEvent is an event on its way to the table.
type NewReviewEvent struct {
	Seq     int
	At      time.Time
	Type    string
	Payload json.RawMessage
}

const reviewSessionColumns = `id, coalesce(job_id::text, ''), installation_id, org_name, repo_name,
	pr_number, coalesce(pr_title, ''), coalesce(head_sha, ''), coalesce(base_sha, ''), coalesce(model, ''),
	status, coalesce(summary, ''), coalesce(review_event, ''), tool_calls, coalesce(error, ''),
	coalesce(parent_session_id::text, ''), turn_limit_reached, coalesce(requested_by, ''), requested_at,
	input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, cost_usd,
	started_at, finished_at`

// CreateReviewSession records the start of a review and returns the row.
func (d *DB) CreateReviewSession(ctx context.Context, session NewReviewSession) (ReviewSession, error) {
	row := d.pool.QueryRow(ctx, `
		INSERT INTO review_sessions (job_id, installation_id, org_name, repo_name, pr_number, model,
			parent_session_id, requested_by, requested_at)
		VALUES (nullif($1, '')::uuid, $2, $3, $4, $5, nullif($6, ''),
			nullif($7, '')::uuid, nullif($8, ''), $9)
		RETURNING `+reviewSessionColumns,
		session.JobID, session.InstallationID, session.OrgName, session.RepoName, session.PRNumber, session.Model,
		session.ParentSessionID, session.RequestedBy, session.RequestedAt)

	record, err := scanReviewSession(row)
	if err != nil {
		return ReviewSession{}, fmt.Errorf("creating a review session: %w", err)
	}
	return record, nil
}

// UpdateReviewSessionMetadata fills the fields the engine learns after the
// session starts. A value that is not known leaves the column alone.
func (d *DB) UpdateReviewSessionMetadata(ctx context.Context, id, title, headSHA, baseSHA string) error {
	tag, err := d.pool.Exec(ctx, `
		UPDATE review_sessions
		SET pr_title = coalesce(nullif($2, ''), pr_title),
		    head_sha = coalesce(nullif($3, ''), head_sha),
		    base_sha = coalesce(nullif($4, ''), base_sha)
		WHERE id = $1`, id, title, headSHA, baseSHA)
	if err != nil {
		return fmt.Errorf("updating review session %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrReviewSessionNotFound
	}
	return nil
}

// FinishReviewSession records the outcome. It is safe to call once; a second
// call moves the finish time, which a restart should not do, so the caller
// owns the one call.
func (d *DB) FinishReviewSession(ctx context.Context, id string, outcome ReviewOutcome) error {
	status := outcome.Status
	if status == "" {
		status = ReviewSucceeded
	}
	tag, err := d.pool.Exec(ctx, `
		UPDATE review_sessions
		SET status = $2,
		    summary = nullif($3, ''),
		    review_event = nullif($4, ''),
		    tool_calls = $5,
		    error = nullif($6, ''),
		    turn_limit_reached = $7,
		    input_tokens = $8,
		    output_tokens = $9,
		    cache_read_tokens = $10,
		    cache_creation_tokens = $11,
		    cost_usd = $12,
		    finished_at = now()
		WHERE id = $1`,
		id, status, outcome.Summary, outcome.ReviewEvent, outcome.ToolCalls, outcome.Error, outcome.TurnLimitReached,
		usageField(outcome.Usage, func(u TokenUsage) int64 { return u.InputTokens }),
		usageField(outcome.Usage, func(u TokenUsage) int64 { return u.OutputTokens }),
		usageField(outcome.Usage, func(u TokenUsage) int64 { return u.CacheReadTokens }),
		usageField(outcome.Usage, func(u TokenUsage) int64 { return u.CacheCreationTokens }),
		costField(outcome.Usage))
	if err != nil {
		return fmt.Errorf("finishing review session %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrReviewSessionNotFound
	}
	return nil
}

// usageField is one nullable token column: NULL when the provider reported no
// usage, the number otherwise.
func usageField(usage *TokenUsage, field func(TokenUsage) int64) any {
	if usage == nil {
		return nil
	}
	return field(*usage)
}

// costField is the nullable cost column.
func costField(usage *TokenUsage) any {
	if usage == nil || usage.CostUSD == nil {
		return nil
	}
	return *usage.CostUSD
}

// InterruptRunningReviewSessions closes sessions that were running when the
// process stopped. A session is written by the process that runs the review,
// so after a restart nobody is going to finish it; leaving it RUNNING would put
// a live badge on a page whose stream can never end. This assumes one server
// drains a database, which is how `rumil serve` is run.
func (d *DB) InterruptRunningReviewSessions(ctx context.Context) (int64, error) {
	tag, err := d.pool.Exec(ctx, `
		UPDATE review_sessions
		SET status = $1,
		    error = coalesce(error, 'the process stopped before the review finished'),
		    finished_at = now()
		WHERE status = $2`, ReviewFailed, ReviewRunning)
	if err != nil {
		return 0, fmt.Errorf("interrupting running review sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ListReviewSessions returns sessions newest first, one page at a time.
func (d *DB) ListReviewSessions(ctx context.Context, filter ReviewSessionFilter) ([]ReviewSession, error) {
	limit := filter.Limit
	switch {
	case limit <= 0:
		limit = ReviewListDefault
	case limit > ReviewListMax:
		limit = ReviewListMax
	}

	rows, err := d.pool.Query(ctx, `
		SELECT `+reviewSessionColumns+`
		FROM review_sessions
		WHERE ($1 = '' OR status::text = $1)
		  AND ($2 = '' OR org_name = $2)
		  AND ($3 = '' OR repo_name = $3)
		ORDER BY started_at DESC, id DESC
		LIMIT $4`, filter.Status, filter.OrgName, filter.RepoName, limit)
	if err != nil {
		return nil, fmt.Errorf("listing review sessions: %w", err)
	}
	defer rows.Close()

	sessions := make([]ReviewSession, 0, limit)
	for rows.Next() {
		session, err := scanReviewSession(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning a review session: %w", err)
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing review sessions: %w", err)
	}
	return sessions, nil
}

// ReviewSessionUsage aggregates the sessions a filter matches, which is what
// the list's header totals. It is a full scan of the filter, not of the page
// the list happens to show.
func (d *DB) ReviewSessionUsage(ctx context.Context, filter ReviewSessionFilter) (UsageTotals, error) {
	var totals UsageTotals
	err := d.pool.QueryRow(ctx, `
		SELECT count(*), count(input_tokens),
		       coalesce(sum(input_tokens), 0)::bigint,
		       coalesce(sum(output_tokens), 0)::bigint,
		       coalesce(sum(cache_read_tokens), 0)::bigint,
		       coalesce(sum(cache_creation_tokens), 0)::bigint,
		       coalesce(sum(cost_usd), 0)::double precision, count(cost_usd)
		FROM review_sessions
		WHERE ($1 = '' OR status::text = $1)
		  AND ($2 = '' OR org_name = $2)
		  AND ($3 = '' OR repo_name = $3)`,
		filter.Status, filter.OrgName, filter.RepoName).Scan(
		&totals.Sessions, &totals.Reported,
		&totals.InputTokens, &totals.OutputTokens, &totals.CacheReadTokens, &totals.CacheCreationTokens,
		&totals.CostUSD, &totals.CostReported)
	if err != nil {
		return UsageTotals{}, fmt.Errorf("aggregating review session usage: %w", err)
	}
	return totals, nil
}

// ReviewSessionByID returns one session, or ErrReviewSessionNotFound.
func (d *DB) ReviewSessionByID(ctx context.Context, id string) (ReviewSession, error) {
	row := d.pool.QueryRow(ctx, `SELECT `+reviewSessionColumns+` FROM review_sessions WHERE id = $1`, id)

	session, err := scanReviewSession(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReviewSession{}, ErrReviewSessionNotFound
	}
	if err != nil {
		return ReviewSession{}, fmt.Errorf("reading review session %s: %w", id, err)
	}
	return session, nil
}

// ReviewSessionContinuation returns the latest session that continued
// parentID, or nil when the parent has not been continued. A parent whose
// continuation job failed before it started has no child and is reported as
// not continued.
func (d *DB) ReviewSessionContinuation(ctx context.Context, parentID string) (*ReviewSession, error) {
	row := d.pool.QueryRow(ctx, `
		SELECT `+reviewSessionColumns+`
		FROM review_sessions
		WHERE parent_session_id = $1
		ORDER BY started_at DESC, id DESC
		LIMIT 1`, parentID)

	session, err := scanReviewSession(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the continuation of session %s: %w", parentID, err)
	}
	return &session, nil
}

// AppendReviewEvents stores a batch in one transaction. A repeated seq is
// absorbed rather than failing the batch, so a writer that flushes after a
// restart cannot take the session down with it.
func (d *DB) AppendReviewEvents(ctx context.Context, sessionID string, events []NewReviewEvent) error {
	if len(events) == 0 {
		return nil
	}

	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("recording session events: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, event := range events {
		if _, err := tx.Exec(ctx, `
			INSERT INTO review_events (session_id, seq, at, type, payload)
			VALUES ($1, $2, $3, $4, $5::jsonb)
			ON CONFLICT (session_id, seq) DO NOTHING`,
			sessionID, event.Seq, event.At, event.Type, string(event.Payload)); err != nil {
			return fmt.Errorf("recording session event %d: %w", event.Seq, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("recording session events: %w", err)
	}
	return nil
}

// ReviewEventsAfter returns the events after a seq, oldest first. One call is
// bounded by limit; a replay loops until it returns fewer.
func (d *DB) ReviewEventsAfter(ctx context.Context, sessionID string, after, limit int) ([]ReviewEvent, error) {
	switch {
	case limit <= 0:
		limit = ReviewEventsDefault
	case limit > ReviewEventsMax:
		limit = ReviewEventsMax
	}

	rows, err := d.pool.Query(ctx, `
		SELECT seq, at, type, payload
		FROM review_events
		WHERE session_id = $1 AND seq > $2
		ORDER BY seq
		LIMIT $3`, sessionID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("reading session events: %w", err)
	}
	defer rows.Close()

	events := make([]ReviewEvent, 0, limit)
	for rows.Next() {
		var event ReviewEvent
		var payload []byte
		if err := rows.Scan(&event.Seq, &event.At, &event.Type, &payload); err != nil {
			return nil, fmt.Errorf("scanning a session event: %w", err)
		}
		event.Payload = payload
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading session events: %w", err)
	}
	return events, nil
}

// ReviewTranscriptEvents returns the events that carry what a session learned,
// oldest first, bounded to the most recent ones. Deltas and step boundaries are
// left out: they say what the model was doing, and a continuation needs what it
// had found. Reading newest first is what makes the bound keep the part the
// session was working on when it stopped.
func (d *DB) ReviewTranscriptEvents(ctx context.Context, sessionID string, limit int) ([]ReviewEvent, error) {
	if limit <= 0 || limit > ReviewTranscriptMax {
		limit = ReviewTranscriptMax
	}

	rows, err := d.pool.Query(ctx, `
		SELECT seq, at, type, payload
		FROM review_events
		WHERE session_id = $1
		  AND type IN ('tool_call', 'tool_result', 'compaction', 'message_end', 'session_error')
		ORDER BY seq DESC
		LIMIT $2`, sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("reading the transcript of session %s: %w", sessionID, err)
	}
	defer rows.Close()

	events := make([]ReviewEvent, 0, limit)
	for rows.Next() {
		var event ReviewEvent
		var payload []byte
		if err := rows.Scan(&event.Seq, &event.At, &event.Type, &payload); err != nil {
			return nil, fmt.Errorf("scanning a transcript event: %w", err)
		}
		event.Payload = payload
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the transcript of session %s: %w", sessionID, err)
	}

	slices.Reverse(events)
	return events, nil
}

// rowScanner is shared by a single-row query and a row of a result set, so the
// column list and the scan stay in one place.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanReviewSession(row rowScanner) (ReviewSession, error) {
	var session ReviewSession
	var (
		inputTokens         *int64
		outputTokens        *int64
		cacheReadTokens     *int64
		cacheCreationTokens *int64
		costUSD             *float64
	)
	if err := row.Scan(
		&session.ID, &session.JobID, &session.InstallationID, &session.OrgName, &session.RepoName,
		&session.PRNumber, &session.PRTitle, &session.HeadSHA, &session.BaseSHA, &session.Model,
		&session.Status, &session.Summary, &session.ReviewEvent, &session.ToolCalls, &session.Error,
		&session.ParentSessionID, &session.TurnLimitReached, &session.RequestedBy, &session.RequestedAt,
		&inputTokens, &outputTokens, &cacheReadTokens, &cacheCreationTokens, &costUSD,
		&session.StartedAt, &session.FinishedAt,
	); err != nil {
		return ReviewSession{}, err
	}
	if inputTokens != nil {
		session.Usage = &TokenUsage{
			InputTokens:         *inputTokens,
			OutputTokens:        valueOrZero(outputTokens),
			CacheReadTokens:     valueOrZero(cacheReadTokens),
			CacheCreationTokens: valueOrZero(cacheCreationTokens),
			CostUSD:             costUSD,
		}
	}
	return session, nil
}

// valueOrZero reads a nullable counter. The columns are written together, so a
// NULL in one of the others is a row a migration left behind rather than a
// value to reject.
func valueOrZero(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}
