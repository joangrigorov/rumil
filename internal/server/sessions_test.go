package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/joangrigorov/rumil/internal/server"
	"github.com/joangrigorov/rumil/internal/store"
	"github.com/joangrigorov/rumil/internal/testutil"
)

// sessionsHandler mounts the observation deck with the harness's database
// behind it. The deck shares the moderation credential.
func sessionsHandler(t *testing.T, h *harness) (http.Handler, *server.SessionsUI) {
	t.Helper()

	deck := server.NewSessionsUI(h.db, adminToken, h.logger)
	return server.Handler(h.webhook(), h.db, nil, deck), deck
}

func newReviewSession(t *testing.T, h *harness, repo string, number int, title, summary string, status string) store.ReviewSession {
	t.Helper()

	ctx := context.Background()
	session, err := h.db.CreateReviewSession(ctx, store.NewReviewSession{
		InstallationID: h.cfg.GitHub.InstallationID,
		OrgName:        "acme",
		RepoName:       repo,
		PRNumber:       number,
		Model:          "test-model",
	})
	if err != nil {
		t.Fatalf("creating a review session: %v", err)
	}
	if err := h.db.UpdateReviewSessionMetadata(ctx, session.ID, title, "abcdef1234567890", "1234567890abcdef"); err != nil {
		t.Fatalf("updating review session metadata: %v", err)
	}
	if status != store.ReviewRunning {
		if err := h.db.FinishReviewSession(ctx, session.ID, store.ReviewOutcome{
			Status:      status,
			Summary:     summary,
			ReviewEvent: "COMMENT",
			ToolCalls:   3,
		}); err != nil {
			t.Fatalf("finishing the review session: %v", err)
		}
	}
	finished, err := h.db.ReviewSessionByID(ctx, session.ID)
	if err != nil {
		t.Fatalf("reading the review session: %v", err)
	}
	return finished
}

func TestObservationDeckNeedsTheToken(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{})
	newReviewSession(t, h, "payments", 42, "Fix the flap", "", store.ReviewRunning)
	handler, _ := sessionsHandler(t, h)

	for _, path := range []string{"/ui/sessions", "/ui/sessions/00000000-0000-0000-0000-000000000000"} {
		response := moderatedRequest(t, handler, http.MethodGet, path, "", nil, "text/html")
		if response.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without a token = %d, want 401", path, response.Code)
		}
		if header := response.Header().Get("WWW-Authenticate"); !strings.Contains(header, `realm="rumil sessions"`) {
			t.Errorf("GET %s WWW-Authenticate = %q, want the sessions realm", path, header)
		}
	}
}

