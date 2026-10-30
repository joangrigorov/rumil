package server_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/joangrigorov/rumil/internal/continuation"
	"github.com/joangrigorov/rumil/internal/server"
	"github.com/joangrigorov/rumil/internal/store"
	"github.com/joangrigorov/rumil/internal/testutil"
)

// sessionsHandler mounts the observation deck with the harness's database
// behind it. The deck shares the moderation credential.
func sessionsHandler(t *testing.T, h *harness) (http.Handler, *server.SessionsUI) {
	t.Helper()

	deck := server.NewSessionsUI(h.db, credentialsFor(adminToken), h.logger)
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

// newSessionWithOutcome records a finished session with the outcome a test
// needs. It is separate from newReviewSession because a failed session that
// never submitted must not carry a review event.
func newSessionWithOutcome(t *testing.T, h *harness, outcome store.ReviewOutcome) store.ReviewSession {
	t.Helper()

	ctx := context.Background()
	session, err := h.db.CreateReviewSession(ctx, store.NewReviewSession{
		InstallationID: h.cfg.GitHub.InstallationID,
		OrgName:        "acme",
		RepoName:       "payments",
		PRNumber:       42,
		Model:          "test-model",
	})
	if err != nil {
		t.Fatalf("creating a review session: %v", err)
	}
	if outcome.Status != store.ReviewRunning {
		if err := h.db.FinishReviewSession(ctx, session.ID, outcome); err != nil {
			t.Fatalf("finishing the review session: %v", err)
		}
	}
	stored, err := h.db.ReviewSessionByID(ctx, session.ID)
	if err != nil {
		t.Fatalf("reading the review session: %v", err)
	}
	return stored
}

// newTurnLimitedSession is the failure a continuation is for, with the tool
// context the earlier attempt had gathered.
func newTurnLimitedSession(t *testing.T, h *harness, headSHA string) store.ReviewSession {
	t.Helper()

	session := newSessionWithOutcome(t, h, store.ReviewOutcome{
		Status:           store.ReviewFailed,
		Summary:          "I had started reading the diff.",
		ToolCalls:        2,
		Error:            "review session ended without submitting a review for acme/payments#42 after 6 steps and 2 tool calls (turn limit 6 reached)",
		TurnLimitReached: true,
	})
	ctx := context.Background()
	if err := h.db.UpdateReviewSessionMetadata(ctx, session.ID, "Fix the flap", headSHA, "base"); err != nil {
		t.Fatalf("updating the session metadata: %v", err)
	}
	now := time.Now().Add(-time.Minute)
	if err := h.db.AppendReviewEvents(ctx, session.ID, []store.NewReviewEvent{
		{Seq: 1, At: now, Type: "tool_call", Payload: json.RawMessage(`{"id":"call_1","name":"get_pr_diff","args":{}}`)},
		{Seq: 2, At: now.Add(time.Second), Type: "tool_result", Payload: json.RawMessage(`{"id":"call_1","name":"get_pr_diff","result":"the settlement retry is unbounded"}`)},
	}); err != nil {
		t.Fatalf("appending the session transcript: %v", err)
	}
	stored, err := h.db.ReviewSessionByID(ctx, session.ID)
	if err != nil {
		t.Fatalf("reading the updated session: %v", err)
	}
	return stored
}

func TestObservationDeckOffersContinuationToAnyFailedSession(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{})
	handler, _ := sessionsHandler(t, h)

	turnLimited := newTurnLimitedSession(t, h, gh.HeadSHA)
	otherFailure := newSessionWithOutcome(t, h, store.ReviewOutcome{
		Status: store.ReviewFailed,
		Error:  "the GitHub App is not installed",
	})
	succeeded := newSessionWithOutcome(t, h, store.ReviewOutcome{
		Status:      store.ReviewSucceeded,
		ReviewEvent: "COMMENT",
	})

	for name, session := range map[string]store.ReviewSession{
		"turn limited": turnLimited,
		"other cause":  otherFailure,
	} {
		t.Run(name, func(t *testing.T) {
			offered := moderatedRequest(t, handler, http.MethodGet, "/ui/sessions/"+session.ID, adminToken, nil, "text/html")
			if offered.Code != http.StatusOK {
				t.Fatalf("detail status = %d, want 200 (body: %s)", offered.Code, offered.Body.String())
			}
			for _, want := range []string{
				"Continue review",
				`action="/ui/sessions/` + session.ID + `/continue"`,
				"Another attempt with the configured turn budget",
			} {
				if !strings.Contains(offered.Body.String(), want) {
					t.Errorf("the failed session does not offer a continuation (%q):\n%s", want, offered.Body.String())
				}
			}
		})
	}

	response := moderatedRequest(t, handler, http.MethodGet, "/ui/sessions/"+succeeded.ID, adminToken, nil, "text/html")
	if response.Code != http.StatusOK {
		t.Fatalf("detail status = %d, want 200", response.Code)
	}
	if strings.Contains(response.Body.String(), "Continue review") {
		t.Errorf("a session that submitted a review offers a continuation:\n%s", response.Body.String())
	}
}

