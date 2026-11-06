package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/joangrigorov/rumil/internal/config"
	"github.com/joangrigorov/rumil/internal/store"
	"github.com/joangrigorov/rumil/internal/testutil"
)

// loginFlow is one sign-in: it reads the form, posts the credentials with the
// paired CSRF cookie, and returns the answer.
func loginFlow(t *testing.T, handler http.Handler, username, password string, remember bool, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	page := moderatedRequest(t, handler, http.MethodGet, "/ui/login", "", nil, "text/html")
	if page.Code != http.StatusOK {
		t.Fatalf("GET /ui/login = %d, want 200 (body: %s)", page.Code, page.Body.String())
	}
	csrf := ""
	for _, cookie := range page.Result().Cookies() {
		if cookie.Name == "rumil_login_csrf" {
			csrf = cookie.Value
		}
	}
	if csrf == "" {
		t.Fatal("the login page set no CSRF cookie")
	}

	form := url.Values{"username": {username}, "password": {password}, "csrf": {csrf}}
	if remember {
		form.Set("remember", "1")
	}
	requestHeaders := map[string]string{"Cookie": "rumil_login_csrf=" + csrf}
	for name, value := range headers {
		requestHeaders[name] = value
	}
	return moderatedRequestWith(t, handler, http.MethodPost, "/ui/login", "", form, "text/html", requestHeaders)
}

func sessionCookie(t *testing.T, response *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == "rumil_session" {
			return cookie
		}
	}
	return nil
}

// Signing in hands the browser a session cookie, and that cookie opens the
// pages without a header.
func TestSignInHandsTheBrowserASession(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{embedding: true})
	seedMemory(t, h, "Clock skew up to 60 seconds is intentional")
	handler := moderationHandler(t, h, &textEmbedder{}, adminToken)

	response := loginFlow(t, handler, adminUser, adminPassword, false, nil)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("sign-in status = %d, want 303 (body: %s)", response.Code, response.Body.String())
	}
	cookie := sessionCookie(t, response)
	if cookie == nil {
		t.Fatal("the answer set no rumil_session cookie")
	}
	if !cookie.HttpOnly {
		t.Error("the session cookie is readable from a script")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", cookie.SameSite)
	}
	if cookie.Path != "/" {
		t.Errorf("Path = %q, want /", cookie.Path)
	}
	if cookie.MaxAge != 0 || !cookie.Expires.IsZero() {
		t.Error("a sign-in without remember-me set a persistent cookie")
	}
	if cookie.Secure {
		t.Error("the cookie is Secure on a plain-HTTP request, so a browser would drop it")
	}

	page := moderatedRequestWith(t, handler, http.MethodGet, "/ui/moderation", "", nil, "text/html",
		map[string]string{"Cookie": "rumil_session=" + cookie.Value})
	if page.Code != http.StatusOK {
		t.Fatalf("the page with the session = %d, want 200 (body: %s)", page.Code, page.Body.String())
	}
	if !strings.Contains(page.Body.String(), "Clock skew up to 60 seconds") {
		t.Errorf("the page did not list the rule:\n%s", page.Body.String())
	}
	if !strings.Contains(page.Body.String(), ">legacy<") {
		t.Errorf("the page does not show the signed-in user:\n%s", page.Body.String())
	}
}

// Remember me sets a cookie that outlives the browser.
func TestRememberMeSetsAPersistentCookie(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{})
	handler := moderationHandler(t, h, nil, adminToken)

	response := loginFlow(t, handler, adminUser, adminPassword, true, nil)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("sign-in status = %d, want 303 (body: %s)", response.Code, response.Body.String())
	}
	cookie := sessionCookie(t, response)
	if cookie == nil {
		t.Fatal("the answer set no rumil_session cookie")
	}
	if cookie.MaxAge <= 29*24*60*60 || cookie.MaxAge > 31*24*60*60 {
		t.Errorf("MaxAge = %d, want about 30 days", cookie.MaxAge)
	}
	if cookie.Expires.IsZero() {
		t.Error("the remembered cookie has no expiry")
	}
}

// A wrong password and a form without the paired token both leave the browser
// without a session.
func TestSignInRefusesWhatItCannotVerify(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{})
	handler := moderationHandler(t, h, nil, adminToken)

	wrong := loginFlow(t, handler, adminUser, "wrong-password", false, nil)
	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("a wrong password = %d, want 401", wrong.Code)
	}
	if cookie := sessionCookie(t, wrong); cookie != nil {
		t.Error("a wrong password set a session")
	}

	request := httptest.NewRequest(http.MethodPost, "/ui/login",
		strings.NewReader(url.Values{"username": {adminUser}, "password": {adminPassword}}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "text/html")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Errorf("a form without the CSRF token = %d, want 400", recorder.Code)
	}
	if cookie := sessionCookie(t, recorder); cookie != nil {
		t.Error("a form without the CSRF token set a session")
	}
}

