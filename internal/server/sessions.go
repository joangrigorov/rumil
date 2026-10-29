package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/joangrigorov/rumil/internal/continuation"
	"github.com/joangrigorov/rumil/internal/store"
)

// SessionsUI is the observation deck: the review sessions the workers ran, and
// the transcript of one of them, live while it runs. It shares the moderation
// credential and has no login of its own.
type SessionsUI struct {
	db     *store.DB
	auth   *adminAuth
	broker *broker
	logger *slog.Logger
}

// NewSessionsUI builds the deck. The broker is owned here so the recorder the
// worker pool uses and the streams the browser reads are the same fan-out.
func NewSessionsUI(db *store.DB, token string, logger *slog.Logger) *SessionsUI {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &SessionsUI{
		db:     db,
		auth:   newAdminAuth(token, "rumil sessions", logger),
		broker: newBroker(),
		logger: logger,
	}
}

// Recorder returns the worker-side half of the deck.
func (s *SessionsUI) Recorder() *SessionRecorder {
	return NewSessionRecorder(s.db, s.broker, s.logger)
}

// Routes mounts the deck.
func (s *SessionsUI) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /ui/sessions", s.auth.authorize(s.list))
	mux.HandleFunc("GET /ui/sessions/{id}", s.auth.authorize(s.detail))
	mux.HandleFunc("POST /ui/sessions/{id}/continue", s.auth.authorize(s.auth.sameSite(s.continueSession)))
	mux.HandleFunc("GET /api/sessions/{id}/events", s.auth.authorize(s.events))
	mux.HandleFunc("GET /api/sessions/{id}/events.json", s.auth.authorize(s.eventsJSON))
}

// reviewSessionsPage is the list's view model.
type reviewSessionsPage struct {
	Title    string
	Section  string
	Refresh  string
	Fixed    bool
	Sessions []store.ReviewSession
	Count    int
	Status   string
	Repo     string
	Live     bool
}

// reviewSessionPage is the transcript's view model. The timeline itself is
// filled by the browser from the SSE stream, so a running session and a
// finished one render the same shell.
type reviewSessionPage struct {
	Title   string
	Section string
	Refresh string
	Fixed   bool
	Session store.ReviewSession
	Live    bool
	// Parent and Continuation are the two attempts of the same review, when
	// they exist, so the page can link them.
	Parent       *store.ReviewSession
	Continuation *store.ReviewSession
	// CanContinue is set only when the session stopped at its turn limit and
	// has not been continued yet.
	CanContinue bool
	// ContinuationQueued and ContinuationFailed report a continuation request
	// that has not produced a session yet.
	ContinuationQueued bool
	ContinuationFailed bool
}

func (s *SessionsUI) list(w http.ResponseWriter, r *http.Request) {
	rawRepo := r.URL.Query().Get("repo")
	filter := store.ReviewSessionFilter{
		Status: strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("status"))),
		Limit:  store.ReviewListDefault,
	}
	if rawRepo != "" {
		org, name, err := splitRepoRef(rawRepo)
		if err != nil {
			s.auth.refuse(w, r, http.StatusBadRequest, "repo takes owner/name")
			return
		}
		filter.OrgName, filter.RepoName = org, name
	}

	sessions, err := s.db.ListReviewSessions(r.Context(), filter)
	if err != nil {
		s.logger.Error("listing review sessions failed", "error", err)
		s.auth.refuse(w, r, http.StatusInternalServerError, "the sessions could not be read")
		return
	}

	live := false
	for _, session := range sessions {
		if session.Status == store.ReviewRunning {
			live = true
			break
		}
	}

	page := reviewSessionsPage{
		Title:    "Review sessions",
		Section:  "sessions",
		Sessions: sessions,
		Count:    len(sessions),
		Status:   filter.Status,
		Repo:     rawRepo,
		Live:     live,
	}
	if live {
		// The list has no JavaScript of its own: while a review runs, the page
		// refreshes itself so the status does not go stale in a parked tab.
		page.Refresh = "10"
	}

	s.writePage(w, r, sessionsTemplates, "sessions.html", page)
}