func TestObservationDeckQueuesAContinuation(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{})
	handler, _ := sessionsHandler(t, h)
	session := newTurnLimitedSession(t, h, gh.HeadSHA)
	path := "/ui/sessions/" + session.ID + "/continue"

	withoutToken := moderatedRequest(t, handler, http.MethodPost, path, "", nil, "text/html")
	if withoutToken.Code != http.StatusUnauthorized {
		t.Errorf("a continuation without a token = %d, want 401", withoutToken.Code)
	}
	if job, err := h.db.JobByDelivery(context.Background(), continuation.DeliveryID(session.ID)); err != nil {
		t.Fatalf("reading the job: %v", err)
	} else if job != nil {
		t.Error("an unauthenticated continuation queued a job")
	}

	crossSite := moderatedRequestWith(t, handler, http.MethodPost, path, adminToken, nil, "text/html",
		map[string]string{"Sec-Fetch-Site": "cross-site"})
	if crossSite.Code != http.StatusForbidden {
		t.Errorf("a cross-site continuation = %d, want 403", crossSite.Code)
	}

	response := moderatedRequest(t, handler, http.MethodPost, path, adminToken, nil, "text/html")
	if response.Code != http.StatusSeeOther {
		t.Fatalf("continuation status = %d, want 303 (body: %s)", response.Code, response.Body.String())
	}
	if location := response.Header().Get("Location"); location != "/ui/sessions/"+session.ID {
		t.Errorf("Location = %q, want the session page", location)
	}

	job, err := h.db.JobByDelivery(context.Background(), continuation.DeliveryID(session.ID))
	if err != nil {
		t.Fatalf("reading the queued job: %v", err)
	}
	if job == nil {
		t.Fatal("no continuation job was queued")
	}
	if job.EventType != continuation.EventType {
		t.Errorf("event_type = %q, want %q", job.EventType, continuation.EventType)
	}
	request, err := continuation.Parse(job.Payload)
	if err != nil {
		t.Fatalf("parsing the queued request: %v", err)
	}
	if request.SessionID != session.ID || request.Turns != 0 || request.RequestedBy != "legacy" {
		t.Errorf("queued request = %+v, want the session, the configured budget and the admin token", request)
	}

	// The page reports the queued request instead of offering a second one.
	again := moderatedRequest(t, handler, http.MethodGet, "/ui/sessions/"+session.ID, adminToken, nil, "text/html")
	if !strings.Contains(again.Body.String(), "Continuation queued") {
		t.Errorf("the page does not report the queued continuation:\n%s", again.Body.String())
	}
	if strings.Contains(again.Body.String(), "Continue review") {
		t.Errorf("the page still offers a second continuation:\n%s", again.Body.String())
	}

	duplicate := moderatedRequest(t, handler, http.MethodPost, path, adminToken, nil, "text/html")
	if duplicate.Code != http.StatusConflict {
		t.Errorf("a duplicate continuation = %d, want 409", duplicate.Code)
	}
}