// A login posted by another site is refused even when the password is right.
func TestSignInRefusesACrossSiteForm(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{})
	handler := moderationHandler(t, h, nil, adminToken)

	response := loginFlow(t, handler, adminUser, adminPassword, false,
		map[string]string{"Sec-Fetch-Site": "cross-site"})
	if response.Code != http.StatusForbidden {
		t.Errorf("a cross-site login = %d, want 403", response.Code)
	}
	if cookie := sessionCookie(t, response); cookie != nil {
		t.Error("a cross-site login set a session")
	}
}

// Logging out clears the cookie.
func TestLogOutClearsTheSession(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{})
	handler := moderationHandler(t, h, nil, adminToken)

	login := loginFlow(t, handler, adminUser, adminPassword, false, nil)
	cookie := sessionCookie(t, login)
	if cookie == nil {
		t.Fatal("sign-in set no session")
	}

	logout := moderatedRequestWith(t, handler, http.MethodPost, "/ui/logout", "", nil, "text/html",
		map[string]string{"Cookie": "rumil_session=" + cookie.Value, "Sec-Fetch-Site": "same-origin"})
	if logout.Code != http.StatusSeeOther {
		t.Fatalf("logout = %d, want 303", logout.Code)
	}
	cleared := sessionCookie(t, logout)
	if cleared == nil || cleared.MaxAge >= 0 {
		t.Error("logout did not expire the session cookie")
	}
}

// EventSource cannot set an Authorization header, so the stream reads the same
// session cookie the pages use.
func TestTheLiveStreamAcceptsTheSessionCookie(t *testing.T) {
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
	login := loginFlow(t, handler, adminUser, adminPassword, false, nil)
	cookie := sessionCookie(t, login)
	if cookie == nil {
		t.Fatal("sign-in set no session")
	}

	response := moderatedRequestWith(t, handler, http.MethodGet, "/api/sessions/"+session.ID+"/events", "", nil, "text/event-stream",
		map[string]string{"Cookie": "rumil_session=" + cookie.Value})
	if response.Code != http.StatusOK {
		t.Fatalf("SSE with the session = %d, want 200 (body: %s)", response.Code, response.Body.String())
	}
	if body := response.Body.String(); !strings.Contains(body, `"type":"session_end"`) {
		t.Errorf("the stream did not replay the session:\n%s", body)
	}
}

// A changed password invalidates every session signed with the old one, so
// removing somebody from the list is immediate.
func TestASessionStopsWhenTheUserIsRemoved(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{})
	handler := moderationHandler(t, h, nil, adminToken)
	login := loginFlow(t, handler, adminUser, adminPassword, false, nil)
	cookie := sessionCookie(t, login)
	if cookie == nil {
		t.Fatal("sign-in set no session")
	}

	removed := moderationHandlerWith(t, h, &textEmbedder{}, []config.AdminUser{
		{Name: "somebody-else", PasswordHash: adminPasswordHash},
	})
	response := moderatedRequestWith(t, removed, http.MethodGet, "/ui/moderation", "", nil, "text/html",
		map[string]string{"Cookie": "rumil_session=" + cookie.Value})
	if response.Code != http.StatusSeeOther {
		t.Errorf("a session for a removed user = %d, want a redirect to login", response.Code)
	}
	if location := response.Header().Get("Location"); !strings.HasPrefix(location, "/ui/login") {
		t.Errorf("redirect = %q, want the login page", location)
	}
}

// A cookie is a session, not a bearer credential: a state-changing request
// still has to come from this deck.
func TestASessionCookieCannotMakeACrossSiteDecision(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{})
	session := newReviewSession(t, h, "payments", 42, "Fix the flap", "", store.ReviewFailed)
	handler, _ := sessionsHandler(t, h)
	login := loginFlow(t, handler, adminUser, adminPassword, false, nil)
	cookie := sessionCookie(t, login)
	if cookie == nil {
		t.Fatal("sign-in set no session")
	}

	response := moderatedRequestWith(t, handler, http.MethodPost, "/ui/sessions/"+session.ID+"/continue", "", nil, "text/html",
		map[string]string{
			"Cookie":         "rumil_session=" + cookie.Value,
			"Sec-Fetch-Site": "cross-site",
		})
	if response.Code != http.StatusForbidden {
		t.Fatalf("a cross-site decision = %d, want 403 (body: %s)", response.Code, response.Body.String())
	}

	sameSite := moderatedRequestWith(t, handler, http.MethodPost, "/ui/sessions/"+session.ID+"/continue", "", nil, "text/html",
		map[string]string{
			"Cookie":         "rumil_session=" + cookie.Value,
			"Sec-Fetch-Site": "same-origin",
		})
	if sameSite.Code == http.StatusUnauthorized || sameSite.Code == http.StatusForbidden {
		t.Fatalf("a same-origin decision = %d, want it to pass auth (body: %s)", sameSite.Code, sameSite.Body.String())
	}
}