func (s *SessionsUI) detail(w http.ResponseWriter, r *http.Request) {
	session, err := s.db.ReviewSessionByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrReviewSessionNotFound) {
		s.auth.refuse(w, r, http.StatusNotFound, "no such review session")
		return
	}
	if err != nil {
		s.logger.Error("reading a review session failed", "error", err)
		s.auth.refuse(w, r, http.StatusInternalServerError, "the session could not be read")
		return
	}

	page := reviewSessionPage{
		Title:   fmt.Sprintf("%s/%s#%d", session.OrgName, session.RepoName, session.PRNumber),
		Section: "sessions",
		Fixed:   true,
		Session: session,
		Live:    session.Status == store.ReviewRunning,
	}

	if session.ParentSessionID != "" {
		parent, parentErr := s.db.ReviewSessionByID(r.Context(), session.ParentSessionID)
		switch {
		case parentErr == nil:
			page.Parent = &parent
		case errors.Is(parentErr, store.ErrReviewSessionNotFound):
			// The parent was removed; the session still stands on its own.
		default:
			s.logger.Warn("reading the parent session failed", "session_id", session.ID, "error", parentErr)
		}
	}

	if session.Continuable() == nil {
		child, childErr := s.db.ReviewSessionContinuation(r.Context(), session.ID)
		if childErr != nil {
			s.logger.Warn("reading the continuation session failed", "session_id", session.ID, "error", childErr)
		} else if child != nil {
			page.Continuation = child
		} else {
			// The job is what a queued request looks like before its session
			// exists, and what a refused request leaves behind.
			job, jobErr := s.db.JobByDelivery(r.Context(), continuation.DeliveryID(session.ID))
			switch {
			case jobErr != nil:
				s.logger.Warn("reading the continuation job failed", "session_id", session.ID, "error", jobErr)
			case job == nil:
				page.CanContinue = true
			case job.Status == store.StatusFailed:
				page.ContinuationFailed = true
			default:
				page.ContinuationQueued = true
			}
		}
	}

	s.writePage(w, r, sessionTemplates, "session.html", page)
}

// continueSession queues another attempt at a session that stopped at its turn
// limit. The work is a job of its own, so the answer is a queued continuation
// and a new session appears when a worker picks it up.
func (s *SessionsUI) continueSession(w http.ResponseWriter, r *http.Request) {
	if !s.readContinueForm(w, r) {
		return
	}

	session, err := s.db.ReviewSessionByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrReviewSessionNotFound) {
		s.auth.refuse(w, r, http.StatusNotFound, "no such review session")
		return
	}
	if err != nil {
		s.logger.Error("reading a review session failed", "error", err)
		s.auth.refuse(w, r, http.StatusInternalServerError, "the session could not be read")
		return
	}

	turns, err := continuationTurns(r)
	if err != nil {
		s.auth.refuse(w, r, http.StatusBadRequest, err.Error())
		return
	}

	request, err := continuation.Queue(r.Context(), s.db, session, turns, requestedByOf(r))
	switch {
	case errors.Is(err, continuation.ErrAlreadyQueued):
		s.auth.refuse(w, r, http.StatusConflict, "this session already has a continuation")
		return
	case errors.Is(err, store.ErrReviewAlreadySubmitted):
		s.auth.refuse(w, r, http.StatusConflict, "this session already submitted a review")
		return
	case errors.Is(err, store.ErrReviewNotFailed):
		s.auth.refuse(w, r, http.StatusConflict, "only a failed session can be continued")
		return
	case errors.Is(err, store.ErrReviewNotTurnLimited):
		s.auth.refuse(w, r, http.StatusConflict, "this session did not stop at the turn limit")
		return
	case err != nil:
		s.logger.Error("queueing a continuation failed", "session_id", session.ID, "error", err)
		s.auth.refuse(w, r, http.StatusInternalServerError, "the continuation could not be queued")
		return
	}

	s.logger.Info("queued a review continuation",
		"session_id", session.ID,
		"turns", request.Turns,
		"requested_by", request.RequestedBy,
	)

	if wantsHTML(r) {
		http.Redirect(w, r, "/ui/sessions/"+session.ID, http.StatusSeeOther)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"session_id":   session.ID,
		"turns":        request.Turns,
		"requested_by": request.RequestedBy,
		"requested_at": request.RequestedAt,
	})
}

