package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/joangrigorov/rumil/internal/moderation"
	"github.com/joangrigorov/rumil/internal/store"
)

// Every refusal the surface can make has to reach the caller as a status they
// can act on: 500 for a rule that needs a moderator to look again says the
// surface is broken rather than the decision.
func TestStatusForMapsWhatTheSurfaceCanRefuse(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"an unknown rule", fmt.Errorf("%w: 9f1c", store.ErrMemoryNotFound), http.StatusNotFound},
		{"a shared prefix", fmt.Errorf("%w: 9f1c", store.ErrAmbiguousMemory), http.StatusConflict},
		{"a rule that moved", fmt.Errorf("%w: 9f1c", store.ErrMemoryChanged), http.StatusConflict},
		{"an edit without a provider", moderation.ErrNoEmbedder, http.StatusConflict},
		{"a status that is not one", fmt.Errorf("%w: %q", moderation.ErrUnknownStatus, "APPROVE"), http.StatusBadRequest},
		{"the database is gone", fmt.Errorf("listing memories: connection refused"), http.StatusInternalServerError},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := statusFor(tc.err); got != tc.want {
				t.Errorf("statusFor(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

// A browser attaches a cached basic credential to a request to this origin
// whichever page started it, so the decision has to be told apart by where the
// request came from.
func TestSameOriginReadsWhatTheBrowserSays(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    bool
	}{
		{"the page itself", map[string]string{"Sec-Fetch-Site": "same-origin"}, true},
		{"started by the browser", map[string]string{"Sec-Fetch-Site": "none"}, true},
		{"a form on another site", map[string]string{"Sec-Fetch-Site": "cross-site"}, false},
		{"a sibling subdomain", map[string]string{"Sec-Fetch-Site": "same-site"}, false},
		{"its own origin", map[string]string{"Origin": "http://example.com"}, true},
		{"another origin", map[string]string{"Origin": "https://evil.example"}, false},
		{"another port", map[string]string{"Origin": "http://example.com:8080"}, false},
		{"a malformed origin", map[string]string{"Origin": "://"}, false},
		{"an origin naming no host", map[string]string{"Origin": "null"}, false},
		{"no browser at all", nil, true},
		{"a script that sets its own headers", map[string]string{"User-Agent": "curl/8"}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "http://example.com/api/moderation/9f1c/reject", nil)
			for name, value := range tc.headers {
				request.Header.Set(name, value)
			}
			if got := sameOrigin(request); got != tc.want {
				t.Errorf("sameOrigin = %v, want %v", got, tc.want)
			}
		})
	}

	// An Origin that names no host must not agree with a request that carries no
	// Host either: that is how a `null` origin would otherwise get in.
	request := httptest.NewRequest(http.MethodPost, "/api/moderation/9f1c/reject", nil)
	request.Host = ""
	request.Header.Set("Origin", "null")
	if sameOrigin(request) {
		t.Error("an origin naming no host was accepted against an empty Host")
	}
}

// A repository reference means one thing on the page and in the CLI.
func TestSplitRepoRefTakesOwnerAndNameOnly(t *testing.T) {
	cases := []struct {
		ref   string
		org   string
		name  string
		valid bool
	}{
		{"acme/payments", "acme", "payments", true},
		{"acme/payments/extra", "", "", false},
		{"/payments", "", "", false},
		{"acme/", "", "", false},
		{"acme", "", "", false},
		{"", "", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.ref, func(t *testing.T) {
			org, name, err := splitRepoRef(tc.ref)
			if tc.valid {
				if err != nil {
					t.Fatalf("splitRepoRef(%q) returned error: %v", tc.ref, err)
				}
				if org != tc.org || name != tc.name {
					t.Errorf("splitRepoRef(%q) = %q, %q, want %q, %q", tc.ref, org, name, tc.org, tc.name)
				}
				return
			}
			if err == nil {
				t.Errorf("splitRepoRef(%q) was accepted as %q, %q", tc.ref, org, name)
			}
		})
	}
}

// A refusal is shaped like the success it stands in for, so a JSON client never
// has to parse a plain-text error to find out what happened.
func TestRefuseFollowsTheCallersShape(t *testing.T) {
	ui := NewModerationUI(nil, "s3cret-token", nil)

	jsonRequest := httptest.NewRequest(http.MethodPost, "/api/moderation/9f1c/reject", nil)
	jsonRecorder := httptest.NewRecorder()
	ui.refuse(jsonRecorder, jsonRequest, http.StatusConflict, "the rule changed")
	if jsonRecorder.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", jsonRecorder.Code)
	}
	if got := jsonRecorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want JSON for a client that did not ask for the page", got)
	}
	if body := jsonRecorder.Body.String(); !strings.Contains(body, `"error":"the rule changed"`) {
		t.Errorf("body = %q, want the reason under an error key", body)
	}

	pageRequest := httptest.NewRequest(http.MethodPost, "/api/moderation/9f1c/reject", nil)
	pageRequest.Header.Set("Accept", "text/html,application/xhtml+xml")
	pageRecorder := httptest.NewRecorder()
	ui.refuse(pageRecorder, pageRequest, http.StatusConflict, "the rule changed")
	if got := pageRecorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("Content-Type = %q, want text for a browser", got)
	}
	if !strings.Contains(pageRecorder.Body.String(), "the rule changed") {
		t.Errorf("the browser is not told why: %q", pageRecorder.Body.String())
	}
}

// An absent glob and an empty one are different decisions.
func TestPathGlobOfTellsAnAbsentFieldFromAnEmptyOne(t *testing.T) {
	form := func(values url.Values) *http.Request {
		request := httptest.NewRequest(http.MethodPost, "/api/moderation/9f1c/approve", strings.NewReader(values.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return request
	}

	absent := form(url.Values{"rule_text": {"a rule"}})
	if err := absent.ParseForm(); err != nil {
		t.Fatalf("ParseForm returned error: %v", err)
	}
	if glob := pathGlobOf(absent); glob != nil {
		t.Errorf("an absent path_glob read as %q, want no change", *glob)
	}

	empty := form(url.Values{"rule_text": {"a rule"}, "path_glob": {""}})
	if err := empty.ParseForm(); err != nil {
		t.Fatalf("ParseForm returned error: %v", err)
	}
	glob := pathGlobOf(empty)
	if glob == nil || *glob != "" {
		t.Errorf("an empty path_glob read as %v, want it widened", glob)
	}
}

// The credential is compared whole, and the scheme is the RFC's to define.
func TestAuthorizedAcceptsWhatTheRFCAllows(t *testing.T) {
	ui := NewModerationUI(nil, "s3cret-token", nil)
	cases := []struct {
		name   string
		header string
		want   bool
	}{
		{"a bearer token", "Bearer s3cret-token", true},
		{"a lowercase scheme", "bearer s3cret-token", true},
		{"another token", "Bearer nope", false},
		{"a prefix of the token", "Bearer s3cret", false},
		{"a token with a prefix", "Bearer xs3cret-token", false},
		{"the token with a trailing space", "Bearer s3cret-token ", false},
		{"no credential", "", false},
		{"a scheme with nothing after it", "Bearer", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/ui/moderation", nil)
			if tc.header != "" {
				request.Header.Set("Authorization", tc.header)
			}
			if got := ui.authorized(request); got != tc.want {
				t.Errorf("authorized(%q) = %v, want %v", tc.header, got, tc.want)
			}
		})
	}
}