func TestObservationDeckContinuesAFailureWithAnotherCause(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{})
	handler, _ := sessionsHandler(t, h)
	session := newSessionWithOutcome(t, h, store.ReviewOutcome{
		Status: store.ReviewFailed,
		Error:  "the GitHub App is not installed",
	})

	response := moderatedRequest(t, handler, http.MethodPost, "/ui/sessions/"+session.ID+"/continue", adminToken, nil, "text/html")
	if response.Code != http.StatusSeeOther {
		t.Errorf("continuation of another failure = %d, want 303 (body: %s)", response.Code, response.Body.String())
	}
	job, err := h.db.JobByDelivery(context.Background(), continuation.DeliveryID(session.ID))
	if err != nil || job == nil {
		t.Fatalf("reading the queued job returned (%v, %v), want a job", job, err)
	}
}

func TestObservationDeckContinuationTakesItsOwnBudget(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{})
	handler, _ := sessionsHandler(t, h)
	session := newTurnLimitedSession(t, h, gh.HeadSHA)

	form := url.Values{"turns": {"7"}, "requested_by": {"alice"}}
	response := moderatedRequest(t, handler, http.MethodPost, "/ui/sessions/"+session.ID+"/continue", adminToken, form, "")
	if response.Code != http.StatusAccepted {
		t.Fatalf("JSON continuation status = %d, want 202 (body: %s)", response.Code, response.Body.String())
	}

	job, err := h.db.JobByDelivery(context.Background(), continuation.DeliveryID(session.ID))
	if err != nil || job == nil {
		t.Fatalf("reading the queued job returned (%v, %v), want a job", job, err)
	}
	request, err := continuation.Parse(job.Payload)
	if err != nil {
		t.Fatalf("parsing the queued request: %v", err)
	}
	if request.Turns != 7 || request.RequestedBy != "alice" {
		t.Errorf("queued request = %+v, want seven turns requested by alice", request)
	}

	bad := moderatedRequest(t, handler, http.MethodPost, "/ui/sessions/"+session.ID+"/continue", adminToken,
		url.Values{"turns": {"-1"}}, "")
	if bad.Code != http.StatusBadRequest {
		t.Errorf("negative turns = %d, want 400", bad.Code)
	}
}

// The worker continues the session from its stored transcript and records the
// new attempt as a session of its own.
func TestWorkerContinuesATurnLimitedSession(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	scriptReview(llm)

	h := newHarness(t, gh, llm, harnessOpts{})
	_, deck := sessionsHandler(t, h)
	parent := newTurnLimitedSession(t, h, gh.HeadSHA)

	if _, err := continuation.Queue(context.Background(), h.db, parent, 5, "alice"); err != nil {
		t.Fatalf("queueing the continuation: %v", err)
	}

	pool := server.NewPool(h.db, h.engine(t, gh), "@rumil", 1, h.logger)
	pool.SetSessionRecorder(deck.Recorder())
	poolCtx, stopPool := context.WithCancel(context.Background())
	pool.Start(poolCtx)
	waitFor(t, "the continuation to finish", func() bool {
		sessions, err := h.db.ListReviewSessions(context.Background(), store.ReviewSessionFilter{})
		if err != nil {
			return false
		}
		for _, session := range sessions {
			if session.ParentSessionID == parent.ID && session.Status != store.ReviewRunning {
				return true
			}
		}
		return false
	})
	stopPool()
	pool.Wait()

	sessions, err := h.db.ListReviewSessions(context.Background(), store.ReviewSessionFilter{})
	if err != nil {
		t.Fatalf("listing review sessions: %v", err)
	}
	var child *store.ReviewSession
	for i := range sessions {
		if sessions[i].ParentSessionID == parent.ID {
			child = &sessions[i]
		}
	}
	if child == nil {
		t.Fatal("the continuation recorded no session")
	}
	if child.Status != store.ReviewSucceeded {
		t.Errorf("continuation status = %q, want %q (error: %s)", child.Status, store.ReviewSucceeded, child.Error)
	}
	if child.ReviewEvent != "COMMENT" {
		t.Errorf("continuation review_event = %q, want COMMENT", child.ReviewEvent)
	}
	if child.RequestedBy != "alice" {
		t.Errorf("requested_by = %q, want alice", child.RequestedBy)
	}
	if child.RequestedAt == nil {
		t.Error("requested_at is nil")
	}
	if child.JobID == "" || child.JobID == parent.JobID {
		t.Errorf("job_id = %q, want the continuation job", child.JobID)
	}

	kept, err := h.db.ReviewSessionByID(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("reading the parent session: %v", err)
	}
	if kept.Status != store.ReviewFailed || !kept.TurnLimitReached {
		t.Errorf("parent = %q (turn limit %t), want the original failure", kept.Status, kept.TurnLimitReached)
	}

	prompt := llm.Requests()[0].SystemPrompt
	for _, want := range []string{"<previous_attempt>", "the settlement retry is unbounded"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the continuation prompt lost %q:\n%s", want, prompt)
		}
	}

	job, err := h.db.JobByDelivery(context.Background(), continuation.DeliveryID(parent.ID))
	if err != nil {
		t.Fatalf("reading the continuation job: %v", err)
	}
	if job == nil || job.Status != store.StatusCompleted {
		t.Errorf("continuation job = %+v, want COMPLETED", job)
	}
}