// readContinueForm parses the body a continuation arrives in. The body may be
// empty, which takes the configured budget; anything that is not the form
// encoding is refused rather than read as no instructions.
func (s *SessionsUI) readContinueForm(w http.ResponseWriter, r *http.Request) bool {
	if contentType := r.Header.Get("Content-Type"); contentType != "" {
		mediaType, _, err := mime.ParseMediaType(contentType)
		if err != nil || mediaType != "application/x-www-form-urlencoded" {
			s.auth.refuse(w, r, http.StatusUnsupportedMediaType,
				"a continuation is sent as application/x-www-form-urlencoded")
			return false
		}
	}
	if err := r.ParseForm(); err != nil {
		s.auth.refuse(w, r, http.StatusBadRequest, "malformed form")
		return false
	}
	return true
}

// continuationTurns reads the optional turns field. An absent or zero value
// uses the configured budget, which is what the dashboard button sends.
func continuationTurns(r *http.Request) (int, error) {
	raw := strings.TrimSpace(r.PostFormValue("turns"))
	if raw == "" {
		return 0, nil
	}
	turns, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("turns has to be a whole number")
	}
	if turns < 0 {
		return 0, fmt.Errorf("turns has to be zero for the configured budget or positive")
	}
	return turns, nil
}

// requestedByOf names the person asking. The credential is shared, so the page
// cannot know who used it; an API client may name somebody with requested_by,
// and the default says what the surface actually knows.
func requestedByOf(r *http.Request) string {
	if name := strings.TrimSpace(r.PostFormValue("requested_by")); name != "" {
		return name
	}
	return "admin-token"
}

// writePage renders one operator page with the shared headers.
func (s *SessionsUI) writePage(w http.ResponseWriter, r *http.Request, templates *template.Template, name string, page any) {
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, "layout", page); err != nil {
		s.logger.Error("rendering an operator page failed", "page", name, "error", err)
	}
}

// sessionsTemplates and sessionTemplates are separate sets because every page
// defines "content".
var (
	sessionsTemplates = parseTemplates("sessions.html", pageFuncs())
	sessionTemplates  = parseTemplates("session.html", pageFuncs())
)

// events streams one session. It replays the stored transcript after the
// client's last seq, then follows the live broker. A finished session is a
// replay followed by session_end, which is how a reload reconstructs the whole
// timeline from the table.
func (s *SessionsUI) events(w http.ResponseWriter, r *http.Request) {
	session, err := s.db.ReviewSessionByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrReviewSessionNotFound) {
		s.auth.refuse(w, r, http.StatusNotFound, "no such review session")
		return
	}
	if err != nil {
		s.logger.Error("reading a review session failed", "error", err)
		s.auth.refuse(w, r, http.StatusInternalServerError, "the session could not be read")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		s.auth.refuse(w, r, http.StatusInternalServerError, "this server cannot stream")
		return
	}

	after := lastEventID(r)
	live, unsubscribe := s.broker.subscribe(session.ID)
	defer unsubscribe()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, "retry: 2000\n\n")
	flusher.Flush()

	last, ended := s.replay(r, w, flusher, session.ID, after)
	if ended {
		return
	}
	if session.Status != store.ReviewRunning {
		// A session recorded before the terminal event existed, or one whose
		// end was lost: the row's outcome is enough to close the page.
		writeSessionEnd(w, flusher, session)
		return
	}

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case event := <-live:
			if event.Seq <= last {
				continue
			}
			if writeEvent(w, event) != nil {
				return
			}
			last = event.Seq
			flusher.Flush()
			if event.Type == "session_end" {
				return
			}
		case <-heartbeat.C:
			_, _ = fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// replay writes stored events after the client's last seq. It reports the last