func TestObservationDeckListsAndFiltersSessions(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{})
	succeeded := newReviewSession(t, h, "payments", 42, "Fix the flap", "The change is sound.", store.ReviewSucceeded)
	failed := newReviewSession(t, h, "ledger", 7, "Add a retry", "", store.ReviewFailed)
	handler, _ := sessionsHandler(t, h)

	response := moderatedRequest(t, handler, http.MethodGet, "/ui/sessions", adminToken, nil, "text/html")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, want := range []string{
		`<body class="console">`,
		"Review sessions",
		"acme/payments#42",
		"acme/ledger#7",
		"Fix the flap",
		"Add a retry",
		"SUCCEEDED",
		"FAILED",
		`href="/ui/sessions/` + succeeded.ID + `"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the list does not contain %q:\n%s", want, body)
		}
	}
	if failed.Status != store.ReviewFailed {
		t.Fatalf("the failed fixture is %q", failed.Status)
	}
	if policy := response.Header().Get("Content-Security-Policy"); policy != "default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'" {
		t.Errorf("Content-Security-Policy = %q", policy)
	}
	if cache := response.Header().Get("Cache-Control"); cache != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cache)
	}

	filtered := moderatedRequest(t, handler, http.MethodGet, "/ui/sessions?status=FAILED", adminToken, nil, "text/html")
	if filtered.Code != http.StatusOK {
		t.Fatalf("filtered status = %d, want 200", filtered.Code)
	}
	if filteredBody := filtered.Body.String(); !strings.Contains(filteredBody, "acme/ledger#7") || strings.Contains(filteredBody, "acme/payments#42") {
		t.Errorf("the FAILED filter did not narrow the list:\n%s", filteredBody)
	}

	repository := moderatedRequest(t, handler, http.MethodGet, "/ui/sessions?repo=acme/payments", adminToken, nil, "text/html")
	if repository.Code != http.StatusOK {
		t.Fatalf("repository filter status = %d, want 200", repository.Code)
	}
	if repositoryBody := repository.Body.String(); !strings.Contains(repositoryBody, "acme/payments#42") || strings.Contains(repositoryBody, "acme/ledger#7") {
		t.Errorf("the repository filter did not narrow the list:\n%s", repositoryBody)
	}

	malformed := moderatedRequest(t, handler, http.MethodGet, "/ui/sessions?repo=payments", adminToken, nil, "text/html")
	if malformed.Code != http.StatusBadRequest {
		t.Errorf("a malformed repo filter = %d, want 400", malformed.Code)
	}

	empty := moderatedRequest(t, handler, http.MethodGet, "/ui/sessions?status=RUNNING", adminToken, nil, "text/html")
	if !strings.Contains(empty.Body.String(), "No sessions match.") {
		t.Errorf("the empty state is missing:\n%s", empty.Body.String())
	}
}

func TestObservationDeckShowsOneSession(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{})
	session := newReviewSession(t, h, "payments", 42, "Fix the flap", "The change is sound.", store.ReviewSucceeded)
	handler, _ := sessionsHandler(t, h)

	response := moderatedRequest(t, handler, http.MethodGet, "/ui/sessions/"+session.ID, adminToken, nil, "text/html")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, want := range []string{
		`<body class="console console-fixed">`,
		"acme/payments#42",
		"Fix the flap",
		"data-session-id=\"" + session.ID + "\"",
		`class="timeline console-log"`,
		`id="activity"`,
		`aria-live="polite"`,
		"abcdef1",
		"https://github.com/acme/payments/pull/42",
		"/static/sessions.js",
		"/static/theme.js",
		`id="theme-toggle"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the detail page does not contain %q:\n%s", want, body)
		}
	}

	missing := moderatedRequest(t, handler, http.MethodGet, "/ui/sessions/00000000-0000-0000-0000-000000000000", adminToken, nil, "text/html")
	if missing.Code != http.StatusNotFound {
		t.Errorf("an unknown session = %d, want 404", missing.Code)
	}
}