// The deck shows what a session spent, and says so plainly when the provider
// reported nothing rather than showing a free review.
func TestObservationDeckShowsUsage(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{})
	ctx := context.Background()

	session, err := h.db.CreateReviewSession(ctx, store.NewReviewSession{
		InstallationID: h.cfg.GitHub.InstallationID,
		OrgName:        "acme",
		RepoName:       "payments",
		PRNumber:       42,
		Model:          "test-model",
	})
	if err != nil {
		t.Fatalf("creating a review session: %v", err)
	}
	cost := 0.0012
	if err := h.db.FinishReviewSession(ctx, session.ID, store.ReviewOutcome{
		Status: store.ReviewSucceeded, ReviewEvent: "COMMENT",
		Usage: &store.TokenUsage{
			InputTokens: 1234, OutputTokens: 567, CacheReadTokens: 89,
			CacheCreationTokens: 10, CostUSD: &cost,
		},
	}); err != nil {
		t.Fatalf("finishing the review session: %v", err)
	}

	silent := newReviewSession(t, h, "ledger", 7, "Add a retry", "", store.ReviewSucceeded)
	handler, _ := sessionsHandler(t, h)

	detail := moderatedRequest(t, handler, http.MethodGet, "/ui/sessions/"+session.ID, adminToken, nil, "text/html")
	if detail.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", detail.Code, detail.Body.String())
	}
	body := detail.Body.String()
	for _, want := range []string{"1,234 in", "567 out", "89 cache read", "10 cache write", "est. $0.0012"} {
		if !strings.Contains(body, want) {
			t.Errorf("the session page lost %q:\n%s", want, body)
		}
	}

	unreported := moderatedRequest(t, handler, http.MethodGet, "/ui/sessions/"+silent.ID, adminToken, nil, "text/html")
	if !strings.Contains(unreported.Body.String(), "usage not reported") {
		t.Errorf("a session without usage does not say so:\n%s", unreported.Body.String())
	}

	list := moderatedRequest(t, handler, http.MethodGet, "/ui/sessions", adminToken, nil, "text/html")
	listBody := list.Body.String()
	for _, want := range []string{"1,900 tokens", "est. $0.0012"} {
		if !strings.Contains(listBody, want) {
			t.Errorf("the session list lost %q:\n%s", want, listBody)
		}
	}
}

