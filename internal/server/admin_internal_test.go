package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A session verifies while it is intact and stops when either its payload or
// its signature changes.
func TestSessionCookieRoundTripAndTamper(t *testing.T) {
	auth := NewAdminAuth(testCredentials(testUserPassword), nil)
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	recorder := httptest.NewRecorder()
	auth.setSession(recorder, request, "alice", true)

	cookie := recorder.Result().Cookies()
	if len(cookie) != 1 || cookie[0].Name != sessionCookieName {
		t.Fatalf("setSession wrote %v, want one %s", cookie, sessionCookieName)
	}

	withCookie := func(value string) (string, bool) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: value})
		return auth.sessionUser(r, testCredentials(testUserPassword))
	}
	if user, ok := withCookie(cookie[0].Value); !ok || user != "alice" {
		t.Fatalf("the session verified as %q/%v, want alice/true", user, ok)
	}

	tampered := []byte(cookie[0].Value)
	tampered[0] ^= 0x01
	if _, ok := withCookie(string(tampered)); ok {
		t.Error("a tampered signature still verified")
	}
}

// An expired claim is refused even with a valid signature.
func TestExpiredSessionIsRefused(t *testing.T) {
	users := testCredentials(testUserPassword)
	auth := NewAdminAuth(users, nil)
	value := auth.sealSession(sessionClaims{
		Version: sessionVersion,
		User:    "alice",
		Expires: time.Now().Add(-time.Minute).Unix(),
	}, users)

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: value})
	if _, ok := auth.sessionUser(request, users); ok {
		t.Error("an expired session verified")
	}
}

// A post-login redirect stays on this deck.
func TestNextPathStaysOnTheDeck(t *testing.T) {
	cases := map[string]string{
		"":                             "/ui/home",
		"/ui/sessions?status=RUNNING":  "/ui/sessions?status=RUNNING",
		"https://evil.example/ui/home": "/ui/home",
		"//evil.example/ui/home":       "/ui/home",
		"/etc/passwd":                  "/ui/home",
		"/ui/login":                    "/ui/login",
	}
	for input, want := range cases {
		if got := nextPath(input); got != want {
			t.Errorf("nextPath(%q) = %q, want %q", input, got, want)
		}
	}
}

// The login form's token has to match its cookie, and the cookie is scoped to
// the login route.
func TestLoginCSRFIsPaired(t *testing.T) {
	auth := NewAdminAuth(testCredentials(testUserPassword), nil)
	recorder := httptest.NewRecorder()
	auth.renderLogin(recorder, httptest.NewRequest(http.MethodGet, "/ui/login", nil),
		http.StatusOK, "", "", "/ui/home")

	cookie := recorder.Result().Cookies()
	if len(cookie) != 1 || cookie[0].Name != loginCSRFCookieName {
		t.Fatalf("renderLogin wrote %v, want one %s", cookie, loginCSRFCookieName)
	}

	request := httptest.NewRequest(http.MethodPost, "/ui/login",
		strings.NewReader("csrf="+cookie[0].Value))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(cookie[0])
	if err := request.ParseForm(); err != nil {
		t.Fatalf("ParseForm returned error: %v", err)
	}
	if !auth.validLoginCSRF(request) {
		t.Error("the paired token was refused")
	}

	request.PostForm.Set("csrf", "another-token")
	if auth.validLoginCSRF(request) {
		t.Error("a token that does not match the cookie was accepted")
	}
}