func TestObservationDeckReplaysEvents(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{})
	session := newReviewSession(t, h, "payments", 42, "Fix the flap", "The change is sound.", store.ReviewSucceeded)

	ctx := context.Background()
	now := time.Now().Add(-time.Minute)
	if err := h.db.AppendReviewEvents(ctx, session.ID, []store.NewReviewEvent{
		{Seq: 1, At: now, Type: "step_start", Payload: json.RawMessage(`{"step":1}`)},
		{Seq: 2, At: now.Add(time.Second), Type: "tool_call", Payload: json.RawMessage(`{"id":"call_1","name":"read_file"}`)},
		{Seq: 3, At: now.Add(2 * time.Second), Type: "session_end", Payload: json.RawMessage(`{"status":"SUCCEEDED"}`)},
	}); err != nil {
		t.Fatalf("appending events: %v", err)
	}
	handler, _ := sessionsHandler(t, h)

	response := moderatedRequest(t, handler, http.MethodGet, "/api/sessions/"+session.ID+"/events.json", adminToken, nil, "")
	if response.Code != http.StatusOK {
		t.Fatalf("JSON status = %d, want 200 (body: %s)", response.Code, response.Body.String())
	}
	var payload struct {
		Events []struct {
			Seq     int             `json:"seq"`
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		} `json:"events"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decoding the JSON replay: %v\n%s", err, response.Body.String())
	}
	if len(payload.Events) != 3 {
		t.Fatalf("events = %d, want 3", len(payload.Events))
	}
	if payload.Events[1].Type != "tool_call" || !strings.Contains(string(payload.Events[1].Payload), "read_file") {
		t.Errorf("second event = %+v, want the tool call", payload.Events[1])
	}

	stream := moderatedRequest(t, handler, http.MethodGet, "/api/sessions/"+session.ID+"/events", adminToken, nil, "text/event-stream")
	if stream.Code != http.StatusOK {
		t.Fatalf("SSE status = %d, want 200", stream.Code)
	}
	streamBody := stream.Body.String()
	for _, want := range []string{
		"id: 1\n",
		`"type":"step_start"`,
		`"type":"tool_call"`,
		`"type":"session_end"`,
	} {
		if !strings.Contains(streamBody, want) {
			t.Errorf("the stream does not contain %q:\n%s", want, streamBody)
		}
	}

	// A reconnecting browser sends the last seq it rendered, and the replay
	// starts after it.
	request := moderatedRequestWith(t, handler, http.MethodGet, "/api/sessions/"+session.ID+"/events", adminToken, nil, "text/event-stream", map[string]string{"Last-Event-ID": "1"})
	if strings.Contains(request.Body.String(), "id: 1\n") {
		t.Errorf("a resume at seq 1 replayed seq 1:\n%s", request.Body.String())
	}
	if !strings.Contains(request.Body.String(), "id: 2\n") {
		t.Errorf("a resume at seq 1 did not replay seq 2:\n%s", request.Body.String())
	}
}

// The worker stores the whole trace of a review: the session row, the events
// the engine emitted, and the terminal event that carries the outcome.
func TestWorkerRecordsAReviewSession(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	scriptReview(llm)

	h := newHarness(t, gh, llm, harnessOpts{})
	handler, deck := sessionsHandler(t, h)
	pool := server.NewPool(h.db, h.engine(t, gh), "@rumil", 1, h.logger)
	pool.SetSessionRecorder(deck.Recorder())

	body := reviewRequestedPayload("review_requested", "rumil[bot]", "Bot")
	response := h.deliver(t, handler, "pull_request", "delivery-session", body, sign(webhookSecret, body))
	if response.Code != http.StatusAccepted {
		t.Fatalf("delivery status = %d, want 202", response.Code)
	}

	poolCtx, stopPool := context.WithCancel(context.Background())
	pool.Start(poolCtx)
	waitFor(t, "the review session to finish", func() bool {
		sessions, err := h.db.ListReviewSessions(context.Background(), store.ReviewSessionFilter{})
		return err == nil && len(sessions) == 1 && sessions[0].Status != store.ReviewRunning
	})
	stopPool()
	pool.Wait()

	sessions, err := h.db.ListReviewSessions(context.Background(), store.ReviewSessionFilter{})
	if err != nil {
		t.Fatalf("listing review sessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want the one review", len(sessions))
	}
	session := sessions[0]
	if session.Status != store.ReviewSucceeded {
		t.Errorf("status = %q, want %q (error: %s)", session.Status, store.ReviewSucceeded, session.Error)
	}
	if session.Model != "test-model" {
		t.Errorf("model = %q, want test-model", session.Model)
	}
	if session.ReviewEvent != "COMMENT" {
		t.Errorf("review_event = %q, want COMMENT", session.ReviewEvent)
	}
	if session.ToolCalls < 1 {
		t.Errorf("tool_calls = %d, want the submitted review's call", session.ToolCalls)
	}
	if session.FinishedAt == nil {
		t.Error("finished_at is nil")
	}
	if session.Summary == "" {
		t.Error("the session has no closing summary")
	}

	events, err := h.db.ReviewEventsAfter(context.Background(), session.ID, 0, store.ReviewEventsMax)
	if err != nil {
		t.Fatalf("reading the session events: %v", err)
	}
	if len(events) < 4 {
		t.Fatalf("events = %d, want a transcript", len(events))
	}
	types := make(map[string]bool, len(events))
	for i, event := range events {
		if event.Seq != i+1 {
			t.Errorf("event %d has seq %d, want %d", i, event.Seq, i+1)
		}
		types[event.Type] = true
	}
	for _, want := range []string{"session_metadata", "step_start", "tool_call", "review_submitted", "session_end"} {
		if !types[want] {
			t.Errorf("the transcript has no %s event (types: %v)", want, types)
		}
	}
	if last := events[len(events)-1]; last.Type != "session_end" {
		t.Errorf("the last event is %q, want session_end", last.Type)
	}
}
