package server_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/joangrigorov/rumil/internal/store"
	"github.com/joangrigorov/rumil/internal/testutil"
)

// adminCookie is the value the pages hand the browser for the live stream.
func adminCookie() string {
	return base64.RawURLEncoding.EncodeToString([]byte(adminToken))
}

// The detail page is authenticated with a header the browser cannot repeat on
// an EventSource, so the answer has to leave the browser a credential the
// stream can carry: a same-site, script-invisible cookie.
func TestTheDeckHandsTheBrowserACookieForItsStream(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{})
	session := newReviewSession(t, h, "payments", 42, "Fix the flap", "The change is sound.", store.ReviewSucceeded)
	handler, _ := sessionsHandler(t, h)

	response := moderatedRequest(t, handler, http.MethodGet, "/ui/sessions/"+session.ID, adminToken, nil, "text/html")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", response.Code, response.Body.String())
	}
	cookies := response.Result().Cookies()
	var found *http.Cookie
	for _, cookie := range cookies {
		if cookie.Name == "rumil_admin" {
			found = cookie
		}
	}
	if found == nil {
		t.Fatalf("the page set no rumil_admin cookie: %v", cookies)
	}
	if found.Value != adminCookie() {
		t.Errorf("cookie value = %q, want the encoded token", found.Value)
	}
	if !found.HttpOnly {
		t.Error("the cookie is readable from a script")
	}
	if found.SameSite != http.SameSiteStrictMode {
		t.Errorf("SameSite = %v, want Strict", found.SameSite)
	}
	if found.Path != "/" {
		t.Errorf("Path = %q, want /", found.Path)
	}
	if found.Secure {
		t.Error("the cookie is Secure on a plain-HTTP request, so a browser would drop it")
	}
}

// Behind a proxy that terminates TLS the request itself is plain HTTP, so the
// Secure attribute has to come from the proxy's view or the browser would drop
// the cookie it needs.
func TestTheStreamCookieIsSecureBehindAProxy(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{})
	session := newReviewSession(t, h, "payments", 42, "Fix the flap", "The change is sound.", store.ReviewSucceeded)
	handler, _ := sessionsHandler(t, h)

	response := moderatedRequestWith(t, handler, http.MethodGet, "/ui/sessions/"+session.ID, adminToken, nil, "text/html",
		map[string]string{"X-Forwarded-Proto": "https"})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == "rumil_admin" && !cookie.Secure {
			t.Error("the cookie is not Secure behind a TLS-terminating proxy")
		}
	}
}

// EventSource cannot set an Authorization header, so the stream has to accept
// the cookie the page set. The session is finished so the handler returns.
func TestTheLiveStreamAcceptsTheCookie(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{})
	session := newReviewSession(t, h, "payments", 42, "Fix the flap", "The change is sound.", store.ReviewSucceeded)

	now := time.Now().Add(-time.Minute)
	if err := h.db.AppendReviewEvents(context.Background(), session.ID, []store.NewReviewEvent{
		{Seq: 1, At: now, Type: "step_start", Payload: json.RawMessage(`{"step":1}`)},
		{Seq: 2, At: now.Add(time.Second), Type: "session_end", Payload: json.RawMessage(`{"status":"SUCCEEDED"}`)},
	}); err != nil {
		t.Fatalf("appending events: %v", err)
	}
	handler, _ := sessionsHandler(t, h)

	response := moderatedRequestWith(t, handler, http.MethodGet, "/api/sessions/"+session.ID+"/events", "", nil, "text/event-stream",
		map[string]string{"Cookie": "rumil_admin=" + adminCookie()})
	if response.Code != http.StatusOK {
		t.Fatalf("SSE with the cookie = %d, want 200 (body: %s)", response.Code, response.Body.String())
	}
	if body := response.Body.String(); !strings.Contains(body, `"type":"session_end"`) {
		t.Errorf("the stream did not replay the session:\n%s", body)
	}
}

// A cookie is a read credential. It reaches a state-changing route on its own
// only if a browser was tricked into sending it, so it must never authorize
// one, even from this site.
func TestACookieCannotMakeADecision(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{})
	session := newReviewSession(t, h, "payments", 42, "Fix the flap", "", store.ReviewFailed)
	handler, _ := sessionsHandler(t, h)

	response := moderatedRequestWith(t, handler, http.MethodPost, "/ui/sessions/"+session.ID+"/continue", "", nil, "text/html",
		map[string]string{
			"Cookie":         "rumil_admin=" + adminCookie(),
			"Sec-Fetch-Site": "same-origin",
		})
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("a cookie-only decision = %d, want 401 (body: %s)", response.Code, response.Body.String())
	}

	// The same request with the header is authorized and sets no cookie: a
	// write is not where the browser is handed a read credential.
	withHeader := moderatedRequestWith(t, handler, http.MethodPost, "/ui/sessions/"+session.ID+"/continue", adminToken, nil, "text/html",
		map[string]string{"Sec-Fetch-Site": "same-origin"})
	if withHeader.Code == http.StatusUnauthorized {
		t.Fatalf("a header-authorized decision = %d, want it to pass auth (body: %s)", withHeader.Code, withHeader.Body.String())
	}
	for _, cookie := range withHeader.Result().Cookies() {
		if cookie.Name == "rumil_admin" {
			t.Error("a state-changing request set the read cookie")
		}
	}
}

func TestAWrongCookieIsRefused(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{})
	session := newReviewSession(t, h, "payments", 42, "Fix the flap", "The change is sound.", store.ReviewSucceeded)
	handler, _ := sessionsHandler(t, h)

	response := moderatedRequestWith(t, handler, http.MethodGet, "/api/sessions/"+session.ID+"/events", "", nil, "text/event-stream",
		map[string]string{"Cookie": "rumil_admin=" + base64.RawURLEncoding.EncodeToString([]byte("not-the-token"))})
	if response.Code != http.StatusUnauthorized {
		t.Errorf("a wrong cookie = %d, want 401", response.Code)
	}
}