// seq written and whether it saw the terminal event.
func (s *SessionsUI) replay(r *http.Request, w http.ResponseWriter, flusher http.Flusher, sessionID string, after int) (int, bool) {
	last := after
	for {
		events, err := s.db.ReviewEventsAfter(r.Context(), sessionID, last, store.ReviewEventsDefault)
		if err != nil {
			s.logger.Error("replaying a review session failed", "session_id", sessionID, "error", err)
			return last, false
		}
		for _, event := range events {
			if writeEvent(w, event) != nil {
				return last, true
			}
			last = event.Seq
			if event.Type == "session_end" {
				flusher.Flush()
				return last, true
			}
		}
		flusher.Flush()
		if len(events) < store.ReviewEventsDefault {
			return last, false
		}
	}
}

// eventsJSON is the same replay as a JSON array, for a script or a page that
// cannot hold a stream. It is a bounded read: callers page it with after.
func (s *SessionsUI) eventsJSON(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	if _, err := s.db.ReviewSessionByID(r.Context(), sessionID); errors.Is(err, store.ErrReviewSessionNotFound) {
		s.auth.refuse(w, r, http.StatusNotFound, "no such review session")
		return
	} else if err != nil {
		s.logger.Error("reading a review session failed", "error", err)
		s.auth.refuse(w, r, http.StatusInternalServerError, "the session could not be read")
		return
	}

	events, err := s.db.ReviewEventsAfter(r.Context(), sessionID, lastEventID(r), store.ReviewEventsDefault)
	if err != nil {
		s.logger.Error("reading a review session failed", "session_id", sessionID, "error", err)
		s.auth.refuse(w, r, http.StatusInternalServerError, "the events could not be read")
		return
	}

	type event struct {
		Seq     int             `json:"seq"`
		At      time.Time       `json:"at"`
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	out := make([]event, 0, len(events))
	for _, stored := range events {
		out = append(out, event{Seq: stored.Seq, At: stored.At, Type: stored.Type, Payload: stored.Payload})
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}

// lastEventID reads the resume point EventSource sends on reconnect, with the
// after query as the fallback a script uses.
func lastEventID(r *http.Request) int {
	if header := r.Header.Get("Last-Event-ID"); header != "" {
		if seq, err := strconv.Atoi(header); err == nil && seq >= 0 {
			return seq
		}
	}
	if query := r.URL.Query().Get("after"); query != "" {
		if seq, err := strconv.Atoi(query); err == nil && seq >= 0 {
			return seq
		}
	}
	return 0
}

// writeEvent writes one SSE frame. The data is a single envelope rather than
// an HTML event name, so one listener sees every type: the deck decides how to
// render an event, and a new type does not need a new listener.
func writeEvent(w http.ResponseWriter, event store.ReviewEvent) error {
	envelope, err := json.Marshal(map[string]any{
		"seq":     event.Seq,
		"at":      event.At,
		"type":    event.Type,
		"payload": json.RawMessage(event.Payload),
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %d\ndata: %s\n\n", event.Seq, envelope)
	return err
}

// writeSessionEnd closes a replay whose row is already finished.
func writeSessionEnd(w http.ResponseWriter, flusher http.Flusher, session store.ReviewSession) {
	finishedAt := time.Now()
	if session.FinishedAt != nil {
		finishedAt = *session.FinishedAt
	}
	envelope, _ := json.Marshal(map[string]any{
		"seq":  0,
		"at":   finishedAt,
		"type": "session_end",
		"payload": map[string]any{
			"status":       session.Status,
			"summary":      session.Summary,
			"review_event": session.ReviewEvent,
			"tool_calls":   session.ToolCalls,
			"error":        session.Error,
			"finished_at":  finishedAt,
		},
	})
	_, _ = fmt.Fprintf(w, "data: %s\n\n", envelope)
	flusher.Flush()
}