// The queue view lists waiting and running jobs, and a cancel POST needs the
// header and the same-site check.
func TestObservationDeckQueueListsAndCancels(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{})
	ctx := context.Background()

	for _, delivery := range []string{"delivery-1", "delivery-2"} {
		if err := h.db.Enqueue(ctx, store.Job{
			DeliveryID: delivery, InstallationID: h.cfg.GitHub.InstallationID,
			EventType: "issue_comment", OrgName: "acme", RepoName: "payments",
			PRNumber: 42, Payload: []byte(`{}`),
		}); err != nil {
			t.Fatalf("enqueueing %s: %v", delivery, err)
		}
	}
	running, err := h.db.ClaimJob(ctx, "worker-a")
	if err != nil || running == nil {
		t.Fatalf("ClaimJob returned (%v, %v), want a job", running, err)
	}
	handler, _ := sessionsHandler(t, h)

	page := moderatedRequest(t, handler, http.MethodGet, "/ui/queue", adminToken, nil, "text/html")
	if page.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", page.Code, page.Body.String())
	}
	body := page.Body.String()
	for _, want := range []string{"acme/payments#42", "RUNNING", "QUEUED", "worker-a", "issue_comment"} {
		if !strings.Contains(body, want) {
			t.Errorf("the queue lost %q:\n%s", want, body)
		}
	}

	confirm := moderatedRequest(t, handler, http.MethodGet, "/ui/queue/"+running.ID+"/cancel", adminToken, nil, "text/html")
	if confirm.Code != http.StatusOK || !strings.Contains(confirm.Body.String(), "Stop this review?") {
		t.Fatalf("confirm page = %d %q, want the running-review question", confirm.Code, confirm.Body.String())
	}

	// A cookie cannot drive the cancel; the POST needs the header.
	cookieOnly := httptest.NewRequest(http.MethodPost, "/ui/queue/"+running.ID+"/cancel", nil)
	cookieOnly.AddCookie(&http.Cookie{
		Name:  "rumil_admin",
		Value: base64.RawURLEncoding.EncodeToString([]byte(adminToken)),
	})
	cookieOnly.Header.Set("Accept", "text/html")
	cookieRecorder := httptest.NewRecorder()
	handler.ServeHTTP(cookieRecorder, cookieOnly)
	if cookieRecorder.Code != http.StatusUnauthorized {
		t.Errorf("cookie-only cancel = %d, want 401", cookieRecorder.Code)
	}

	// A form on another site is refused even with the token.
	crossSite := moderatedRequestWith(t, handler, http.MethodPost, "/ui/queue/"+running.ID+"/cancel",
		adminToken, nil, "text/html", map[string]string{"Sec-Fetch-Site": "cross-site"})
	if crossSite.Code != http.StatusForbidden {
		t.Errorf("cross-site cancel = %d, want 403", crossSite.Code)
	}

	posted := moderatedRequestWith(t, handler, http.MethodPost, "/ui/queue/"+running.ID+"/cancel",
		adminToken, nil, "text/html", map[string]string{"Sec-Fetch-Site": "same-origin"})
	if posted.Code != http.StatusSeeOther {
		t.Fatalf("cancel = %d, want a redirect (body: %s)", posted.Code, posted.Body.String())
	}
	if flag, err := h.db.JobCancelRequested(ctx, running.ID); err != nil || !flag {
		t.Errorf("cancel flag = %v, %v, want the request recorded", flag, err)
	}

	queued, err := h.db.JobByDelivery(ctx, "delivery-2")
	if err != nil || queued == nil {
		t.Fatalf("JobByDelivery returned (%v, %v)", queued, err)
	}
	cancelled := moderatedRequestWith(t, handler, http.MethodPost, "/ui/queue/"+queued.ID+"/cancel",
		adminToken, url.Values{}, "application/json", nil)
	if cancelled.Code != http.StatusAccepted {
		t.Fatalf("queued cancel = %d, want 202 (body: %s)", cancelled.Code, cancelled.Body.String())
	}
	stored, err := h.db.JobByID(ctx, queued.ID)
	if err != nil {
		t.Fatalf("JobByID returned error: %v", err)
	}
	if stored.Status != store.StatusCancelled {
		t.Errorf("status = %q, want CANCELLED", stored.Status)
	}
}
